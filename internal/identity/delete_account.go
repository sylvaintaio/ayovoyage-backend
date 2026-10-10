package identity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/notify"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// DeleteCustomerAccount supprime le compte d'un client : coordonnées, proches,
// messages et noms des passagers sont effacés ; les réservations et paiements
// sont conservés anonymisés (obligations comptables). Refusé s'il reste un
// voyage à venir ou un remboursement en cours.
func (s *Service) DeleteCustomerAccount(ctx context.Context, customerID uuid.UUID) error {
	var c store.Customer
	err := s.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		if c, err = q.GetCustomer(ctx, customerID); err != nil {
			return err
		}
		open, err := q.CountCustomerOpenItems(ctx, &c.ID)
		if err != nil {
			return err
		}
		switch {
		case open.UpcomingTrips > 0:
			return huma.Error409Conflict(fmt.Sprintf("Vous avez %d voyage(s) à venir : effectuez-les ou annulez-les avant de supprimer votre compte.", open.UpcomingTrips))
		case open.OpenRefunds > 0:
			return huma.Error409Conflict("Un remboursement est en cours : votre compte pourra être supprimé une fois le remboursement versé.")
		}
		id := c.ID
		if err := q.AnonymizeCustomerSeats(ctx, &id); err != nil {
			return err
		}
		if err := q.AnonymizeCustomerBookings(ctx, &id); err != nil {
			return err
		}
		if err := q.DeleteCustomerPassengers(ctx, id); err != nil {
			return err
		}
		if err := q.DeleteCustomerNotifications(ctx, &id); err != nil {
			return err
		}
		if err := q.RevokeSubjectRefreshTokens(ctx, store.RevokeSubjectRefreshTokensParams{SubjectType: string(auth.SubjectCustomer), SubjectID: id}); err != nil {
			return err
		}
		if err := q.AnonymizeCustomer(ctx, id); err != nil {
			return err
		}
		return apix.Audit(ctx, q, nil, "customer.deleted", "customer", id.String(), nil, nil)
	})
	if err != nil || c.Email == nil || s.Email == nil {
		return err
	}
	// Confirmation à l'ancienne adresse (dernier message envoyé).
	go func(to, name string) {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Email.SendEmail(bg, notify.SecurityEmail(notify.BrandFromConfig(s.Cfg), to, name, "Votre compte a été supprimé",
			"Votre compte et vos données personnelles ont été supprimés à votre demande. Les justificatifs de paiement sont conservés de manière anonyme, comme l’exige la loi. Merci d’avoir voyagé avec nous."))
	}(*c.Email, c.FullName)
	return nil
}

func (h *Handler) registerDeleteAccount(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("delete-my-account", http.MethodDelete, "/v1/me", tagMe,
		"Supprimer mon compte", apix.Customer),
		"Irréversible. Envoyer `{\"confirm\": \"SUPPRIMER\"}`. Refusé (409) s'il reste un voyage à venir ou un remboursement en cours. "+
			"Les réservations passées sont conservées sans données personnelles (comptabilité)."),
		func(ctx context.Context, in *struct {
			Body struct {
				Confirm string `json:"confirm" enum:"SUPPRIMER" doc:"Confirmation explicite"`
			}
		}) (*SessionClearOutput, error) {
			if err := h.S.DeleteCustomerAccount(ctx, auth.MustFromContext(ctx).ID); err != nil {
				return nil, apix.Err(err)
			}
			return &SessionClearOutput{SetCookie: h.clearCookies()}, nil
		})
}

// SessionClearOutput : réponse vide qui efface les cookies de session.
type SessionClearOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
}

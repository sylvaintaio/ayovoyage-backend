package booking

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/phone"
)

func (s *Service) registerPayLater(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("get-pay-later-offer", http.MethodGet, "/v1/bookings/{id}/pay-later", tagBookings,
		"Payer en agence : l'option est-elle proposée ?", apix.Customer),
		"Indique si la réservation en attente peut être réglée en agence, jusqu'à quand, et dans quelles agences. "+
			"Sinon, `reason` explique pourquoi (départ trop proche, quota atteint…)."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body PayLaterOffer }, error) {
			o, err := s.PayLaterOfferFor(ctx, auth.MustFromContext(ctx), in.ID)
			return &struct{ Body PayLaterOffer }{Body: o}, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("choose-pay-later", http.MethodPost, "/v1/bookings/{id}/pay-later", tagBookings,
		"Payer en agence : bloquer les places jusqu'à la date limite", apix.Customer),
		"Les places restent bloquées jusqu'à `pay_by`. Le client reçoit la référence et la liste des agences ; un rappel est envoyé "+
			"avant l'échéance. Il peut aussi payer en ligne plus tard. Sans paiement à l'échéance, les places sont libérées."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body BookingDetail }, error) {
			d, err := s.ChoosePayLater(ctx, auth.MustFromContext(ctx), in.ID)
			return &struct{ Body BookingDetail }{Body: d}, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("collect-booking", http.MethodPost, "/v1/admin/bookings/{id}/collect", tagAdmin,
		"Encaisser une réservation en attente (guichet)", apix.Staff, auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleCashier),
		"Réservation « payer en agence » (ou en ligne non encore payée) : encaissement en espèces ou Mobile Money, "+
			"puis billets et facture émis comme pour une vente au guichet."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body CollectInput
		}) (*struct{ Body BookingDetail }, error) {
			d, err := s.Collect(ctx, auth.MustFromContext(ctx), in.ID, in.Body)
			return &struct{ Body BookingDetail }{Body: d}, apix.Err(err)
		})

	huma.Register(api, apix.Op("lift-pay-later-block", http.MethodDelete, "/v1/admin/pay-later-blocks/{phone}", tagAdmin,
		"Rendre le paiement en agence à un numéro mis à l'écart", apix.Staff, auth.RoleCompanyAdmin, auth.RoleAgencyManager),
		func(ctx context.Context, in *struct {
			Phone string `path:"phone" maxLength:"20"`
		}) (*struct{}, error) {
			number, err := phone.Normalize(in.Phone, s.Cfg.DefaultCountryCode)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("Numéro invalide")
			}
			_, err = s.LiftPayLaterBlock(ctx, auth.MustFromContext(ctx), number)
			return nil, apix.Err(err)
		})
}

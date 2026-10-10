package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Paiement en agence : une réservation en attente (créée avec un blocage court
// de quelques minutes) passe en « payer en agence » ; ses places restent
// bloquées jusqu'à une date limite. Garde-fous : nombre de places, réservations
// en attente par numéro, quota par départ, mise à l'écart après des oublis répétés.

// minPayLaterWindow : délai minimal laissé au client pour passer en agence.
const minPayLaterWindow = time.Hour

// PayLaterAgency : agence où régler la réservation.
type PayLaterAgency struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	City    string    `json:"city"`
	Address string    `json:"address,omitempty"`
}

// PayLaterOffer : disponibilité de l'option pour une réservation.
type PayLaterOffer struct {
	Enabled   bool             `json:"enabled" doc:"La compagnie propose le paiement en agence"`
	Available bool             `json:"available"`
	Reason    string           `json:"reason,omitempty" doc:"Motif si l'option n'est pas proposée"`
	PayBy     *time.Time       `json:"pay_by,omitempty" doc:"Date limite de paiement si l'option est choisie maintenant"`
	Amount    int64            `json:"amount"`
	Agencies  []PayLaterAgency `json:"agencies"`
}

// payLaterUnavailable : refus métier (affiché tel quel au client).
type payLaterUnavailable struct{ reason string }

func (e payLaterUnavailable) Error() string { return e.reason }

// payLaterDeadline calcule la date limite et vérifie les règles. Sans effet en base.
func payLaterDeadline(ctx context.Context, q *store.Queries, b store.Booking, now time.Time) (time.Time, store.Company, error) {
	co, err := q.GetCompany(ctx, b.CompanyID)
	if err != nil {
		return time.Time{}, co, err
	}
	switch {
	case !co.PayLaterEnabled:
		return time.Time{}, co, payLaterUnavailable{"Le paiement en agence n'est pas proposé par cette compagnie"}
	case b.Status != "pending" || b.ExpiresAt.Before(now):
		return time.Time{}, co, payLaterUnavailable{"Cette réservation n'est plus en attente de paiement"}
	case b.PaymentMode == "agency":
		return time.Time{}, co, payLaterUnavailable{"Cette réservation est déjà à payer en agence"}
	case b.PaymentMode != "online" || b.Channel == "counter":
		return time.Time{}, co, payLaterUnavailable{"Le paiement en agence n'est pas possible pour cette réservation"}
	case b.ChangedFromBookingID != nil:
		return time.Time{}, co, payLaterUnavailable{"Une modification de voyage se paie immédiatement"}
	case b.TotalAmount <= 0:
		return time.Time{}, co, payLaterUnavailable{"Rien à payer pour cette réservation"}
	}

	dep, err := q.GetDepartureDetail(ctx, b.DepartureID)
	if err != nil {
		return time.Time{}, co, err
	}
	payBy := now.Add(time.Duration(co.PayLaterHoldHours) * time.Hour)
	if limit := dep.DepartureAt.Add(-time.Duration(co.PayLaterCutoffMinutes) * time.Minute); limit.Before(payBy) {
		payBy = limit
	}
	if payBy.Sub(now) < minPayLaterWindow {
		return time.Time{}, co, payLaterUnavailable{"Départ trop proche pour payer en agence : réglez en ligne maintenant"}
	}

	seats, err := q.ListBookingSeats(ctx, b.ID)
	if err != nil {
		return time.Time{}, co, err
	}
	if len(seats) > int(co.PayLaterMaxSeats) {
		return time.Time{}, co, payLaterUnavailable{fmt.Sprintf("Le paiement en agence est limité à %d places par réservation", co.PayLaterMaxSeats)}
	}
	if blk, err := q.GetActivePayLaterBlock(ctx, store.GetActivePayLaterBlockParams{CompanyID: b.CompanyID, Phone: b.ContactPhone}); err == nil {
		return time.Time{}, co, payLaterUnavailable{fmt.Sprintf("Paiement en agence indisponible pour ce numéro jusqu'au %s (réservations non payées)",
			blk.BlockedUntil.In(localTZ).Format("02/01/2006"))}
	} else if !database.IsNotFound(err) {
		return time.Time{}, co, err
	}
	active, err := q.CountActivePayLater(ctx, store.CountActivePayLaterParams{CompanyID: b.CompanyID, Phone: b.ContactPhone})
	if err != nil {
		return time.Time{}, co, err
	}
	if active >= co.PayLaterMaxActive {
		return time.Time{}, co, payLaterUnavailable{fmt.Sprintf("Vous avez déjà %d réservation(s) à payer en agence : réglez-les d'abord", active)}
	}
	held, err := q.CountPayLaterSeatsOnDeparture(ctx, b.DepartureID)
	if err != nil {
		return time.Time{}, co, err
	}
	if quota := int(dep.SeatCount) * int(co.PayLaterQuotaPercent) / 100; int(held)+len(seats) > quota {
		return time.Time{}, co, payLaterUnavailable{"Plus de places réservables sans paiement sur ce départ : réglez en ligne pour garantir vos sièges"}
	}
	return payBy, co, nil
}

func listPayLaterAgencies(ctx context.Context, q *store.Queries, companyID uuid.UUID) ([]PayLaterAgency, error) {
	rows, err := q.ListPayLaterAgencies(ctx, companyID)
	if err != nil {
		return nil, err
	}
	out := make([]PayLaterAgency, 0, len(rows))
	for _, a := range rows {
		out = append(out, PayLaterAgency{ID: a.ID, Name: a.Name, City: a.City, Address: a.Address})
	}
	return out, nil
}

// PayLaterOfferFor : l'option est-elle proposée pour cette réservation du client ?
func (s *Service) PayLaterOfferFor(ctx context.Context, p auth.Principal, bookingID uuid.UUID) (PayLaterOffer, error) {
	var out PayLaterOffer
	err := s.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
		b, err := ownBooking(ctx, q, p, bookingID)
		if err != nil {
			return err
		}
		out.Amount = b.TotalAmount
		if out.Agencies, err = listPayLaterAgencies(ctx, q, b.CompanyID); err != nil {
			return err
		}
		payBy, co, err := payLaterDeadline(ctx, q, b, time.Now())
		out.Enabled = co.PayLaterEnabled
		if reason, ok := err.(payLaterUnavailable); ok {
			out.Reason = reason.reason
			return nil
		}
		if err != nil {
			return err
		}
		out.Available, out.PayBy = true, &payBy
		return nil
	})
	return out, err
}

// ChoosePayLater passe la réservation du client en « payer en agence ».
func (s *Service) ChoosePayLater(ctx context.Context, p auth.Principal, bookingID uuid.UUID) (BookingDetail, error) {
	var out BookingDetail
	err := s.DB.Tx(ctx, database.Customer(), func(tx pgx.Tx, q *store.Queries) error {
		b, err := ownBooking(ctx, q, p, bookingID)
		if err != nil {
			return err
		}
		if _, err := q.GetBookingForUpdate(ctx, b.ID); err != nil { // verrou : deux clics simultanés
			return err
		}
		if err := s.switchToPayLater(ctx, tx, q, b); err != nil {
			return err
		}
		out, err = LoadDetail(ctx, q, b.ID)
		return err
	})
	return out, err
}

// switchToPayLater applique l'option (utilisé aussi par le canal USSD).
func (s *Service) switchToPayLater(ctx context.Context, tx pgx.Tx, q *store.Queries, b store.Booking) error {
	payBy, _, err := payLaterDeadline(ctx, q, b, time.Now())
	if reason, ok := err.(payLaterUnavailable); ok {
		return huma.Error409Conflict(reason.reason)
	}
	if err != nil {
		return err
	}
	updated, err := q.SetBookingPayLater(ctx, store.SetBookingPayLaterParams{ID: b.ID, PayBy: &payBy})
	if database.IsNotFound(err) {
		return huma.Error409Conflict("Cette réservation n'est plus en attente de paiement")
	}
	if err != nil {
		return err
	}
	if err := q.ExtendBookingSeatHolds(ctx, store.ExtendBookingSeatHoldsParams{BookingID: b.ID, Until: payBy}); err != nil {
		return err
	}
	if err := s.Queue.InsertTx(ctx, tx, jobs.PayLaterNoticeArgs{BookingID: b.ID, Event: "reserved"}); err != nil {
		return err
	}
	return apix.Audit(ctx, q, &b.CompanyID, "booking.pay_later", "booking", b.ID.String(), nil,
		map[string]any{"reference": updated.Reference, "pay_by": payBy, "amount": updated.TotalAmount})
}

func ownBooking(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID) (store.Booking, error) {
	b, err := q.GetBooking(ctx, id)
	if err != nil || b.CustomerID == nil || *b.CustomerID != p.ID {
		return store.Booking{}, apix.NotFound("Réservation")
	}
	return b, nil
}

// CollectInput : encaissement au guichet d'une réservation en attente.
type CollectInput struct {
	Method    string     `json:"method" enum:"cash,mobile_money" doc:"Espèces, ou Mobile Money reçu sur le numéro de l'agence"`
	Reference string     `json:"reference,omitempty" maxLength:"60" doc:"Référence de la transaction Mobile Money (obligatoire pour mobile_money)"`
	AgencyID  *uuid.UUID `json:"agency_id,omitempty" doc:"Agence d'encaissement (personnel du siège)"`
}

// Collect : le caissier encaisse une réservation en attente (payer en agence,
// ou réservation en ligne non encore payée) ; billets et facture sont émis.
func (s *Service) Collect(ctx context.Context, p auth.Principal, bookingID uuid.UUID, in CollectInput) (BookingDetail, error) {
	agencyID := in.AgencyID
	if agencyID == nil {
		agencyID = p.AgencyID
	}
	if agencyID == nil {
		return BookingDetail{}, huma.Error422UnprocessableEntity("Agence d'encaissement requise (agency_id)")
	}
	if p.Role != auth.RoleCompanyAdmin && (p.AgencyID == nil || *p.AgencyID != *agencyID) {
		return BookingDetail{}, huma.Error403Forbidden("Vous ne pouvez encaisser que pour votre agence")
	}
	if in.Method == "mobile_money" && len(in.Reference) < 4 {
		return BookingDetail{}, huma.Error422UnprocessableEntity("Saisissez la référence de la transaction Mobile Money")
	}
	var out BookingDetail
	err := s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
		b, err := q.GetBookingForUpdate(ctx, bookingID)
		if err != nil || b.CompanyID != p.CompanyID {
			return apix.NotFound("Réservation")
		}
		if b.Status != "pending" {
			return huma.Error409Conflict("Cette réservation n'est pas en attente de paiement")
		}
		if b.ExpiresAt.Before(time.Now()) {
			return huma.Error409Conflict("Délai de paiement dépassé : les places ont été libérées, faites une nouvelle vente")
		}
		if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *agencyID, CompanyID: p.CompanyID}); err != nil {
			return huma.Error422UnprocessableEntity("Agence inconnue")
		}
		staffID := p.ID
		provider, ref := "cash", (*string)(nil)
		if in.Method == "mobile_money" {
			provider, ref = "cash", &in.Reference // encaissé par l'agence : rapproché via le relevé du numéro marchand
		}
		pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{
			CompanyID: b.CompanyID, BookingID: &b.ID, Provider: provider, Method: in.Method, Amount: b.TotalAmount,
			Currency: b.Currency, PayerPhone: b.ContactPhone, IdempotencyKey: "collect:" + b.ID.String(),
			AgencyID: agencyID, ReceivedByStaffID: &staffID, ProviderReference: ref,
		})
		if database.UniqueViolation(err) != "" {
			return huma.Error409Conflict("Cette réservation a déjà été encaissée")
		}
		if err != nil {
			return err
		}
		if _, err := q.MarkPaymentSucceeded(ctx, pay.ID); err != nil {
			return err
		}
		res, err := s.ConfirmPaid(ctx, tx, q, b.ID)
		if err != nil {
			return err
		}
		if !res.Confirmed {
			return huma.Error409Conflict("Les places ne sont plus disponibles : la réservation n'a pas pu être confirmée")
		}
		if err := apix.Audit(ctx, q, &p.CompanyID, "booking.collected", "booking", b.ID.String(), nil,
			map[string]any{"reference": b.Reference, "amount": b.TotalAmount, "method": in.Method, "agency_id": agencyID}); err != nil {
			return err
		}
		out, err = LoadDetail(ctx, q, b.ID)
		return err
	})
	return out, err
}

// LiftPayLaterBlock : un responsable rend l'option à un numéro mis à l'écart.
func (s *Service) LiftPayLaterBlock(ctx context.Context, p auth.Principal, phoneNumber string) (int, error) {
	n := 0
	err := s.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
		staffID := p.ID
		rows, err := q.LiftPayLaterBlocks(ctx, store.LiftPayLaterBlocksParams{CompanyID: p.CompanyID, Phone: phoneNumber, StaffID: &staffID})
		if err != nil {
			return err
		}
		n = len(rows)
		if n == 0 {
			return apix.NotFound("Mise à l'écart")
		}
		return apix.Audit(ctx, q, &p.CompanyID, "pay_later.block_lifted", "phone", phoneNumber, nil, map[string]any{"count": n})
	})
	return n, err
}

// recordPayLaterStrike : après une réservation « payer en agence » expirée,
// retire l'option au numéro s'il a trop d'oublis sur 30 jours.
func recordPayLaterStrike(ctx context.Context, q *store.Queries, companyID uuid.UUID, phoneNumber string) error {
	co, err := q.GetCompany(ctx, companyID)
	if err != nil {
		return err
	}
	n, err := q.CountExpiredPayLaterSince(ctx, store.CountExpiredPayLaterSinceParams{
		CompanyID: companyID, Phone: phoneNumber, Since: ptrTime(time.Now().AddDate(0, 0, -30)),
	})
	if err != nil || n < co.PayLaterStrikes {
		return err
	}
	if _, err := q.GetActivePayLaterBlock(ctx, store.GetActivePayLaterBlockParams{CompanyID: companyID, Phone: phoneNumber}); err == nil {
		return nil // déjà à l'écart
	} else if !database.IsNotFound(err) {
		return err
	}
	_, err = q.InsertPayLaterBlock(ctx, store.InsertPayLaterBlockParams{
		CompanyID: companyID, Phone: phoneNumber, BlockedUntil: time.Now().AddDate(0, 0, 30),
		Reason: fmt.Sprintf("%d réservation%s non payée%s en 30 jours", n, plural(n), plural(n)),
	})
	return err
}

func ptrTime(t time.Time) *time.Time { return &t }

func plural(n int32) string {
	if n > 1 {
		return "s"
	}
	return ""
}

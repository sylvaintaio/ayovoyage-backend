package booking

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Modification de billet = changement de départ.
//
// Une nouvelle réservation est créée sur le départ choisi (mêmes passagers),
// liée à l'ancienne. La valeur déjà payée est reportée en avoir ; le client
// règle la différence éventuelle et les frais. Tant que la nouvelle n'est pas
// payée, l'ancienne reste valable. Au paiement, l'ancienne passe au statut
// « changed » : sièges libérés, billets annulés, nouveaux billets émis.

type ChangeQuote struct {
	DepartureID      uuid.UUID `json:"departure_id"`
	Passengers       int32     `json:"passengers"`
	NewFare          int64     `json:"new_fare" doc:"Prix du nouveau trajet (remise fidélité déduite)"`
	Credit           int64     `json:"credit" doc:"Valeur reportée depuis la réservation actuelle (argent et points)"`
	Fee              int64     `json:"fee" doc:"Frais de modification"`
	FareDifference   int64     `json:"fare_difference" doc:"Supplément de tarif à payer"`
	AmountDue        int64     `json:"amount_due" doc:"Total à payer : supplément + frais"`
	NonRefundableGap int64     `json:"non_refundable_gap" doc:"Trajet moins cher : différence non remboursée"`
	Currency         string    `json:"currency"`
}

type ChangeCandidate struct {
	ID                    uuid.UUID `json:"id"`
	DepartureAt           time.Time `json:"departure_at"`
	ArrivalEta            time.Time `json:"arrival_eta"`
	BasePrice             int64     `json:"base_price"`
	OriginAgencyName      string    `json:"origin_agency_name"`
	DestinationAgencyName string    `json:"destination_agency_name"`
	BusModel              string    `json:"bus_model"`
	AvailableSeats        int32     `json:"available_seats"`
}

var errQuoteOnly = errors.New("devis : transaction annulée")

// abandonPendingChanges libère les sièges d'une modification non payée.
func (s *Service) abandonPendingChanges(ctx context.Context, q *store.Queries, bookingID uuid.UUID) error {
	ids, err := q.ExpirePendingChanges(ctx, &bookingID)
	if err != nil || len(ids) == 0 {
		return err
	}
	return q.ExpireSeatsOfBookings(ctx, ids)
}

// changeActor : qui modifie (client en ligne ou personnel au guichet).
type changeActor struct {
	principal auth.Principal
	channel   string
	staff     bool
	agencyID  *uuid.UUID // guichet : agence d'encaissement
	waiveFee  bool       // guichet : frais de modification offerts
}

func customerActor(p auth.Principal, channel string) changeActor {
	return changeActor{principal: p, channel: channel}
}

func (a changeActor) tenant() database.Tenant { return a.principal.Tenant() }

// load verrouille la réservation et vérifie qu'elle appartient à l'acteur.
func (a changeActor) load(ctx context.Context, q *store.Queries, id uuid.UUID) (store.Booking, error) {
	b, err := q.GetBookingForUpdate(ctx, id)
	if database.IsNotFound(err) {
		return b, apix.NotFound("Réservation")
	}
	if err != nil {
		return b, err
	}
	if a.staff {
		if b.CompanyID != a.principal.CompanyID {
			return b, apix.NotFound("Réservation")
		}
	} else if b.CustomerID == nil || *b.CustomerID != a.principal.ID {
		return b, apix.NotFound("Réservation")
	}
	return b, nil
}

func (s *Service) actorPolicy(ctx context.Context, q *store.Queries, a changeActor, b store.Booking) (BookingPolicy, error) {
	if a.staff {
		return s.StaffPolicy(ctx, q, b, time.Now())
	}
	return s.Policy(ctx, q, b, time.Now())
}

// ChangeCandidates liste les départs vers lesquels la réservation peut être déplacée.
func (s *Service) ChangeCandidates(ctx context.Context, p auth.Principal, bookingID uuid.UUID, day time.Time) ([]ChangeCandidate, error) {
	return s.changeCandidates(ctx, customerActor(p, ""), bookingID, day)
}

func (s *Service) changeCandidates(ctx context.Context, a changeActor, bookingID uuid.UUID, day time.Time) ([]ChangeCandidate, error) {
	out := []ChangeCandidate{}
	err := s.DB.Tx(ctx, a.tenant(), func(_ pgx.Tx, q *store.Queries) error {
		b, err := a.load(ctx, q, bookingID)
		if err != nil {
			return err
		}
		pol, err := s.actorPolicy(ctx, q, a, b)
		if err != nil {
			return err
		}
		if !pol.Change.Allowed {
			return huma.Error409Conflict(pol.Change.Reason)
		}
		cur, err := q.GetDepartureCities(ctx, b.DepartureID)
		if err != nil {
			return err
		}
		from, to := day, day.AddDate(0, 0, 1)
		if to.After(*pol.Change.WindowEnd) {
			to = *pol.Change.WindowEnd
		}
		rows, err := q.ListChangeCandidates(ctx, store.ListChangeCandidatesParams{
			CompanyID: b.CompanyID, OriginCityID: cur.OriginCityID, DestinationCityID: cur.DestinationCityID,
			ExcludeDepartureID: b.DepartureID, FromTs: from, ToTs: to, Passengers: pol.Change.Passengers,
		})
		for _, r := range rows {
			out = append(out, ChangeCandidate{ID: r.ID, DepartureAt: r.DepartureAt, ArrivalEta: r.ArrivalEta, BasePrice: r.BasePrice,
				OriginAgencyName: r.OriginAgencyName, DestinationAgencyName: r.DestinationAgencyName, BusModel: r.BusModel,
				AvailableSeats: r.AvailableSeats})
		}
		return err
	})
	return out, err
}

// QuoteChange calcule ce que coûterait le changement, sans rien réserver :
// la modification est simulée puis la transaction annulée.
func (s *Service) QuoteChange(ctx context.Context, p auth.Principal, channel string, bookingID, departureID uuid.UUID) (ChangeQuote, error) {
	return s.quoteChange(ctx, customerActor(p, channel), bookingID, departureID)
}

func (s *Service) quoteChange(ctx context.Context, a changeActor, bookingID, departureID uuid.UUID) (ChangeQuote, error) {
	var quote ChangeQuote
	err := s.DB.Tx(ctx, a.tenant(), func(tx pgx.Tx, q *store.Queries) error {
		b, err := a.load(ctx, q, bookingID)
		if err != nil {
			return err
		}
		if _, quote, err = s.change(ctx, tx, q, a, b, departureID); err != nil {
			return err
		}
		return errQuoteOnly
	})
	if errors.Is(err, errQuoteOnly) {
		err = nil
	}
	return quote, err
}

// Change crée la nouvelle réservation. Si rien n'est à payer, elle est
// confirmée immédiatement ; sinon le client passe au paiement.
func (s *Service) Change(ctx context.Context, p auth.Principal, channel string, bookingID, departureID uuid.UUID) (BookingDetail, error) {
	return s.applyChange(ctx, customerActor(p, channel), bookingID, departureID)
}

func (s *Service) applyChange(ctx context.Context, a changeActor, bookingID, departureID uuid.UUID) (BookingDetail, error) {
	var out BookingDetail
	err := s.DB.Tx(ctx, a.tenant(), func(tx pgx.Tx, q *store.Queries) error {
		b, err := a.load(ctx, q, bookingID)
		if err != nil {
			return err
		}
		nb, quote, err := s.change(ctx, tx, q, a, b, departureID)
		if err != nil {
			return err
		}
		if err := apix.Audit(ctx, q, &b.CompanyID, "booking.change_requested", "booking", b.ID.String(), nil,
			map[string]any{"new_booking": nb.Reference, "departure_id": departureID, "amount_due": quote.AmountDue,
				"fee": quote.Fee, "at_counter": a.staff, "fee_waived": a.waiveFee}); err != nil {
			return err
		}
		if a.staff && nb.TotalAmount > 0 { // guichet : encaissement en espèces immédiat
			staffID := a.principal.ID
			pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{
				CompanyID: nb.CompanyID, BookingID: &nb.ID, Provider: "cash", Method: "cash", Amount: nb.TotalAmount,
				Currency: nb.Currency, PayerPhone: nb.ContactPhone, IdempotencyKey: "cash:" + nb.ID.String(),
				AgencyID: a.agencyID, ReceivedByStaffID: &staffID,
			})
			if err != nil {
				return err
			}
			if _, err := q.MarkPaymentSucceeded(ctx, pay.ID); err != nil {
				return err
			}
		}
		if a.staff || nb.TotalAmount == 0 {
			if _, err := s.ConfirmPaid(ctx, tx, q, nb.ID); err != nil {
				return err
			}
		}
		out, err = LoadDetail(ctx, q, nb.ID)
		return err
	})
	return out, err
}

func (s *Service) change(ctx context.Context, tx pgx.Tx, q *store.Queries, a changeActor, b store.Booking, departureID uuid.UUID) (store.Booking, ChangeQuote, error) {
	quote := ChangeQuote{DepartureID: departureID, Currency: b.Currency}
	pol, err := s.actorPolicy(ctx, q, a, b)
	if err != nil {
		return store.Booking{}, quote, err
	}
	if !pol.Change.Allowed {
		return store.Booking{}, quote, huma.Error409Conflict(pol.Change.Reason)
	}
	cur, err := q.GetDepartureCities(ctx, b.DepartureID)
	if err != nil {
		return store.Booking{}, quote, err
	}
	nd, err := q.GetDepartureCities(ctx, departureID)
	if database.IsNotFound(err) {
		return store.Booking{}, quote, apix.NotFound("Départ")
	}
	if err != nil {
		return store.Booking{}, quote, err
	}
	switch {
	case nd.ID == cur.ID:
		return store.Booking{}, quote, huma.Error422UnprocessableEntity("Choisissez un départ différent de l'actuel")
	case nd.CompanyID != b.CompanyID || nd.OriginCityID != cur.OriginCityID || nd.DestinationCityID != cur.DestinationCityID:
		return store.Booking{}, quote, huma.Error422UnprocessableEntity("Le nouveau départ doit être sur le même trajet et avec la même compagnie")
	case nd.DepartureAt.After(*pol.Change.WindowEnd):
		return store.Booking{}, quote, huma.Error422UnprocessableEntity("Le nouveau départ doit avoir lieu au plus tard le " + pol.Change.WindowEnd.In(localTZ).Format("02/01/2006"))
	}

	// Une seule modification en cours : la précédente demande non payée est abandonnée.
	if err := s.abandonPendingChanges(ctx, q, b.ID); err != nil {
		return store.Booking{}, quote, err
	}

	seats, err := q.ListBookingSeats(ctx, b.ID)
	if err != nil {
		return store.Booking{}, quote, err
	}
	in := CreateInput{DepartureID: departureID, ContactName: b.ContactName, ContactPhone: b.ContactPhone}
	if b.BillingName != nil {
		in.BillingName = *b.BillingName
	}
	if b.BillingTaxID != nil {
		in.BillingTaxID = *b.BillingTaxID
	}
	if b.ContactEmail != nil {
		in.ContactEmail = *b.ContactEmail
	}
	category := ""
	for _, st := range seats {
		if st.Status != "sold" {
			continue
		}
		in.Passengers = append(in.Passengers, PassengerInput{Name: st.PassengerName, Phone: st.PassengerPhone})
		if len(in.Passengers) == 1 || category == st.CategoryCode {
			category = st.CategoryCode
		} else {
			category = "" // catégories mélangées : meilleurs sièges disponibles
		}
	}
	in.Preferences = &SeatPreferences{CategoryCode: category, Together: len(in.Passengers) > 1}

	cc := createContext{channel: a.channel, customerID: b.CustomerID}
	if a.staff {
		staffID := a.principal.ID
		cc = createContext{channel: "counter", customerID: b.CustomerID, agencyID: a.agencyID, staffID: &staffID, companyID: &a.principal.CompanyID}
	}
	nb, err := s.create(ctx, tx, q, cc, in)
	if err != nil {
		return store.Booking{}, quote, err
	}

	fare := nb.SubtotalAmount - nb.DiscountAmount
	credit := paidValue(b) + b.LoyaltyPointsAmount
	fee := pol.Change.FeeTotal
	if a.waiveFee {
		fee = 0
	}
	quote.Passengers = int32(len(in.Passengers))
	quote.NewFare, quote.Credit, quote.Fee = fare, credit, fee
	quote.FareDifference = max(0, fare-credit)
	quote.NonRefundableGap = max(0, credit-fare)
	quote.AmountDue = quote.FareDifference + fee

	nb, err = q.SetBookingChange(ctx, store.SetBookingChangeParams{
		ID: nb.ID, ChangedFromBookingID: &b.ID, ChangeCount: b.ChangeCount + 1,
		ChangeFeeAmount: fee, ChangeCreditAmount: paidValue(b), TotalAmount: quote.AmountDue,
	})
	if database.UniqueViolation(err) != "" {
		return store.Booking{}, quote, huma.Error409Conflict("Une modification est déjà en cours pour cette réservation")
	}
	return nb, quote, err
}

// completeChange : appelée à la confirmation d'une réservation issue d'une
// modification. Retourne false si l'ancienne réservation n'est plus valable
// (annulée ou passager embarqué entre-temps) : le paiement est alors remboursé.
func (s *Service) completeChange(ctx context.Context, q *store.Queries, nb store.Booking) (bool, error) {
	old, err := q.GetBookingForUpdate(ctx, *nb.ChangedFromBookingID)
	if err != nil {
		return false, err
	}
	used, err := q.CountUsedTicketsForBooking(ctx, old.ID)
	if err != nil {
		return false, err
	}
	if old.Status != "confirmed" || used > 0 {
		return false, nil
	}
	if _, err := q.SetBookingStatus(ctx, store.SetBookingStatusParams{ID: old.ID, Status: "changed",
		CancelReason: "Remplacée par la réservation " + nb.Reference}); err != nil {
		return false, err
	}
	if err := q.ReleaseBookingSeats(ctx, store.ReleaseBookingSeatsParams{BookingID: old.ID, Status: "cancelled"}); err != nil {
		return false, err
	}
	if err := q.CancelBookingTickets(ctx, old.ID); err != nil {
		return false, err
	}
	// Les points utilisés suivent la nouvelle réservation (dans la limite de son prix).
	if old.LoyaltyPointsUsed > 0 {
		if err := q.TransferLoyaltyPoints(ctx, store.TransferLoyaltyPointsParams{ID: nb.ID, PointsUsed: old.LoyaltyPointsUsed,
			PointsAmount: min(old.LoyaltyPointsAmount, nb.SubtotalAmount-nb.DiscountAmount)}); err != nil {
			return false, err
		}
	}
	return true, apix.Audit(ctx, q, &old.CompanyID, "booking.changed", "booking", old.ID.String(), nil,
		map[string]any{"new_booking": nb.Reference, "new_departure_id": nb.DepartureID, "fee": nb.ChangeFeeAmount})
}

var localTZ = time.FixedZone("WAT", 3600)

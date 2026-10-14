package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// OnboardPassenger : voyageur pris à bord. Siège : obligatoire hors connexion (place du quota),
// sinon attribué automatiquement.
type OnboardPassenger struct {
	Name   string     `json:"name,omitempty" maxLength:"120" doc:"Absent : « Voyageur à bord »"`
	SeatID *uuid.UUID `json:"seat_id,omitempty"`
}

// OnboardSaleInput : vente à bord, en direct ou envoyée après une coupure de réseau.
type OnboardSaleInput struct {
	ClientSaleID string             `json:"client_sale_id" minLength:"1" maxLength:"64" doc:"Identifiant unique côté téléphone : renvoyer la vente est sans effet"`
	BoardingSeq  *int               `json:"boarding_seq,omitempty" minimum:"0" doc:"Absent : arrêt où se trouve le car (ou dernier arrêt quitté)"`
	AlightingSeq int                `json:"alighting_seq" minimum:"1"`
	Passengers   []OnboardPassenger `json:"passengers" minItems:"1" maxItems:"10"`
	ContactName  string             `json:"contact_name,omitempty" maxLength:"120"`
	ContactPhone string             `json:"contact_phone,omitempty" maxLength:"20" doc:"Facultatif : billet par SMS et points de fidélité"`
	SoldAt       time.Time          `json:"sold_at" doc:"Heure réelle de la vente (plafonnée à l'heure du serveur)"`
	Offline      bool               `json:"offline,omitempty" doc:"Vente faite sans réseau, sur les places du quota"`
	Collected    int64              `json:"collected,omitempty" minimum:"0" doc:"Hors connexion : montant encaissé par le contrôleur"`
	Payment      string             `json:"payment,omitempty" enum:"cash,online" default:"cash" doc:"online : Mobile Money du voyageur (FedaPay), avec réseau ; le contrôleur lance ensuite le paiement"`
}

// OnboardSaleResult : issue d'une vente à bord.
type OnboardSaleResult struct {
	ClientSaleID string         `json:"client_sale_id"`
	Result       string         `json:"result" enum:"recorded,duplicate,issue,awaiting_payment"`
	Message      string         `json:"message"`
	Booking      *BookingDetail `json:"booking,omitempty"`
	// Hors connexion : le serveur a dû changer de siège, ou le prix diffère de celui encaissé.
	SeatChanged bool  `json:"seat_changed,omitempty"`
	PriceGap    int64 `json:"price_gap,omitempty" doc:"Montant encaissé - prix calculé par le serveur"`
}

// OnboardSeller : contrôleur qui vend (terminal compris).
type OnboardSeller struct {
	Principal auth.Principal
	DeviceID  *uuid.UUID
}

// SellOnboard enregistre une vente à bord dans la transaction de l'appelant : réservation confirmée,
// espèces encaissées par le contrôleur, billets émis et embarqués aussitôt (le voyageur est à bord).
// Hors connexion, si le siège a été pris entre-temps, un autre est attribué ; s'il n'y en a plus,
// la vente est enregistrée en anomalie (argent encaissé, voyageur à bord).
func (s *Service) SellOnboard(ctx context.Context, tx pgx.Tx, q *store.Queries, seller OnboardSeller, d store.Departure,
	in OnboardSaleInput) (OnboardSaleResult, error) {
	p := seller.Principal
	res := OnboardSaleResult{ClientSaleID: in.ClientSaleID}

	// Renvoi après une coupure : la vente est déjà enregistrée.
	if b, err := q.GetBookingByClientSale(ctx, store.GetBookingByClientSaleParams{StaffID: &p.ID, ClientSaleID: &in.ClientSaleID}); err == nil {
		detail, err := LoadDetail(ctx, q, b.ID)
		if err != nil {
			return res, err
		}
		res.Result, res.Message, res.Booking = "duplicate", "Vente déjà enregistrée", &detail
		return res, nil
	} else if !database.IsNotFound(err) {
		return res, err
	}
	if issue, err := q.GetOnboardSaleIssue(ctx, store.GetOnboardSaleIssueParams{StaffID: p.ID, ClientSaleID: in.ClientSaleID}); err == nil {
		res.Result, res.Message = "issue", issue.Reason
		return res, nil
	} else if !database.IsNotFound(err) {
		return res, err
	}

	soldAt := in.SoldAt
	if soldAt.IsZero() || soldAt.After(time.Now()) {
		soldAt = time.Now()
	}
	boarding := in.BoardingSeq
	if boarding == nil {
		seq, err := busPosition(ctx, q, d.ID)
		if err != nil {
			return res, err
		}
		boarding = &seq
	}
	if in.AlightingSeq <= *boarding {
		return res, huma.Error422UnprocessableEntity("La descente doit être après l'arrêt de montée")
	}

	create := CreateInput{DepartureID: d.ID, BoardingSeq: boarding, AlightingSeq: &in.AlightingSeq,
		ContactName: strings.TrimSpace(in.ContactName), ContactPhone: strings.TrimSpace(in.ContactPhone)}
	for i, ps := range in.Passengers {
		name := strings.TrimSpace(ps.Name)
		if len([]rune(name)) < 2 {
			name = "Voyageur à bord"
			if len(in.Passengers) > 1 {
				name = fmt.Sprintf("Voyageur à bord %d", i+1)
			}
		}
		create.Passengers = append(create.Passengers, PassengerInput{Name: name, SeatID: ps.SeatID})
	}
	if len([]rune(create.ContactName)) < 2 {
		create.ContactName = create.Passengers[0].Name
	}

	staffID := p.ID
	cc := createContext{channel: "onboard", staffID: &staffID, companyID: &p.CompanyID}
	// Siège non choisi : la catégorie la moins chère (prix annoncé au voyageur), sinon toute place libre.
	cheapest, err := cheapestCategory(ctx, q, d.CompanyID)
	if err != nil {
		return res, err
	}
	if cheapest != "" {
		create.Preferences = &SeatPreferences{CategoryCode: cheapest}
	}
	b, err := s.createInSavepoint(ctx, tx, q, cc, create)
	if conflict(err) && create.Preferences != nil {
		create.Preferences = nil
		b, err = s.createInSavepoint(ctx, tx, q, cc, create)
	}
	if conflict(err) && in.Offline {
		// Siège du quota pris entre-temps : n'importe quelle place libre sur le tronçon.
		for i := range create.Passengers {
			create.Passengers[i].SeatID = nil
		}
		res.SeatChanged = true
		b, err = s.createInSavepoint(ctx, tx, q, cc, create)
	}
	if conflict(err) && in.Offline {
		payload, _ := json.Marshal(in)
		reason := "Plus aucun siège libre sur ce tronçon : vente faite sans réseau, à régulariser"
		if _, err := q.InsertOnboardSaleIssue(ctx, store.InsertOnboardSaleIssueParams{CompanyID: d.CompanyID, DepartureID: d.ID,
			StaffID: p.ID, ClientSaleID: in.ClientSaleID, Reason: reason, Amount: in.Collected, Payload: payload, SoldAt: soldAt}); err != nil {
			return res, err
		}
		if err := apix.Audit(ctx, q, &p.CompanyID, "booking.onboard_issue", "departure", d.ID.String(), nil,
			map[string]any{"client_sale_id": in.ClientSaleID, "amount": in.Collected}); err != nil {
			return res, err
		}
		res.Result, res.Message, res.SeatChanged = "issue", reason, false
		return res, nil
	}
	if err != nil {
		return res, err
	}

	// Mobile Money du voyageur : réservation gardée le temps du paiement, billets émis à la confirmation.
	if in.Payment == "online" {
		if in.Offline {
			return res, huma.Error422UnprocessableEntity("Le paiement Mobile Money demande du réseau : encaissez en espèces")
		}
		if err := q.SetBookingClientSale(ctx, store.SetBookingClientSaleParams{ID: b.ID, ClientSaleID: &in.ClientSaleID}); err != nil {
			return res, err
		}
		detail, err := LoadDetail(ctx, q, b.ID)
		if err != nil {
			return res, err
		}
		res.Result, res.Message, res.Booking = "awaiting_payment", "En attente du paiement Mobile Money", &detail
		return res, nil
	}

	if b.TotalAmount > 0 {
		pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{
			CompanyID: b.CompanyID, BookingID: &b.ID, Provider: "cash", Method: "cash", Amount: b.TotalAmount,
			Currency: b.Currency, PayerPhone: b.ContactPhone, IdempotencyKey: "onboard:" + b.ID.String(),
			ReceivedByStaffID: &staffID,
		})
		if err != nil {
			return res, err
		}
		if _, err := q.MarkPaymentSucceeded(ctx, pay.ID); err != nil {
			return res, err
		}
	}
	if _, err := s.ConfirmPaid(ctx, tx, q, b.ID); err != nil {
		return res, err
	}
	if err := q.SetBookingClientSale(ctx, store.SetBookingClientSaleParams{ID: b.ID, ClientSaleID: &in.ClientSaleID, SoldOffline: in.Offline}); err != nil {
		return res, err
	}
	detail, err := LoadDetail(ctx, q, b.ID)
	if err != nil {
		return res, err
	}
	// Le voyageur est déjà dans le car : billets embarqués à l'heure de la vente.
	stopSeq := int32(*boarding)
	for _, t := range detail.Tickets {
		if t.Status == "cancelled" {
			continue
		}
		if err := q.MarkTicketUsed(ctx, store.MarkTicketUsedParams{ID: t.ID, BoardedAt: &soldAt, BoardedBy: &staffID}); err != nil {
			return res, err
		}
		ticketID := t.ID
		if _, err := q.InsertBoardingScan(ctx, store.InsertBoardingScanParams{CompanyID: b.CompanyID, DepartureID: d.ID,
			TicketID: &ticketID, StaffID: staffID, DeviceID: seller.DeviceID, Result: "boarded", Offline: in.Offline,
			ScannedAt: soldAt, StopSeq: &stopSeq}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return res, err
		}
	}
	if err := apix.Audit(ctx, q, &p.CompanyID, "booking.onboard_sale", "booking", b.ID.String(), nil,
		map[string]any{"reference": b.Reference, "amount": b.TotalAmount, "offline": in.Offline, "collected": in.Collected,
			"boarding_seq": *boarding, "alighting_seq": in.AlightingSeq}); err != nil {
		return res, err
	}
	if detail, err = LoadDetail(ctx, q, b.ID); err != nil {
		return res, err
	}
	res.Result, res.Message, res.Booking = "recorded", "Vente enregistrée", &detail
	if in.Offline {
		res.PriceGap = in.Collected - b.TotalAmount
	}
	return res, nil
}

// createInSavepoint : un échec (siège pris) n'annule pas la transaction de l'appelant (envoi groupé).
func (s *Service) createInSavepoint(ctx context.Context, tx pgx.Tx, q *store.Queries, cc createContext, in CreateInput) (store.Booking, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return store.Booking{}, err
	}
	b, err := s.create(ctx, sp, q.WithTx(sp), cc, in)
	if err != nil {
		_ = sp.Rollback(ctx)
		return b, err
	}
	return b, sp.Commit(ctx)
}

func conflict(err error) bool {
	var se huma.StatusError
	return errors.As(err, &se) && se.GetStatus() == 409
}

func cheapestCategory(ctx context.Context, q *store.Queries, companyID uuid.UUID) (string, error) {
	cats, err := q.ListSeatCategories(ctx, companyID)
	if err != nil || len(cats) == 0 {
		return "", err
	}
	best := cats[0]
	for _, c := range cats[1:] {
		if c.SurchargeAmount < best.SurchargeAmount {
			best = c
		}
	}
	return best.Code, nil
}

// busPosition : arrêt où se trouve le car (arrêt ouvert), sinon dernier arrêt quitté, sinon le premier.
func busPosition(ctx context.Context, q *store.Queries, departureID uuid.UUID) (int, error) {
	visits, err := q.ListStopVisits(ctx, departureID)
	if err != nil {
		return 0, err
	}
	pos := 0
	for _, v := range visits {
		if v.Seq != nil && int(*v.Seq) > pos {
			pos = int(*v.Seq)
		}
	}
	return pos, nil
}

// Package booking gère le cycle de vie des réservations :
// blocage temporaire des sièges, calcul du prix, confirmation après paiement,
// annulation, expiration.
//
// Garantie anti-double-vente (3 niveaux) :
//  1. verrou de ligne sur le départ pendant l'attribution (sérialisation) ;
//  2. contrôle de l'état du siège dans la même transaction ;
//  3. index unique partiel (departure_id, seat_cell_id) WHERE status IN ('held','sold').
package booking

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/invoice"
	"busvoyage/internal/jobs"
	"busvoyage/internal/loyalty"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/pricing"
	"busvoyage/internal/store"
	"busvoyage/internal/ticket"
)

const maxPassengers = 10

type Service struct {
	DB      *database.DB
	Cfg     config.Config
	Tickets *ticket.Service
	Queue   *jobs.Queue
}

type PassengerInput struct {
	Name   string     `json:"name" minLength:"2" maxLength:"120"`
	Phone  string     `json:"phone,omitempty" maxLength:"20"`
	SeatID *uuid.UUID `json:"seat_id,omitempty" doc:"Siège choisi sur le plan. Absent : attribution automatique du meilleur siège disponible"`
}

type CreateInput struct {
	DepartureID  uuid.UUID        `json:"departure_id"`
	Passengers   []PassengerInput `json:"passengers" minItems:"1" maxItems:"10"`
	Preferences  *SeatPreferences `json:"preferences,omitempty"`
	ContactName  string           `json:"contact_name" minLength:"2" maxLength:"120"`
	ContactPhone string           `json:"contact_phone" minLength:"8" maxLength:"20"`
	ContactEmail string           `json:"contact_email,omitempty" format:"email" maxLength:"254"`
	RedeemPoints int32            `json:"redeem_points,omitempty" minimum:"0" doc:"Points de fidélité à utiliser (clients connectés uniquement)"`
	BillingName  string           `json:"billing_name,omitempty" maxLength:"160" doc:"Facture au nom d'une entreprise : raison sociale"`
	BillingTaxID string           `json:"billing_tax_id,omitempty" maxLength:"30" doc:"IFU de l'entreprise"`
	PromoCode    string           `json:"promo_code,omitempty" maxLength:"30" doc:"Code promo (refus motivé si non valable)"`
}

// createContext : informations dérivées de l'appelant (jamais du corps de la requête).
type createContext struct {
	channel    string
	customerID *uuid.UUID
	agencyID   *uuid.UUID
	staffID    *uuid.UUID
	tenant     database.Tenant
	companyID  *uuid.UUID // personnel : la réservation doit concerner sa compagnie
}

// Create bloque les sièges et crée une réservation en attente de paiement.
func (s *Service) create(ctx context.Context, tx pgx.Tx, q *store.Queries, cc createContext, in CreateInput) (store.Booking, error) {
	if len(in.Passengers) == 0 || len(in.Passengers) > maxPassengers {
		return store.Booking{}, huma.Error422UnprocessableEntity(fmt.Sprintf("Entre 1 et %d passagers par réservation", maxPassengers))
	}
	contactPhone, err := phone.Normalize(in.ContactPhone, s.Cfg.DefaultCountryCode)
	if err != nil {
		return store.Booking{}, huma.Error422UnprocessableEntity("Téléphone de contact invalide")
	}

	dep, err := q.LockDepartureForBooking(ctx, in.DepartureID)
	if database.IsNotFound(err) {
		return store.Booking{}, apix.NotFound("Départ")
	}
	if err != nil {
		return store.Booking{}, err
	}
	if cc.companyID != nil && dep.CompanyID != *cc.companyID {
		return store.Booking{}, apix.NotFound("Départ")
	}
	if dep.Status != "scheduled" && dep.Status != "boarding" {
		return store.Booking{}, huma.Error409Conflict("Ce départ n'est plus ouvert à la vente")
	}
	cutoff := time.Duration(dep.SalesCutoffMinutes) * time.Minute
	if cc.channel == "counter" {
		cutoff = 0 // le guichet peut vendre jusqu'au départ
	}
	if time.Now().Add(cutoff).After(dep.DepartureAt) {
		return store.Booking{}, huma.Error409Conflict("Les ventes sont closes pour ce départ")
	}

	// Vente au guichet : le client est identifié par son téléphone (fidélité),
	// sauf modification d'une réservation existante (le client est déjà connu).
	if cc.channel == "counter" && cc.customerID == nil {
		c, err := q.UpsertCustomerByPhone(ctx, store.UpsertCustomerByPhoneParams{
			Phone: &contactPhone, FullName: in.ContactName, ReferralCode: refgen.Referral(),
		})
		if err != nil {
			return store.Booking{}, err
		}
		cc.customerID = &c.ID
	}

	if err := q.ExpireStaleSeatHolds(ctx, dep.ID); err != nil {
		return store.Booking{}, err
	}
	seatMap, err := q.GetDepartureSeatMap(ctx, store.GetDepartureSeatMapParams{DepartureID: dep.ID, LayoutID: dep.LayoutID})
	if err != nil {
		return store.Booking{}, err
	}
	cats, err := q.ListSeatCategories(ctx, dep.CompanyID)
	if err != nil {
		return store.Booking{}, err
	}
	categories := make(map[uuid.UUID]store.SeatCategory, len(cats))
	for _, c := range cats {
		categories[c.ID] = c
	}
	cells := make(map[uuid.UUID]store.GetDepartureSeatMapRow, len(seatMap))
	for _, c := range seatMap {
		cells[c.ID] = c
	}

	// 1) sièges choisis explicitement
	assigned := make([]store.GetDepartureSeatMapRow, len(in.Passengers))
	chosen := map[uuid.UUID]bool{}
	autoCount := 0
	for i, p := range in.Passengers {
		if p.SeatID == nil {
			autoCount++
			continue
		}
		c, ok := cells[*p.SeatID]
		if !ok || c.Kind != "seat" {
			return store.Booking{}, huma.Error422UnprocessableEntity("Siège inconnu pour ce départ")
		}
		if chosen[c.ID] {
			return store.Booking{}, huma.Error422UnprocessableEntity("Le même siège est demandé deux fois")
		}
		if c.State != "available" {
			return store.Booking{}, huma.Error409Conflict(fmt.Sprintf("Le siège %s n'est plus disponible", *c.SeatLabel))
		}
		chosen[c.ID] = true
		assigned[i] = c
	}
	// 2) attribution automatique pour les autres passagers
	if autoCount > 0 {
		prefs := SeatPreferences{}
		if in.Preferences != nil {
			prefs = *in.Preferences
		}
		picked := pickSeats(seatMap, categories, prefs, autoCount, chosen)
		if picked == nil {
			msg := "Plus assez de sièges disponibles sur ce départ"
			if prefs.CategoryCode != "" {
				msg = "Plus assez de sièges disponibles dans la catégorie demandée"
			}
			return store.Booking{}, huma.Error409Conflict(msg)
		}
		j := 0
		for i, p := range in.Passengers {
			if p.SeatID == nil {
				assigned[i] = picked[j]
				j++
			}
		}
	}

	// 3) prix (toujours calculé par le serveur) : réservez tôt, puis fidélité, par siège.
	now := time.Now()
	lc, err := loyalty.Load(ctx, q, dep.CompanyID, cc.customerID, in.RedeemPoints > 0)
	if err != nil {
		return store.Booking{}, err
	}
	tierPct := int64(lc.DiscountPercent())
	early, err := pricing.EarlyBirdFor(ctx, q, dep.CompanyID, dep.RouteID, dep.ID, dep.DepartureAt, now)
	if err != nil {
		return store.Booking{}, err
	}
	type line struct {
		cell                                                    store.GetDepartureSeatMapRow
		cat                                                     store.SeatCategory
		catSurcharge, seatSurcharge, earlyBird, discount, total int64
	}
	lines := make([]line, len(assigned))
	var subtotal, discount, earlyBirdTotal int64
	promoSeats := make([]pricing.SeatAmount, len(assigned))
	for i, c := range assigned {
		cat := categories[*c.CategoryID]
		gross := dep.BasePrice + cat.SurchargeAmount + c.ExtraSurcharge
		var eb int64
		if early != nil && i < early.SeatsLeft {
			eb = gross * early.Percent / 100
		}
		tier := (gross - eb) * tierPct / 100
		lines[i] = line{cell: c, cat: cat, catSurcharge: cat.SurchargeAmount, seatSurcharge: c.ExtraSurcharge,
			earlyBird: eb, discount: eb + tier, total: gross - eb - tier}
		promoSeats[i] = pricing.SeatAmount{CategoryCode: cat.Code, Amount: lines[i].total}
		subtotal += gross
		discount += eb + tier
		earlyBirdTotal += eb
	}

	// 4) code promo, ou à défaut remise de parrainage (premier voyage) — jamais les deux.
	var promoCode *store.PromoCode
	var promoAmount, referralAmount int64
	var referral *store.Referral
	if code := strings.TrimSpace(in.PromoCode); code != "" {
		pc, amount, err := pricing.CheckPromo(ctx, q, pricing.PromoRequest{
			CompanyID: dep.CompanyID, Code: code, Channel: cc.channel, CustomerID: cc.customerID, Phone: contactPhone,
			RouteID: dep.RouteID, Seats: promoSeats, Now: now,
		})
		if perr, ok := err.(pricing.PromoError); ok {
			return store.Booking{}, huma.Error422UnprocessableEntity(perr.Message)
		}
		if err != nil {
			return store.Booking{}, err
		}
		promoCode, promoAmount = &pc, amount
	} else if cc.channel != "counter" {
		if referral, referralAmount, err = pricing.Referral(ctx, q, dep.CompanyID, cc.customerID, contactPhone, subtotal-discount); err != nil {
			return store.Booking{}, err
		}
	}

	// 5) points de fidélité, sur le montant restant.
	var pointsUsed int32
	var pointsAmount int64
	afterOffers := subtotal - discount - promoAmount - referralAmount
	if in.RedeemPoints > 0 {
		if cc.channel == "counter" {
			return store.Booking{}, huma.Error422UnprocessableEntity("L'utilisation de points se fait depuis l'application du client")
		}
		pointsUsed, pointsAmount = lc.RedeemableAmount(in.RedeemPoints, afterOffers)
	}
	total := afterOffers - pointsAmount

	holdMinutes := time.Duration(dep.BookingHoldMinutes) * time.Minute
	expiresAt := time.Now().Add(holdMinutes)
	b, err := q.InsertBooking(ctx, store.InsertBookingParams{
		CompanyID: dep.CompanyID, Reference: refgen.Booking(), DepartureID: dep.ID, CustomerID: cc.customerID,
		Channel: cc.channel, AgencyID: cc.agencyID, CreatedByStaffID: cc.staffID,
		ContactName: in.ContactName, ContactPhone: contactPhone, ContactEmail: apix.NilIfEmpty(in.ContactEmail),
		SubtotalAmount: subtotal, DiscountAmount: discount, LoyaltyPointsUsed: pointsUsed, LoyaltyPointsAmount: pointsAmount,
		TotalAmount: total, Currency: dep.Currency, ExpiresAt: expiresAt,
		BillingName: apix.NilIfEmpty(strings.TrimSpace(in.BillingName)), BillingTaxID: apix.NilIfEmpty(strings.TrimSpace(in.BillingTaxID)),
		PromoAmount: promoAmount, EarlyBirdAmount: earlyBirdTotal, ReferralAmount: referralAmount,
		PromoCodeID: promoCodeID(promoCode), PromoCode: promoCodeText(promoCode),
	})
	if err != nil {
		return store.Booking{}, err
	}
	for i, l := range lines {
		p := in.Passengers[i]
		passengerPhone := ""
		if p.Phone != "" {
			if passengerPhone, err = phone.Normalize(p.Phone, s.Cfg.DefaultCountryCode); err != nil {
				return store.Booking{}, huma.Error422UnprocessableEntity("Téléphone du passager invalide : " + p.Name)
			}
		}
		_, err := q.InsertBookingSeat(ctx, store.InsertBookingSeatParams{
			CompanyID: dep.CompanyID, BookingID: b.ID, DepartureID: dep.ID, SeatCellID: l.cell.ID, SeatLabel: *l.cell.SeatLabel,
			CategoryID: l.cat.ID, PassengerName: p.Name, PassengerPhone: passengerPhone,
			BasePrice: dep.BasePrice, CategorySurcharge: l.catSurcharge, SeatSurcharge: l.seatSurcharge,
			DiscountAmount: l.discount, TotalPrice: l.total, HoldExpiresAt: expiresAt, EarlyBirdAmount: l.earlyBird,
		})
		if database.UniqueViolation(err) != "" {
			return store.Booking{}, huma.Error409Conflict(fmt.Sprintf("Le siège %s vient d'être pris, choisissez-en un autre", *l.cell.SeatLabel))
		}
		if err != nil {
			return store.Booking{}, err
		}
	}
	if pointsUsed > 0 {
		if err := loyalty.Redeem(ctx, q, *lc.Account, b.ID, pointsUsed); err != nil {
			return store.Booking{}, err
		}
	}
	if promoCode != nil && promoAmount > 0 {
		if _, err := q.InsertPromoRedemption(ctx, store.InsertPromoRedemptionParams{
			CompanyID: dep.CompanyID, PromoCodeID: promoCode.ID, BookingID: b.ID, CustomerID: cc.customerID,
			Phone: contactPhone, Amount: promoAmount,
		}); err != nil {
			return store.Booking{}, err
		}
	}
	if referral != nil && referralAmount > 0 {
		if err := q.SetReferralBooking(ctx, store.SetReferralBookingParams{ID: referral.ID, BookingID: &b.ID, CompanyID: &dep.CompanyID}); err != nil {
			return store.Booking{}, err
		}
	}
	return b, nil
}

func promoCodeID(pc *store.PromoCode) *uuid.UUID {
	if pc == nil {
		return nil
	}
	return &pc.ID
}

func promoCodeText(pc *store.PromoCode) *string {
	if pc == nil {
		return nil
	}
	return &pc.Code
}

// CreateForCustomer : réservation depuis l'application ou le site.
func (s *Service) CreateForCustomer(ctx context.Context, p auth.Principal, channel string, in CreateInput) (BookingDetail, error) {
	var out BookingDetail
	err := s.DB.Tx(ctx, database.Customer(), func(tx pgx.Tx, q *store.Queries) error {
		b, err := s.create(ctx, tx, q, createContext{channel: channel, customerID: &p.ID}, in)
		if err != nil {
			return err
		}
		if b.TotalAmount == 0 { // entièrement payé en points
			if _, err := s.ConfirmPaid(ctx, tx, q, b.ID); err != nil {
				return err
			}
		}
		out, err = LoadDetail(ctx, q, b.ID)
		return err
	})
	return out, err
}

// CreateAtCounter : vente au guichet, encaissement en espèces immédiat.
func (s *Service) CreateAtCounter(ctx context.Context, p auth.Principal, agencyID *uuid.UUID, in CreateInput) (BookingDetail, error) {
	if agencyID == nil {
		agencyID = p.AgencyID
	}
	if agencyID == nil {
		return BookingDetail{}, huma.Error422UnprocessableEntity("Agence de vente requise (agency_id)")
	}
	if p.Role != auth.RoleCompanyAdmin && (p.AgencyID == nil || *p.AgencyID != *agencyID) {
		return BookingDetail{}, huma.Error403Forbidden("Vous ne pouvez vendre que pour votre agence")
	}
	var out BookingDetail
	err := s.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
		if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *agencyID, CompanyID: p.CompanyID}); err != nil {
			if database.IsNotFound(err) {
				return huma.Error422UnprocessableEntity("Agence inconnue")
			}
			return err
		}
		staffID := p.ID
		b, err := s.create(ctx, tx, q, createContext{channel: "counter", agencyID: agencyID, staffID: &staffID, companyID: &p.CompanyID}, in)
		if err != nil {
			return err
		}
		if b.TotalAmount > 0 {
			pay, err := q.InsertPayment(ctx, store.InsertPaymentParams{
				CompanyID: b.CompanyID, BookingID: &b.ID, Provider: "cash", Method: "cash", Amount: b.TotalAmount,
				Currency: b.Currency, PayerPhone: b.ContactPhone, IdempotencyKey: "cash:" + b.ID.String(),
				AgencyID: agencyID, ReceivedByStaffID: &staffID,
			})
			if err != nil {
				return err
			}
			if _, err := q.MarkPaymentSucceeded(ctx, pay.ID); err != nil {
				return err
			}
		}
		if _, err := s.ConfirmPaid(ctx, tx, q, b.ID); err != nil {
			return err
		}
		if err := apix.Audit(ctx, q, &p.CompanyID, "booking.counter_sale", "booking", b.ID.String(), nil,
			map[string]any{"reference": b.Reference, "amount": b.TotalAmount, "agency_id": agencyID}); err != nil {
			return err
		}
		out, err = LoadDetail(ctx, q, b.ID)
		return err
	})
	return out, err
}

// ConfirmResult décrit l'issue de la confirmation après paiement.
type ConfirmResult struct {
	Confirmed     bool
	RefundCreated bool
}

// ConfirmPaid confirme une réservation payée : sièges vendus, billets émis,
// notification planifiée. Idempotent. Si les sièges ont été repris entre-temps
// (paiement tardif), un remboursement est créé automatiquement.
func (s *Service) ConfirmPaid(ctx context.Context, tx pgx.Tx, q *store.Queries, bookingID uuid.UUID) (ConfirmResult, error) {
	b, err := q.GetBookingForUpdate(ctx, bookingID)
	if err != nil {
		return ConfirmResult{}, err
	}
	switch b.Status {
	case "confirmed":
		return ConfirmResult{Confirmed: true}, nil
	case "cancelled", "refunded":
		return s.refundLatePayment(ctx, q, b, "Paiement reçu pour une réservation annulée")
	case "expired":
		if b.LoyaltyPointsUsed > 0 { // les points ont déjà été restitués
			return s.refundLatePayment(ctx, q, b, "Paiement reçu après expiration de la réservation")
		}
	}

	sp, err := tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ConfirmResult{}, err
	}
	seats, err := q.WithTx(sp).SellBookingSeats(ctx, b.ID)
	if database.UniqueViolation(err) != "" {
		_ = sp.Rollback(ctx)
		return s.refundLatePayment(ctx, q, b, "Paiement reçu après expiration : siège repris par un autre voyageur")
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return ConfirmResult{}, err
	}
	if b.ChangedFromBookingID != nil { // modification : l'ancienne réservation est remplacée
		ok, err := s.completeChange(ctx, q.WithTx(sp), b)
		if err != nil {
			_ = sp.Rollback(ctx)
			return ConfirmResult{}, err
		}
		if !ok {
			_ = sp.Rollback(ctx)
			if err := q.ReleaseBookingSeats(ctx, store.ReleaseBookingSeatsParams{BookingID: b.ID, Status: "expired"}); err != nil {
				return ConfirmResult{}, err
			}
			res, err := s.refundLatePayment(ctx, q, b, "Paiement reçu alors que la réservation d'origine n'était plus modifiable")
			if err == nil && !res.RefundCreated {
				err = huma.Error409Conflict("La réservation d'origine n'est plus modifiable")
			}
			return res, err
		}
	}
	if err := sp.Commit(ctx); err != nil {
		return ConfirmResult{}, err
	}

	b, err = q.ConfirmBooking(ctx, b.ID)
	if err != nil {
		return ConfirmResult{}, err
	}
	if err := s.Tickets.IssueForBooking(ctx, q, b, seats); err != nil {
		return ConfirmResult{}, err
	}
	if err := s.Queue.InsertTx(ctx, tx, jobs.BookingConfirmedArgs{BookingID: b.ID}); err != nil {
		return ConfirmResult{}, err
	}
	if err := q.ConfirmPromoRedemption(ctx, b.ID); err != nil {
		return ConfirmResult{}, err
	}
	invoice.ForBooking(ctx, tx, q, b.ID)
	return ConfirmResult{Confirmed: true}, apix.Audit(ctx, q, &b.CompanyID, "booking.confirmed", "booking", b.ID.String(), nil,
		map[string]any{"reference": b.Reference, "amount": b.TotalAmount})
}

func (s *Service) refundLatePayment(ctx context.Context, q *store.Queries, b store.Booking, reason string) (ConfirmResult, error) {
	pay, err := q.GetSucceededPaymentForBooking(ctx, &b.ID)
	if database.IsNotFound(err) {
		return ConfirmResult{}, nil
	}
	if err != nil {
		return ConfirmResult{}, err
	}
	if b.Status == "pending" {
		if _, err := q.SetBookingStatus(ctx, store.SetBookingStatusParams{ID: b.ID, Status: "expired", CancelReason: reason}); err != nil {
			return ConfirmResult{}, err
		}
	}
	if _, err := q.InsertRefund(ctx, store.InsertRefundParams{
		CompanyID: b.CompanyID, BookingID: b.ID, PaymentID: &pay.ID, Amount: pay.Amount,
		Reason: reason, RequestedByType: "system",
	}); err != nil {
		return ConfirmResult{}, err
	}
	return ConfirmResult{RefundCreated: true}, apix.Audit(ctx, q, &b.CompanyID, "refund.auto_created", "booking", b.ID.String(), nil,
		map[string]any{"reason": reason, "amount": pay.Amount})
}

// CancelOptions : paramètres d'annulation.
type CancelOptions struct {
	Reason    string
	ByStaff   bool
	WaiveFee  bool // personnel : rembourser sans frais
	SkipRules bool // annulation de départ par la compagnie : remboursement intégral, pas de délai
}

// Cancel annule une réservation dans la transaction fournie.
func (s *Service) Cancel(ctx context.Context, tx pgx.Tx, q *store.Queries, b store.Booking, opt CancelOptions) error {
	switch b.Status {
	case "pending":
		if _, err := q.SetBookingStatus(ctx, store.SetBookingStatusParams{ID: b.ID, Status: "cancelled", CancelReason: opt.Reason}); err != nil {
			return err
		}
		if err := q.ReleaseBookingSeats(ctx, store.ReleaseBookingSeatsParams{BookingID: b.ID, Status: "cancelled"}); err != nil {
			return err
		}
		if err := releaseOffers(ctx, q, b.ID); err != nil {
			return err
		}
		return loyalty.Restore(ctx, q, b.CompanyID, b.CustomerID, b.ID, b.LoyaltyPointsUsed, "Réservation annulée")

	case "confirmed":
		dep, err := q.GetDeparture(ctx, b.DepartureID)
		if err != nil {
			return err
		}
		_, r, err := s.loadRules(ctx, q, b) // règles de la compagnie + avantage fidélité
		if err != nil {
			return err
		}
		used, err := q.CountUsedTicketsForBooking(ctx, b.ID)
		if err != nil {
			return err
		}
		if used > 0 && !opt.SkipRules {
			return huma.Error409Conflict("Un passager a déjà embarqué : annulation impossible")
		}
		if !opt.ByStaff && !opt.SkipRules {
			limit := dep.DepartureAt.Add(-time.Duration(r.cancelCutoffHours) * time.Hour)
			if time.Now().After(limit) {
				return huma.Error409Conflict(fmt.Sprintf("Annulation possible jusqu'à %d h avant le départ. Contactez l'agence.", r.cancelCutoffHours))
			}
		}
		if err := s.abandonPendingChanges(ctx, q, b.ID); err != nil {
			return err
		}
		if _, err := q.SetBookingStatus(ctx, store.SetBookingStatusParams{ID: b.ID, Status: "cancelled", CancelReason: opt.Reason}); err != nil {
			return err
		}
		if err := q.ReleaseBookingSeats(ctx, store.ReleaseBookingSeatsParams{BookingID: b.ID, Status: "cancelled"}); err != nil {
			return err
		}
		if err := q.CancelBookingTickets(ctx, b.ID); err != nil {
			return err
		}
		paid, err := s.isPaid(ctx, q, b)
		if err != nil {
			return err
		}
		if paid {
			// Base : valeur payée du billet (une réservation modifiée hérite de
			// l'avoir de l'ancienne) ; les frais de modification ne sont pas remboursés.
			fee := int64(r.cancelFeePercent)
			if opt.WaiveFee || opt.SkipRules {
				fee = 0
			}
			amount := paidValue(b) * (100 - fee) / 100
			var payID *uuid.UUID
			pay, err := q.GetSucceededPaymentForBooking(ctx, &b.ID)
			if err := ignoreNotFound(err); err != nil {
				return err
			}
			if err == nil {
				payID = &pay.ID
			}
			if amount > 0 {
				reqType := "customer"
				var reqBy *uuid.UUID
				if pr, ok := auth.FromContext(ctx); ok {
					id := pr.ID
					reqBy = &id
					if pr.IsStaff() {
						reqType = "staff"
					}
				} else {
					reqType = "system"
				}
				if _, err := q.InsertRefund(ctx, store.InsertRefundParams{
					CompanyID: b.CompanyID, BookingID: b.ID, PaymentID: payID, Amount: amount,
					Reason: opt.Reason, RequestedByType: reqType, RequestedBy: reqBy,
				}); err != nil {
					return err
				}
			}
		}
		if err := loyalty.Restore(ctx, q, b.CompanyID, b.CustomerID, b.ID, b.LoyaltyPointsUsed, "Réservation annulée"); err != nil {
			return err
		}
		return s.Queue.InsertTx(ctx, tx, jobs.BookingCancelledArgs{BookingID: b.ID})

	default:
		return huma.Error409Conflict("Cette réservation ne peut plus être annulée (statut : " + b.Status + ")")
	}
}

// CancelDepartureBookings annule toutes les réservations d'un départ annulé
// par la compagnie (remboursement intégral).
func (s *Service) CancelDepartureBookings(ctx context.Context, tx pgx.Tx, q *store.Queries, departureID uuid.UUID, reason string) (int, error) {
	ids, err := q.ListActiveBookingIDsForDeparture(ctx, departureID)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		b, err := q.GetBookingForUpdate(ctx, id)
		if err != nil {
			return 0, err
		}
		if err := s.Cancel(ctx, tx, q, b, CancelOptions{Reason: reason, ByStaff: true, SkipRules: true}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// ---------------------------------------------------------------- Expiration des réservations non payées

type ExpireWorker struct {
	river.WorkerDefaults[jobs.ExpireBookingsArgs]
	DB    *database.DB
	Queue *jobs.Queue
}

func (w *ExpireWorker) Work(ctx context.Context, _ *river.Job[jobs.ExpireBookingsArgs]) error {
	return w.DB.Tx(ctx, database.System(), func(tx pgx.Tx, q *store.Queries) error {
		rows, err := q.ExpirePendingBookings(ctx)
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		if err := q.ExpireSeatsOfBookings(ctx, ids); err != nil {
			return err
		}
		for _, r := range rows {
			if err := loyalty.Restore(ctx, q, r.CompanyID, r.CustomerID, r.ID, r.LoyaltyPointsUsed, "Réservation expirée (non payée)"); err != nil {
				return err
			}
			if err := releaseOffers(ctx, q, r.ID); err != nil {
				return err
			}
			// Payer en agence : le client est prévenu, et les oublis répétés retirent l'option.
			if r.PaymentMode == "agency" {
				if err := recordPayLaterStrike(ctx, q, r.CompanyID, r.ContactPhone); err != nil {
					return err
				}
				if w.Queue != nil {
					if err := w.Queue.InsertTx(ctx, tx, jobs.PayLaterNoticeArgs{BookingID: r.ID, Event: "expired"}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// releaseOffers : réservation non payée (expirée, annulée) — le code promo et la
// remise de parrainage redeviennent disponibles pour le client.
func releaseOffers(ctx context.Context, q *store.Queries, bookingID uuid.UUID) error {
	if err := q.ReleasePromoRedemption(ctx, bookingID); err != nil {
		return err
	}
	return q.ClearReferralBooking(ctx, &bookingID)
}

// Package invoice émet les factures internes : une facture par encaissement
// (billet, modification, colis, excédent de bagage) et une facture d'avoir par
// remboursement versé. Numérotation continue par compagnie, type et année ;
// vendeur et client sont copiés à l'émission (une facture ne change plus).
//
// Les colonnes emecef_* recevront la certification DGI (facture normalisée).
package invoice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/platform/database"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

var localTZ = time.FixedZone("WAT", 3600)

// Line : ligne de facture. Amount = Quantity × UnitPrice (négatif pour une remise).
type Line struct {
	Kind      string // ticket, fare_difference, change_fee, loyalty, parcel, baggage, refund
	Label     string
	Detail    string
	Quantity  int32
	UnitPrice int64
	Amount    int64
}

type Customer struct {
	Name, Phone, Email, Company, TaxID string
}

// Draft : facture à émettre.
type Draft struct {
	CompanyID         uuid.UUID
	Kind              string // invoice ou credit_note
	PaymentID         *uuid.UUID
	RefundID          *uuid.UUID
	BookingID         *uuid.UUID
	ParcelID          *uuid.UUID
	OriginalInvoiceID *uuid.UUID
	CustomerID        *uuid.UUID
	Customer          Customer
	AgencyID          *uuid.UUID
	OperatorID        *uuid.UUID // agent ; nil = vente en ligne
	PaymentMethod     string
	Reference         string
	Currency          string
	Lines             []Line
	// WithConfirmation : le PDF part en pièce jointe de l'e-mail de confirmation
	// du billet (pas d'e-mail séparé). Sinon, envoi dédié si le client a une adresse.
	WithConfirmation bool
}

var ErrTotal = errors.New("facture : le total des lignes ne correspond pas au montant encaissé")

// Issue émet la facture dans la transaction fournie. expected : montant encaissé
// (contrôle de cohérence avec la somme des lignes).
func Issue(ctx context.Context, q *store.Queries, d Draft, expected int64) (store.Invoice, error) {
	var total int64
	for _, l := range d.Lines {
		total += l.Amount
	}
	if total != expected || total < 0 || len(d.Lines) == 0 {
		return store.Invoice{}, fmt.Errorf("%w (%d ≠ %d)", ErrTotal, total, expected)
	}
	company, err := q.GetCompany(ctx, d.CompanyID)
	if err != nil {
		return store.Invoice{}, err
	}
	year := time.Now().In(localTZ).Year()
	seq, err := q.NextInvoiceNumber(ctx, store.NextInvoiceNumberParams{CompanyID: d.CompanyID, Kind: d.Kind, Year: int32(year)})
	if err != nil {
		return store.Invoice{}, err
	}
	prefix := company.InvoicePrefix
	if d.Kind == "credit_note" {
		prefix = "AV"
	}
	agency, operator := "", "Vente en ligne"
	if d.AgencyID != nil {
		if a, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *d.AgencyID, CompanyID: d.CompanyID}); err == nil {
			agency = a.Name
		}
	}
	if d.OperatorID != nil {
		if st, err := q.GetStaff(ctx, *d.OperatorID); err == nil {
			operator = st.FullName
		}
	}
	if d.Currency == "" {
		d.Currency = company.Currency
	}
	inv, err := q.InsertInvoice(ctx, store.InsertInvoiceParams{
		CompanyID: d.CompanyID, Kind: d.Kind, Number: fmt.Sprintf("%s-%d-%06d", prefix, year, seq),
		PaymentID: d.PaymentID, RefundID: d.RefundID, BookingID: d.BookingID, ParcelID: d.ParcelID,
		OriginalInvoiceID: d.OriginalInvoiceID, CustomerID: d.CustomerID,
		SellerName: company.Name, SellerLegalName: deref(company.LegalName), SellerTaxID: deref(company.TaxID),
		SellerTradeRegister: deref(company.TradeRegister), SellerAddress: deref(company.Address),
		SellerPhone: deref(company.Phone), SellerEmail: deref(company.Email), Footer: company.InvoiceFooter,
		CustomerName: strings.TrimSpace(d.Customer.Name), CustomerPhone: d.Customer.Phone, CustomerEmail: d.Customer.Email,
		CustomerCompany: d.Customer.Company, CustomerTaxID: d.Customer.TaxID,
		AgencyName: agency, OperatorName: operator, PaymentMethod: d.PaymentMethod, Reference: d.Reference,
		Currency: d.Currency, TotalAmount: total,
	})
	if err != nil {
		return store.Invoice{}, err
	}
	for i, l := range d.Lines {
		if l.Quantity == 0 {
			l.Quantity = 1
		}
		if l.UnitPrice == 0 {
			l.UnitPrice = l.Amount / int64(l.Quantity)
		}
		if err := q.InsertInvoiceLine(ctx, store.InsertInvoiceLineParams{CompanyID: d.CompanyID, InvoiceID: inv.ID,
			Position: int32(i + 1), Kind: l.Kind, Label: l.Label, Detail: l.Detail, Quantity: l.Quantity,
			UnitPrice: l.UnitPrice, Amount: l.Amount}); err != nil {
			return store.Invoice{}, err
		}
	}
	if to := strings.TrimSpace(d.Customer.Email); to != "" && !d.WithConfirmation {
		if err := q.QueueInvoiceEmail(ctx, store.QueueInvoiceEmailParams{ID: inv.ID, EmailTo: to}); err != nil {
			return store.Invoice{}, err
		}
	}
	return inv, nil
}

// Safe émet une facture sans jamais bloquer l'encaissement : en cas d'erreur,
// la facture est annulée (point de sauvegarde) et l'anomalie journalisée.
func Safe(ctx context.Context, tx pgx.Tx, q *store.Queries, what string, fn func(q *store.Queries) error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		slog.Error("facture non émise", "objet", what, "err", err)
		return
	}
	if err := fn(q.WithTx(sp)); err != nil {
		_ = sp.Rollback(ctx)
		slog.Error("facture non émise", "objet", what, "err", err)
		return
	}
	if err := sp.Commit(ctx); err != nil {
		slog.Error("facture non émise", "objet", what, "err", err)
	}
}

// PaymentMethod : libellé du mode de paiement.
func PaymentMethod(p store.Payment) string {
	switch {
	case p.Provider == "cash" && p.Method == "mobile_money":
		return "Mobile Money (agence)"
	case p.Provider == "cash":
		return "Espèces"
	case p.Provider == "mock":
		return "Paiement en ligne (test)"
	case p.Method == "mobile_money":
		return "Mobile Money"
	case p.Method == "card":
		return "Carte bancaire"
	}
	return "Paiement en ligne"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptr[T any](v T) *T { return &v }

func trip(d store.GetDepartureDetailRow) string {
	return fmt.Sprintf("%s – %s, %s", d.OriginCity, d.DestinationCity, d.DepartureAt.In(localTZ).Format("02/01/2006 à 15h04"))
}

// ---------------------------------------------------------------- Réservations

// ForBooking facture les encaissements d'une réservation qui ne le sont pas
// encore (paiement en ligne, guichet, modification).
func ForBooking(ctx context.Context, tx pgx.Tx, q *store.Queries, bookingID uuid.UUID) {
	Safe(ctx, tx, q, "réservation "+bookingID.String(), func(q *store.Queries) error {
		pays, err := q.ListUninvoicedBookingPayments(ctx, &bookingID)
		if err != nil || len(pays) == 0 {
			return err
		}
		b, err := q.GetBooking(ctx, bookingID)
		if err != nil {
			return err
		}
		d, err := q.GetDepartureDetail(ctx, b.DepartureID)
		if err != nil {
			return err
		}
		// Trajet du voyageur (arrêt de montée → arrêt de descente), pas les extrémités de la ligne.
		if err := segment.ForBooking(ctx, q, &d, b.ID); err != nil {
			return err
		}
		seats, err := q.ListBookingSeats(ctx, b.ID)
		if err != nil {
			return err
		}
		for _, p := range pays {
			lines := bookingLines(b, d, seats)
			if sum(lines) != p.Amount { // encaissement partiel ou atypique : une ligne globale
				lines = []Line{{Kind: "ticket", Label: "Réservation " + b.Reference, Detail: trip(d), Amount: p.Amount}}
			}
			draft := Draft{CompanyID: b.CompanyID, Kind: "invoice", PaymentID: ptr(p.ID), BookingID: &b.ID, CustomerID: b.CustomerID, WithConfirmation: true,
				Customer: Customer{Name: b.ContactName, Phone: b.ContactPhone, Email: deref(b.ContactEmail),
					Company: deref(b.BillingName), TaxID: deref(b.BillingTaxID)},
				AgencyID: p.AgencyID, OperatorID: p.ReceivedByStaffID, PaymentMethod: PaymentMethod(p),
				Reference: b.Reference, Currency: b.Currency, Lines: lines}
			if _, err := Issue(ctx, q, draft, p.Amount); err != nil {
				return err
			}
		}
		return nil
	})
}

func sum(lines []Line) int64 {
	var t int64
	for _, l := range lines {
		t += l.Amount
	}
	return t
}

func bookingLines(b store.Booking, d store.GetDepartureDetailRow, seats []store.ListBookingSeatsRow) []Line {
	if b.ChangedFromBookingID != nil { // modification : seul le supplément est facturé
		var lines []Line
		if diff := b.TotalAmount - b.ChangeFeeAmount; diff > 0 {
			lines = append(lines, Line{Kind: "fare_difference", Label: "Changement de départ — différence de tarif", Detail: trip(d), Amount: diff})
		}
		if b.ChangeFeeAmount > 0 {
			n := int32(0)
			for _, s := range seats {
				if s.Status == "sold" || s.Status == "held" {
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			lines = append(lines, Line{Kind: "change_fee", Label: "Frais de modification", Detail: "Réservation " + b.Reference,
				Quantity: n, UnitPrice: b.ChangeFeeAmount / int64(n), Amount: b.ChangeFeeAmount})
		}
		return lines
	}
	var lines []Line
	for _, s := range seats {
		if s.Status != "sold" && s.Status != "held" {
			continue
		}
		lines = append(lines, Line{Kind: "ticket", Label: fmt.Sprintf("Billet %s – %s (%s)", d.OriginCity, d.DestinationCity, s.CategoryName),
			Detail: fmt.Sprintf("%s · siège %s · %s", d.DepartureAt.In(localTZ).Format("02/01/2006 15h04"), s.SeatLabel, s.PassengerName),
			Amount: s.TotalPrice})
	}
	if b.PromoAmount > 0 {
		code := ""
		if b.PromoCode != nil {
			code = *b.PromoCode
		}
		lines = append(lines, Line{Kind: "promo", Label: "Code promo " + code, Amount: -b.PromoAmount})
	}
	if b.ReferralAmount > 0 {
		lines = append(lines, Line{Kind: "referral", Label: "Offre de bienvenue (parrainage)", Amount: -b.ReferralAmount})
	}
	if b.LoyaltyPointsAmount > 0 {
		lines = append(lines, Line{Kind: "loyalty", Label: "Points de fidélité utilisés",
			Detail: fmt.Sprintf("%d points", b.LoyaltyPointsUsed), Amount: -b.LoyaltyPointsAmount})
	}
	return lines
}

// ---------------------------------------------------------------- Bagages et colis

// ForBaggage facture un excédent de bagage encaissé.
func ForBaggage(ctx context.Context, tx pgx.Tx, q *store.Queries, pay store.Payment, b store.Booking, tag string, passenger string, overKg float64) {
	Safe(ctx, tx, q, "bagage "+tag, func(q *store.Queries) error {
		draft := Draft{CompanyID: b.CompanyID, Kind: "invoice", PaymentID: &pay.ID, BookingID: &b.ID, CustomerID: b.CustomerID,
			Customer: Customer{Name: b.ContactName, Phone: b.ContactPhone, Email: deref(b.ContactEmail),
				Company: deref(b.BillingName), TaxID: deref(b.BillingTaxID)},
			AgencyID: pay.AgencyID, OperatorID: pay.ReceivedByStaffID, PaymentMethod: PaymentMethod(pay),
			Reference: b.Reference, Currency: b.Currency,
			Lines: []Line{{Kind: "baggage", Label: "Excédent de bagage", Detail: fmt.Sprintf("Étiquette %s · %s · %g kg au-delà du poids inclus", tag, passenger, overKg),
				Amount: pay.Amount}}}
		_, err := Issue(ctx, q, draft, pay.Amount)
		return err
	})
}

var parcelCategories = map[string]string{
	"document": "Document / enveloppe", "small": "Petit colis", "medium": "Colis moyen", "large": "Gros colis", "fragile": "Colis fragile",
}

// ForParcel facture un envoi de colis encaissé (au dépôt ou au retrait).
func ForParcel(ctx context.Context, tx pgx.Tx, q *store.Queries, pay store.Payment, p store.Parcel) {
	Safe(ctx, tx, q, "colis "+p.TrackingNumber, func(q *store.Queries) error {
		cust := Customer{Name: p.SenderName, Phone: p.SenderPhone, Email: deref(p.SenderEmail)}
		customerID := p.SenderCustomerID
		if p.Payer == "recipient" {
			cust = Customer{Name: p.RecipientName, Phone: p.RecipientPhone, Email: deref(p.RecipientEmail)}
			customerID = p.RecipientCustomerID
		}
		origin, _ := q.GetAgency(ctx, store.GetAgencyParams{ID: p.OriginAgencyID, CompanyID: p.CompanyID})
		dest, _ := q.GetAgency(ctx, store.GetAgencyParams{ID: p.DestinationAgencyID, CompanyID: p.CompanyID})
		draft := Draft{CompanyID: p.CompanyID, Kind: "invoice", PaymentID: &pay.ID, ParcelID: &p.ID, CustomerID: customerID,
			Customer: cust, AgencyID: pay.AgencyID, OperatorID: pay.ReceivedByStaffID, PaymentMethod: PaymentMethod(pay),
			Reference: p.TrackingNumber, Currency: p.Currency,
			Lines: []Line{{Kind: "parcel", Label: "Envoi de colis — " + parcelCategories[p.Category],
				Detail: fmt.Sprintf("%s → %s · %d pièce(s), %g kg · pour %s", origin.Name, dest.Name, p.Pieces, p.WeightKg, p.RecipientName),
				Amount: pay.Amount}}}
		_, err := Issue(ctx, q, draft, pay.Amount)
		return err
	})
}

// ---------------------------------------------------------------- Avoirs

// ForRefund émet la facture d'avoir d'un remboursement versé.
func ForRefund(ctx context.Context, tx pgx.Tx, q *store.Queries, r store.Refund, staffID uuid.UUID) {
	Safe(ctx, tx, q, "remboursement "+r.ID.String(), func(q *store.Queries) error {
		if exists, err := q.InvoiceExistsForRefund(ctx, &r.ID); err != nil || exists {
			return err
		}
		b, err := q.GetBooking(ctx, r.BookingID)
		if err != nil {
			return err
		}
		var original *uuid.UUID
		if r.PaymentID != nil {
			if inv, err := q.GetInvoiceByPayment(ctx, r.PaymentID); err == nil {
				original = &inv.ID
			}
		}
		if original == nil {
			if inv, err := q.LatestBookingInvoice(ctx, &b.ID); err == nil {
				original = &inv.ID
			} else if !database.IsNotFound(err) {
				return err
			}
		}
		method := "Remboursement"
		if r.ProcessMethod != nil {
			method = map[string]string{"mobile_money": "Mobile Money", "cash": "Espèces", "bank_transfer": "Virement bancaire", "other": "Autre"}[*r.ProcessMethod]
		}
		draft := Draft{CompanyID: r.CompanyID, Kind: "credit_note", RefundID: &r.ID, BookingID: &b.ID, OriginalInvoiceID: original,
			CustomerID: b.CustomerID, Customer: Customer{Name: b.ContactName, Phone: b.ContactPhone, Email: deref(b.ContactEmail),
				Company: deref(b.BillingName), TaxID: deref(b.BillingTaxID)},
			AgencyID: r.ProcessedAgencyID, OperatorID: &staffID, PaymentMethod: method, Reference: b.Reference, Currency: b.Currency,
			Lines: []Line{{Kind: "refund", Label: "Remboursement — réservation " + b.Reference, Detail: r.Reason, Amount: r.Amount}}}
		_, err = Issue(ctx, q, draft, r.Amount)
		return err
	})
}

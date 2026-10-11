package booking

import (
	"context"
	"fmt"
	"time"

	"busvoyage/internal/loyalty"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Conditions d'annulation et de modification d'une réservation, calculées par
// le serveur à partir des règles de la compagnie et du niveau de fidélité du
// client. Le site les affiche telles quelles : ce qui est affiché est appliqué.

type CancelPolicy struct {
	Allowed      bool       `json:"allowed"`
	Reason       string     `json:"reason,omitempty" doc:"Pourquoi l'annulation n'est pas possible"`
	Deadline     *time.Time `json:"deadline,omitempty" doc:"Annulation en ligne possible jusqu'à cette date"`
	CutoffHours  int32      `json:"cutoff_hours"`
	FeePercent   int32      `json:"fee_percent"`
	PaidAmount   int64      `json:"paid_amount" doc:"Valeur payée du billet (hors points et frais de modification)"`
	RefundAmount int64      `json:"refund_amount" doc:"Montant qui serait remboursé en cas d'annulation maintenant"`
	TierName     string     `json:"tier_name,omitempty" doc:"Niveau de fidélité dont l'avantage est appliqué"`
}

type ChangePolicy struct {
	Allowed          bool       `json:"allowed"`
	Reason           string     `json:"reason,omitempty" doc:"Pourquoi la modification n'est pas possible"`
	Deadline         *time.Time `json:"deadline,omitempty"`
	CutoffHours      int32      `json:"cutoff_hours"`
	FeePerPassenger  int64      `json:"fee_per_passenger"`
	FeeTotal         int64      `json:"fee_total"`
	FreeByTier       bool       `json:"free_by_tier" doc:"Frais offerts grâce au niveau de fidélité"`
	TierName         string     `json:"tier_name,omitempty"`
	MaxChanges       int32      `json:"max_changes"`
	RemainingChanges int32      `json:"remaining_changes"`
	WindowDays       int32      `json:"window_days" doc:"Le nouveau départ doit avoir lieu dans ce délai après le départ initial"`
	WindowEnd        *time.Time `json:"window_end,omitempty"`
	Passengers       int32      `json:"passengers"`
}

type BookingPolicy struct {
	Cancel   CancelPolicy `json:"cancel"`
	Change   ChangePolicy `json:"change"`
	Currency string       `json:"currency"`
}

// rules : règles effectives (compagnie + avantage de fidélité).
type rules struct {
	cancelCutoffHours int32
	cancelFeePercent  int32
	changeFee         int64
	freeChange        bool
	tierName          string // renseigné si le niveau améliore une règle
}

func effectiveRules(company store.Company, tier *store.LoyaltyTier) rules {
	r := rules{
		cancelCutoffHours: company.CancellationCutoffHours,
		cancelFeePercent:  company.CancellationFeePercent,
		changeFee:         company.ChangeFeeAmount,
	}
	if tier == nil {
		return r
	}
	better := false
	if v := tier.CancellationCutoffHours; v != nil && *v < r.cancelCutoffHours {
		r.cancelCutoffHours, better = *v, true
	}
	if v := tier.CancellationFeePercent; v != nil && *v < r.cancelFeePercent {
		r.cancelFeePercent, better = *v, true
	}
	if tier.FreeChange && r.changeFee > 0 {
		r.changeFee, r.freeChange, better = 0, true, true
	}
	if better {
		r.tierName = tier.Name
	}
	return r
}

func (s *Service) loadRules(ctx context.Context, q *store.Queries, b store.Booking) (store.Company, rules, error) {
	company, err := q.GetCompany(ctx, b.CompanyID)
	if err != nil {
		return company, rules{}, err
	}
	lc, err := loyalty.Load(ctx, q, b.CompanyID, b.CustomerID, false)
	if err != nil {
		return company, rules{}, err
	}
	return company, effectiveRules(company, lc.Tier), nil
}

// paidValue : part du billet réglée en argent (y compris l'avoir reporté d'une
// modification), hors points de fidélité et hors frais de modification.
func paidValue(b store.Booking) int64 {
	return max(0, b.SubtotalAmount-b.DiscountAmount-b.LoyaltyPointsAmount)
}

func (s *Service) isPaid(ctx context.Context, q *store.Queries, b store.Booking) (bool, error) {
	if b.ChangedFromBookingID != nil {
		return true, nil // payée via la réservation d'origine
	}
	n, err := q.CountSucceededPaymentsForBooking(ctx, &b.ID)
	return n > 0, err
}

// Policy calcule les conditions applicables à une réservation à l'instant now.
func (s *Service) Policy(ctx context.Context, q *store.Queries, b store.Booking, now time.Time) (BookingPolicy, error) {
	return s.policy(ctx, q, b, now, false)
}

// StaffPolicy : conditions vues du guichet. Le personnel peut modifier jusqu'au
// départ, sans limite de nombre, même si la compagnie a fermé la modification en ligne.
func (s *Service) StaffPolicy(ctx context.Context, q *store.Queries, b store.Booking, now time.Time) (BookingPolicy, error) {
	return s.policy(ctx, q, b, now, true)
}

func (s *Service) policy(ctx context.Context, q *store.Queries, b store.Booking, now time.Time, staff bool) (BookingPolicy, error) {
	out := BookingPolicy{Currency: b.Currency}
	// Délais comptés depuis l'heure prévue à l'arrêt de montée du voyageur.
	boardingAt, err := q.BookingBoardingAt(ctx, b.ID)
	if err != nil {
		return out, err
	}
	dep := struct{ DepartureAt time.Time }{boardingAt}
	company, r, err := s.loadRules(ctx, q, b)
	if err != nil {
		return out, err
	}
	used, err := q.CountUsedTicketsForBooking(ctx, b.ID)
	if err != nil {
		return out, err
	}
	seats, err := q.ListBookingSeats(ctx, b.ID)
	if err != nil {
		return out, err
	}
	paid, err := s.isPaid(ctx, q, b)
	if err != nil {
		return out, err
	}

	// ---- Annulation
	c := &out.Cancel
	c.CutoffHours, c.FeePercent, c.TierName = r.cancelCutoffHours, r.cancelFeePercent, r.tierName
	cancelDeadline := dep.DepartureAt.Add(-time.Duration(r.cancelCutoffHours) * time.Hour)
	c.Deadline = &cancelDeadline
	if paid {
		c.PaidAmount = paidValue(b)
		c.RefundAmount = c.PaidAmount * int64(100-r.cancelFeePercent) / 100
	}
	switch {
	case b.Status == "pending":
		c.Allowed, c.FeePercent, c.Deadline = true, 0, nil
	case b.Status != "confirmed":
		c.Reason = "Cette réservation ne peut plus être annulée."
	case used > 0:
		c.Reason = "Un passager a déjà embarqué : la réservation ne peut plus être annulée."
	case now.After(cancelDeadline):
		c.Reason = fmt.Sprintf("L’annulation en ligne est possible jusqu’à %d h avant le départ. Contactez l’agence.", r.cancelCutoffHours)
	default:
		c.Allowed = true
	}

	// ---- Modification
	m := &out.Change
	for _, st := range seats {
		if st.Status == "sold" || st.Status == "held" {
			m.Passengers++
		}
	}
	m.CutoffHours, m.MaxChanges, m.WindowDays = company.ChangeCutoffHours, company.ChangeMaxCount, company.ChangeWindowDays
	m.RemainingChanges = max(0, company.ChangeMaxCount-b.ChangeCount)
	m.FeePerPassenger, m.FreeByTier = r.changeFee, r.freeChange
	if r.freeChange {
		m.TierName = r.tierName
	}
	m.FeeTotal = r.changeFee * int64(m.Passengers)
	changeDeadline := dep.DepartureAt.Add(-time.Duration(company.ChangeCutoffHours) * time.Hour)
	if staff {
		changeDeadline, m.CutoffHours = dep.DepartureAt, 0
		m.RemainingChanges, m.MaxChanges = 99, 0 // 0 = sans limite au guichet
	}
	windowEnd := dep.DepartureAt.AddDate(0, 0, int(company.ChangeWindowDays))
	m.Deadline, m.WindowEnd = &changeDeadline, &windowEnd
	switch {
	case !company.ChangeEnabled && !staff:
		m.Reason = "La compagnie ne propose pas la modification des billets."
	case b.Status == "pending":
		m.Reason = "Réglez d’abord la réservation, ou annulez-la et choisissez un autre départ."
	case b.Status != "confirmed":
		m.Reason = "Cette réservation ne peut plus être modifiée."
	case used > 0:
		m.Reason = "Un passager a déjà embarqué : la réservation ne peut plus être modifiée."
	case m.RemainingChanges == 0:
		m.Reason = fmt.Sprintf("Le nombre maximal de modifications (%d) est atteint.", company.ChangeMaxCount)
	case now.After(changeDeadline) && staff:
		m.Reason = "Le bus est déjà parti."
	case now.After(changeDeadline):
		m.Reason = fmt.Sprintf("La modification est possible jusqu’à %d h avant le départ.", company.ChangeCutoffHours)
	default:
		m.Allowed = true
	}
	return out, nil
}

// ignoreNotFound : utile pour les recherches facultatives.
func ignoreNotFound(err error) error {
	if database.IsNotFound(err) {
		return nil
	}
	return err
}

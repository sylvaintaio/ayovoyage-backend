// Package pricing regroupe les remises commerciales appliquées à une
// réservation : tarif « réservez tôt », code promo et remise de parrainage.
//
// Ordre de calcul (voir docs/evolutions-v3.md) :
//
//	prix de base + supplément de catégorie
//	− réservez tôt (par siège, dans la limite du quota du départ)
//	− remise du niveau de fidélité (par siège)
//	− code promo OU remise de parrainage (sur la réservation, jamais cumulés)
//	− points de fidélité
package pricing

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

var localTZ = time.FixedZone("WAT", 3600)

// ---------------------------------------------------------------- Réservez tôt

// EarlyBird : remise applicable à un départ, et nombre de places encore à ce tarif.
type EarlyBird struct {
	Rule      store.PricingRule
	Percent   int64
	SeatsLeft int
}

// MatchEarlyBird choisit la règle la plus avantageuse pour ce départ (sans tenir compte du quota).
func MatchEarlyBird(rules []store.PricingRule, routeID uuid.UUID, departureAt, now time.Time) *store.PricingRule {
	day := departureAt.In(localTZ)
	date := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	var best *store.PricingRule
	for i := range rules {
		r := &rules[i]
		if !r.IsActive || r.Kind != "early_bird" {
			continue
		}
		if r.RouteID != nil && *r.RouteID != routeID {
			continue
		}
		if departureAt.Sub(now) < time.Duration(r.MinDaysBefore)*24*time.Hour {
			continue
		}
		if r.ValidFrom != nil && date.Before(*r.ValidFrom) || r.ValidTo != nil && date.After(*r.ValidTo) {
			continue
		}
		if slices.ContainsFunc(r.ExcludedDates, func(d time.Time) bool { return d.Equal(date) }) {
			continue
		}
		if best == nil || r.Percent > best.Percent {
			best = r
		}
	}
	return best
}

// EarlyBirdFor : règle applicable et places restantes à ce tarif sur le départ.
func EarlyBirdFor(ctx context.Context, q *store.Queries, companyID, routeID, departureID uuid.UUID, departureAt, now time.Time) (*EarlyBird, error) {
	rules, err := q.ListActivePricingRules(ctx, companyID)
	if err != nil || len(rules) == 0 {
		return nil, err
	}
	r := MatchEarlyBird(rules, routeID, departureAt, now)
	if r == nil {
		return nil, nil
	}
	used, err := q.CountEarlyBirdSeats(ctx, departureID)
	if err != nil {
		return nil, err
	}
	left := int(r.SeatQuota) - int(used)
	if left <= 0 {
		return nil, nil
	}
	return &EarlyBird{Rule: *r, Percent: int64(r.Percent), SeatsLeft: left}, nil
}

// ---------------------------------------------------------------- Codes promo

// PromoError : code refusé (message affiché tel quel au client).
type PromoError struct{ Message string }

func (e PromoError) Error() string { return e.Message }

// SeatAmount : montant d'un siège après les remises par siège, et sa catégorie.
type SeatAmount struct {
	CategoryCode string
	Amount       int64
}

// PromoRequest : contexte de vérification d'un code.
type PromoRequest struct {
	CompanyID  uuid.UUID
	Code       string
	Channel    string // web, app, counter, ussd
	CustomerID *uuid.UUID
	Phone      string
	RouteID    uuid.UUID
	Seats      []SeatAmount
	Now        time.Time
}

// CheckPromo valide un code et calcule la remise (sur les sièges éligibles).
func CheckPromo(ctx context.Context, q *store.Queries, in PromoRequest) (store.PromoCode, int64, error) {
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	pc, err := q.GetPromoCodeByCode(ctx, store.GetPromoCodeByCodeParams{CompanyID: in.CompanyID, Code: code})
	if database.IsNotFound(err) {
		return pc, 0, PromoError{"Code promo inconnu"}
	}
	if err != nil {
		return pc, 0, err
	}
	switch {
	case !pc.IsActive:
		return pc, 0, PromoError{"Ce code promo n'est plus actif"}
	case pc.ValidFrom != nil && in.Now.Before(*pc.ValidFrom):
		return pc, 0, PromoError{"Ce code promo sera valable à partir du " + pc.ValidFrom.In(localTZ).Format("02/01/2006")}
	case pc.ValidTo != nil && in.Now.After(*pc.ValidTo):
		return pc, 0, PromoError{"Ce code promo a expiré"}
	case len(pc.Channels) > 0 && !slices.Contains(pc.Channels, in.Channel):
		return pc, 0, PromoError{"Ce code promo n'est pas valable pour ce mode de réservation"}
	case len(pc.RouteIds) > 0 && !slices.Contains(pc.RouteIds, in.RouteID):
		return pc, 0, PromoError{"Ce code promo n'est pas valable sur cette ligne"}
	}

	var base int64
	for _, s := range in.Seats {
		if len(pc.CategoryCodes) == 0 || slices.Contains(pc.CategoryCodes, s.CategoryCode) {
			base += s.Amount
		}
	}
	if base == 0 {
		return pc, 0, PromoError{"Ce code promo ne s'applique pas à la catégorie de siège choisie"}
	}
	if base < pc.MinAmount {
		return pc, 0, PromoError{fmt.Sprintf("Ce code promo s'applique à partir de %d F CFA d'achat", pc.MinAmount)}
	}

	if pc.UsageLimit != nil {
		n, err := q.CountPromoUses(ctx, pc.ID)
		if err != nil {
			return pc, 0, err
		}
		if n >= *pc.UsageLimit {
			return pc, 0, PromoError{"Ce code promo a atteint son nombre maximal d'utilisations"}
		}
	}
	n, err := q.CountCustomerPromoUses(ctx, store.CountCustomerPromoUsesParams{PromoCodeID: pc.ID, Phone: in.Phone, CustomerID: in.CustomerID})
	if err != nil {
		return pc, 0, err
	}
	if n >= pc.PerCustomerLimit {
		return pc, 0, PromoError{"Vous avez déjà utilisé ce code promo"}
	}
	if pc.FirstTripOnly {
		trips, err := q.CountCustomerPaidTrips(ctx, store.CountCustomerPaidTripsParams{CompanyID: in.CompanyID, Phone: in.Phone, CustomerID: in.CustomerID})
		if err != nil {
			return pc, 0, err
		}
		if trips > 0 {
			return pc, 0, PromoError{"Ce code promo est réservé au premier voyage"}
		}
	}

	discount := pc.Value
	if pc.Kind == "percent" {
		discount = base * pc.Value / 100
		if pc.MaxDiscount != nil && discount > *pc.MaxDiscount {
			discount = *pc.MaxDiscount
		}
	}
	return pc, min(discount, base), nil
}

// Describe : « −10 % (plafonné à 2 000 F CFA) » ou « −1 500 F CFA ».
func Describe(pc store.PromoCode) string {
	if pc.Kind == "percent" {
		if pc.MaxDiscount != nil {
			return fmt.Sprintf("−%d %% (plafonné à %s)", pc.Value, money(*pc.MaxDiscount))
		}
		return fmt.Sprintf("−%d %%", pc.Value)
	}
	return "−" + money(pc.Value)
}

func money(v int64) string {
	s := fmt.Sprint(v)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	return b.String() + " F CFA"
}

// ---------------------------------------------------------------- Parrainage

// Referral : remise de bienvenue du filleul sur son premier voyage payé avec la compagnie.
func Referral(ctx context.Context, q *store.Queries, companyID uuid.UUID, customerID *uuid.UUID, phone string, base int64) (*store.Referral, int64, error) {
	if customerID == nil || base <= 0 {
		return nil, 0, nil
	}
	ref, err := q.GetReferralByReferee(ctx, *customerID)
	if database.IsNotFound(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if ref.Status != "pending" || ref.BookingID != nil {
		return nil, 0, nil // déjà utilisée (ou réservation en cours avec la remise)
	}
	prog, err := q.GetLoyaltyProgram(ctx, companyID)
	if database.IsNotFound(err) || (err == nil && (!prog.IsEnabled || prog.RefereeRewardAmount == 0)) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	trips, err := q.CountCustomerPaidTrips(ctx, store.CountCustomerPaidTripsParams{CompanyID: companyID, Phone: phone, CustomerID: customerID})
	if err != nil || trips > 0 {
		return nil, 0, err
	}
	return &ref, min(prog.RefereeRewardAmount, base), nil
}

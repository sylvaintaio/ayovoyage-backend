// Package loyalty gère le programme de fidélité : points, niveaux, avantages.
//
// Règles anti-fraude :
//   - les points sont crédités après l'EMBARQUEMENT (pas à l'achat) ;
//   - un billet ne rapporte des points qu'une fois (index unique) ;
//   - les points utilisés sont restitués si la réservation expire ou est annulée ;
//   - le grand livre est en ajout seul, le solde n'est qu'un cache.
package loyalty

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

type Service struct {
	DB *database.DB
}

// Context décrit la situation de fidélité d'un client auprès d'une compagnie.
type Context struct {
	Program *store.LoyaltyProgram // nil si aucun programme actif
	Account *store.LoyaltyAccount // nil si le client n'a pas encore de compte
	Tier    *store.LoyaltyTier    // niveau courant (nil = aucun)
}

func (c Context) DiscountPercent() int32 {
	if c.Program == nil || c.Tier == nil {
		return 0
	}
	return c.Tier.DiscountPercent
}

// Load renvoie le contexte de fidélité (lecture seule ; le compte est verrouillé
// si lock=true, pour une utilisation de points).
func Load(ctx context.Context, q *store.Queries, companyID uuid.UUID, customerID *uuid.UUID, lock bool) (Context, error) {
	var out Context
	if customerID == nil {
		return out, nil
	}
	prog, err := q.GetLoyaltyProgram(ctx, companyID)
	if database.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !prog.IsEnabled {
		return out, nil
	}
	out.Program = &prog
	var acct store.LoyaltyAccount
	params := store.GetLoyaltyAccountParams{CompanyID: companyID, CustomerID: *customerID}
	if lock {
		acct, err = q.GetLoyaltyAccountForUpdate(ctx, store.GetLoyaltyAccountForUpdateParams(params))
	} else {
		acct, err = q.GetLoyaltyAccount(ctx, params)
	}
	if database.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Account = &acct
	tiers, err := q.ListLoyaltyTiers(ctx, companyID)
	if err != nil {
		return out, err
	}
	for i := range tiers {
		if tiers[i].Code == acct.TierCode {
			out.Tier = &tiers[i]
		}
	}
	return out, nil
}

// RedeemableAmount calcule combien de points peuvent être utilisés sur un montant.
func (c Context) RedeemableAmount(requestedPoints int32, amountDue int64) (points int32, value int64) {
	if c.Program == nil || c.Account == nil || requestedPoints <= 0 || c.Program.PointValue <= 0 {
		return 0, 0
	}
	points = min(requestedPoints, c.Account.PointsBalance)
	maxValue := amountDue * int64(c.Program.MaxRedeemPercent) / 100
	if int64(points)*int64(c.Program.PointValue) > maxValue {
		points = int32(maxValue / int64(c.Program.PointValue))
	}
	return points, int64(points) * int64(c.Program.PointValue)
}

// Redeem débite des points pour une réservation.
func Redeem(ctx context.Context, q *store.Queries, acct store.LoyaltyAccount, bookingID uuid.UUID, points int32) error {
	if points <= 0 {
		return nil
	}
	if _, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
		CompanyID: acct.CompanyID, AccountID: acct.ID, EntryType: "redeem", Points: -points,
		BookingID: &bookingID, Description: "Utilisation sur une réservation",
	}); err != nil {
		return err
	}
	_, err := q.AddLoyaltyPoints(ctx, store.AddLoyaltyPointsParams{ID: acct.ID, Delta: -points})
	return err
}

// Restore recrédite les points d'une réservation expirée ou annulée.
func Restore(ctx context.Context, q *store.Queries, companyID uuid.UUID, customerID *uuid.UUID, bookingID uuid.UUID, points int32, reason string) error {
	if points <= 0 || customerID == nil {
		return nil
	}
	acct, err := q.GetLoyaltyAccountForUpdate(ctx, store.GetLoyaltyAccountForUpdateParams{CompanyID: companyID, CustomerID: *customerID})
	if err != nil {
		return err
	}
	if _, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
		CompanyID: companyID, AccountID: acct.ID, EntryType: "restore", Points: points,
		BookingID: &bookingID, Description: reason,
	}); err != nil {
		return err
	}
	_, err = q.AddLoyaltyPoints(ctx, store.AddLoyaltyPointsParams{ID: acct.ID, Delta: points})
	return err
}

// ---------------------------------------------------------------- Gain de points (après embarquement)

type EarnWorker struct {
	river.WorkerDefaults[jobs.LoyaltyEarnArgs]
	DB *database.DB
}

func (w *EarnWorker) Work(ctx context.Context, job *river.Job[jobs.LoyaltyEarnArgs]) error {
	return w.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		return Earn(ctx, q, job.Args.TicketID)
	})
}

func Earn(ctx context.Context, q *store.Queries, ticketID uuid.UUID) error {
	t, err := q.GetTicketDetail(ctx, ticketID)
	if err != nil {
		return err
	}
	if t.CustomerID == nil || t.Status != "used" {
		return nil
	}
	prog, err := q.GetLoyaltyProgram(ctx, t.CompanyID)
	if database.IsNotFound(err) || (err == nil && !prog.IsEnabled) {
		return nil
	}
	if err != nil {
		return err
	}
	acct, err := q.EnsureLoyaltyAccount(ctx, store.EnsureLoyaltyAccountParams{CompanyID: t.CompanyID, CustomerID: *t.CustomerID})
	if err != nil {
		return err
	}
	expires := time.Now().AddDate(0, int(prog.PointsValidityMonths), 0)
	var delta int32

	if acct.Inserted && prog.SignupBonusPoints > 0 {
		if _, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
			CompanyID: t.CompanyID, AccountID: acct.ID, EntryType: "bonus", Points: prog.SignupBonusPoints,
			Description: "Bonus de bienvenue", ExpiresAt: &expires,
		}); err != nil {
			return err
		}
		delta += prog.SignupBonusPoints
	}

	points := int32(t.TotalPrice/100) * prog.PointsPer100
	if points > 0 {
		entry, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
			CompanyID: t.CompanyID, AccountID: acct.ID, EntryType: "earn", Points: points,
			BookingID: &t.BookingID, TicketID: &t.ID, ExpiresAt: &expires,
			Description: "Voyage " + t.OriginCity + " → " + t.DestinationCity + " (" + t.TicketNumber + ")",
		})
		switch {
		case database.IsNotFound(err):
			// Déjà crédité (ON CONFLICT DO NOTHING) : idempotent.
		case err != nil:
			return err
		default:
			delta += entry.Points
		}
	}
	if delta != 0 {
		if _, err := q.AddLoyaltyPoints(ctx, store.AddLoyaltyPointsParams{ID: acct.ID, Delta: delta}); err != nil {
			return err
		}
	}
	if err := RefreshTier(ctx, q, t.CompanyID, *t.CustomerID, acct.ID); err != nil {
		return err
	}
	return rewardReferrer(ctx, q, prog, t)
}

// rewardReferrer : le premier voyage effectué (embarqué) d'un filleul rapporte
// des points à son parrain, dans la compagnie de ce voyage. Garde-fous : paiement
// non effectué depuis le numéro du parrain, plafond mensuel de parrainages.
func rewardReferrer(ctx context.Context, q *store.Queries, prog store.LoyaltyProgram, t store.GetTicketDetailRow) error {
	ref, err := q.GetReferralByReferee(ctx, *t.CustomerID)
	if database.IsNotFound(err) || (err == nil && ref.Status != "pending") {
		return nil
	}
	if err != nil {
		return err
	}
	if prog.ReferrerRewardPoints == 0 {
		return nil
	}
	referrer, err := q.GetCustomer(ctx, ref.ReferrerID)
	if err != nil {
		return err
	}
	reject := func(reason string) error {
		return q.MarkReferralRejected(ctx, store.MarkReferralRejectedParams{ID: ref.ID, Reason: reason})
	}
	if pay, err := q.GetSucceededPaymentForBooking(ctx, &t.BookingID); err == nil && referrer.Phone != nil && pay.PayerPhone == *referrer.Phone {
		return reject("Voyage payé depuis le numéro du parrain")
	}
	n, err := q.CountReferrerRewardsSince(ctx, store.CountReferrerRewardsSinceParams{ReferrerID: ref.ReferrerID, Since: ptr(time.Now().AddDate(0, -1, 0))})
	if err != nil {
		return err
	}
	if n >= prog.ReferrerMonthlyCap {
		return reject("Plafond mensuel de parrainages atteint")
	}
	acct, err := q.EnsureLoyaltyAccount(ctx, store.EnsureLoyaltyAccountParams{CompanyID: t.CompanyID, CustomerID: ref.ReferrerID})
	if err != nil {
		return err
	}
	expires := time.Now().AddDate(0, int(prog.PointsValidityMonths), 0)
	if _, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
		CompanyID: t.CompanyID, AccountID: acct.ID, EntryType: "bonus", Points: prog.ReferrerRewardPoints,
		BookingID: &t.BookingID, ExpiresAt: &expires, Description: "Parrainage : premier voyage d'un filleul",
	}); err != nil && !database.IsNotFound(err) {
		return err
	}
	if _, err := q.AddLoyaltyPoints(ctx, store.AddLoyaltyPointsParams{ID: acct.ID, Delta: prog.ReferrerRewardPoints}); err != nil {
		return err
	}
	return q.MarkReferralRewarded(ctx, ref.ID)
}

func ptr[T any](v T) *T { return &v }

// RefreshTier recalcule le niveau à partir des voyages des 12 derniers mois.
func RefreshTier(ctx context.Context, q *store.Queries, companyID, customerID, accountID uuid.UUID) error {
	trips, err := q.CountCustomerTrips12m(ctx, store.CountCustomerTrips12mParams{CompanyID: companyID, CustomerID: &customerID})
	if err != nil {
		return err
	}
	tiers, err := q.ListLoyaltyTiers(ctx, companyID)
	if err != nil {
		return err
	}
	code := ""
	for _, t := range tiers { // triés par min_trips croissant
		if trips >= t.MinTrips {
			code = t.Code
		}
	}
	return q.SetLoyaltyTier(ctx, store.SetLoyaltyTierParams{ID: accountID, TierCode: code, Trips12m: trips})
}

// ---------------------------------------------------------------- Expiration des points

type ExpireWorker struct {
	river.WorkerDefaults[jobs.ExpireLoyaltyPointsArgs]
	DB *database.DB
}

func (w *ExpireWorker) Work(ctx context.Context, _ *river.Job[jobs.ExpireLoyaltyPointsArgs]) error {
	return w.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		accounts, err := q.ListDormantLoyaltyAccounts(ctx)
		if err != nil {
			return err
		}
		for _, a := range accounts {
			if _, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
				CompanyID: a.CompanyID, AccountID: a.ID, EntryType: "expire", Points: -a.PointsBalance,
				Description: "Expiration des points (compte inactif)",
			}); err != nil {
				return err
			}
			if _, err := q.AddLoyaltyPoints(ctx, store.AddLoyaltyPointsParams{ID: a.ID, Delta: -a.PointsBalance}); err != nil {
				return err
			}
		}
		return nil
	})
}

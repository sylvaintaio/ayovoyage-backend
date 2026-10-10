package loyalty

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/phone"
	"busvoyage/internal/store"
)

const (
	tagMe    = "Mon compte"
	tagAdmin = "Admin — Fidélité"
)

type Handler struct {
	DB  *database.DB
	Cfg config.Config
}

// ---------------------------------------------------------------- DTO

type Program struct {
	IsEnabled            bool  `json:"is_enabled"`
	PointsPer100         int32 `json:"points_per_100" minimum:"0" maximum:"100" doc:"Points gagnés par tranche de 100 XOF payés"`
	PointValue           int32 `json:"point_value" minimum:"0" maximum:"1000" doc:"Valeur d'un point en XOF lors de son utilisation"`
	MaxRedeemPercent     int32 `json:"max_redeem_percent" minimum:"0" maximum:"100" doc:"Part maximale d'une réservation payable en points"`
	PointsValidityMonths int32 `json:"points_validity_months" minimum:"1" maximum:"60" doc:"Les points expirent après N mois sans activité"`
	SignupBonusPoints    int32 `json:"signup_bonus_points" minimum:"0" maximum:"100000" doc:"Offerts au premier voyage avec la compagnie"`
	RefereeRewardAmount  int64 `json:"referee_reward_amount" minimum:"0" maximum:"100000" default:"1000" doc:"Parrainage : remise du filleul sur son premier voyage (F CFA)"`
	ReferrerRewardPoints int32 `json:"referrer_reward_points" minimum:"0" maximum:"100000" default:"200" doc:"Parrainage : points du parrain quand le filleul a voyagé"`
	ReferrerMonthlyCap   int32 `json:"referrer_monthly_cap" minimum:"0" maximum:"100" default:"10" doc:"Parrainages récompensés au plus par parrain et par mois"`
}

type Tier struct {
	Code            string   `json:"code" pattern:"^[a-z0-9_]{2,30}$" example:"gold"`
	Name            string   `json:"name" minLength:"2" maxLength:"40" example:"Or"`
	MinTrips        int32    `json:"min_trips" minimum:"0" maximum:"1000" doc:"Voyages sur 12 mois glissants"`
	DiscountPercent int32    `json:"discount_percent" minimum:"0" maximum:"50" doc:"Réduction automatique sur les billets"`
	Perks           []string `json:"perks" maxItems:"20" example:"[\"Bagage supplémentaire gratuit\",\"Embarquement prioritaire\"]"`
	// Avantages appliqués automatiquement (vide = règle générale de la compagnie).
	CancellationFeePercent  *int32 `json:"cancellation_fee_percent,omitempty" minimum:"0" maximum:"100" doc:"Frais d'annulation pour ce niveau"`
	CancellationCutoffHours *int32 `json:"cancellation_cutoff_hours,omitempty" minimum:"0" maximum:"168" doc:"Annulation possible jusqu'à N h avant le départ"`
	FreeChange              bool   `json:"free_change,omitempty" doc:"Modification de billet sans frais"`
}

type ProgramConfig struct {
	Program Program `json:"program"`
	Tiers   []Tier  `json:"tiers" maxItems:"10"`
}

type LedgerEntry struct {
	EntryType   string     `json:"entry_type" enum:"earn,redeem,restore,revoke,expire,bonus,adjust"`
	Points      int32      `json:"points"`
	Description string     `json:"description"`
	BookingID   *uuid.UUID `json:"booking_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Account struct {
	CompanyID     uuid.UUID `json:"company_id"`
	CompanyName   string    `json:"company_name"`
	PointsBalance int32     `json:"points_balance"`
	PointsValue   int64     `json:"points_value" doc:"Valeur des points en XOF"`
	TierCode      string    `json:"tier_code"`
	TierName      string    `json:"tier_name"`
	Trips12m      int32     `json:"trips_12m"`
	NextTier      *Tier     `json:"next_tier,omitempty"`
	TripsToNext   int32     `json:"trips_to_next_tier,omitempty"`
	Perks         []string  `json:"perks" doc:"Avantages du niveau actuel"`
}

type AccountDetail struct {
	Account
	Tiers   []Tier        `json:"tiers"`
	History []LedgerEntry `json:"history"`
}

func toTier(t store.LoyaltyTier) Tier {
	return Tier{Code: t.Code, Name: t.Name, MinTrips: t.MinTrips, DiscountPercent: t.DiscountPercent, Perks: t.Perks,
		CancellationFeePercent: t.CancellationFeePercent, CancellationCutoffHours: t.CancellationCutoffHours, FreeChange: t.FreeChange}
}

func buildAccount(a store.LoyaltyAccount, companyName string, prog store.LoyaltyProgram, tiers []store.LoyaltyTier) Account {
	out := Account{CompanyID: a.CompanyID, CompanyName: companyName, PointsBalance: a.PointsBalance,
		PointsValue: int64(a.PointsBalance) * int64(prog.PointValue), TierCode: a.TierCode, Trips12m: a.Trips12m, Perks: []string{}}
	for _, t := range tiers {
		if t.Code == a.TierCode {
			out.TierName, out.Perks = t.Name, t.Perks
		}
		if t.MinTrips > a.Trips12m && out.NextTier == nil {
			nt := toTier(t)
			out.NextTier, out.TripsToNext = &nt, t.MinTrips-a.Trips12m
		}
	}
	return out
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	huma.Register(api, apix.Op("list-my-loyalty", http.MethodGet, "/v1/me/loyalty", tagMe,
		"Mes comptes fidélité (solde, niveau, avantages)", apix.Customer),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Account }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Account }{Body: []Account{}}
			err := h.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListCustomerLoyaltyAccounts(ctx, p.ID)
				if err != nil {
					return err
				}
				for _, r := range rows {
					prog, err := q.GetLoyaltyProgram(ctx, r.CompanyID)
					if err != nil {
						continue
					}
					tiers, err := q.ListLoyaltyTiers(ctx, r.CompanyID)
					if err != nil {
						return err
					}
					a := store.LoyaltyAccount{ID: r.ID, CompanyID: r.CompanyID, CustomerID: r.CustomerID,
						PointsBalance: r.PointsBalance, TierCode: r.TierCode, Trips12m: r.Trips12m}
					out.Body = append(out.Body, buildAccount(a, r.CompanyName, prog, tiers))
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-my-loyalty-account", http.MethodGet, "/v1/me/loyalty/{company_id}", tagMe,
		"Détail de mon compte fidélité (historique, niveaux)", apix.Customer),
		func(ctx context.Context, in *struct {
			CompanyID uuid.UUID `path:"company_id"`
		}) (*struct{ Body AccountDetail }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body AccountDetail }{}
			err := h.DB.Tx(ctx, database.Customer(), func(_ pgx.Tx, q *store.Queries) error {
				company, err := q.GetCompany(ctx, in.CompanyID)
				if err != nil {
					return err
				}
				prog, err := q.GetLoyaltyProgram(ctx, in.CompanyID)
				if err != nil {
					return err
				}
				tiers, err := q.ListLoyaltyTiers(ctx, in.CompanyID)
				if err != nil {
					return err
				}
				acct, err := q.GetLoyaltyAccount(ctx, store.GetLoyaltyAccountParams{CompanyID: in.CompanyID, CustomerID: p.ID})
				if database.IsNotFound(err) {
					acct = store.LoyaltyAccount{CompanyID: in.CompanyID, CustomerID: p.ID}
				} else if err != nil {
					return err
				}
				out.Body.Account = buildAccount(acct, company.Name, prog, tiers)
				out.Body.Tiers, out.Body.History = []Tier{}, []LedgerEntry{}
				for _, t := range tiers {
					out.Body.Tiers = append(out.Body.Tiers, toTier(t))
				}
				if acct.ID != uuid.Nil {
					entries, err := q.ListLoyaltyLedger(ctx, store.ListLoyaltyLedgerParams{AccountID: acct.ID, Limit: 50})
					if err != nil {
						return err
					}
					for _, e := range entries {
						out.Body.History = append(out.Body.History, LedgerEntry{EntryType: e.EntryType, Points: e.Points,
							Description: e.Description, BookingID: e.BookingID, CreatedAt: e.CreatedAt})
					}
				}
				return nil
			})
			return out, apix.Err(err)
		})

	// --- Administration
	admin := []string{auth.RoleCompanyAdmin}

	huma.Register(api, apix.Op("get-loyalty-program", http.MethodGet, "/v1/admin/loyalty", tagAdmin, "Configuration du programme de fidélité", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body ProgramConfig }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body ProgramConfig }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = loadConfig(ctx, q, p.CompanyID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("update-loyalty-program", http.MethodPut, "/v1/admin/loyalty", tagAdmin,
		"Configurer le programme (barème, niveaux, avantages)", apix.Staff, admin...)),
		"Remplace l'ensemble des niveaux. Les niveaux des clients sont recalculés à leur prochain voyage."),
		func(ctx context.Context, in *struct{ Body ProgramConfig }) (*struct{ Body ProgramConfig }, error) {
			p := auth.MustFromContext(ctx)
			seen := map[string]bool{}
			seenTrips := map[int32]bool{}
			for _, t := range in.Body.Tiers {
				if seen[t.Code] || seenTrips[t.MinTrips] {
					return nil, huma.Error422UnprocessableEntity("Chaque niveau doit avoir un code et un seuil de voyages uniques")
				}
				seen[t.Code], seenTrips[t.MinTrips] = true, true
			}
			out := &struct{ Body ProgramConfig }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, _ := loadConfig(ctx, q, p.CompanyID)
				pr := in.Body.Program
				if _, err := q.UpsertLoyaltyProgram(ctx, store.UpsertLoyaltyProgramParams{
					CompanyID: p.CompanyID, IsEnabled: pr.IsEnabled, PointsPer100: pr.PointsPer100, PointValue: pr.PointValue,
					MaxRedeemPercent: pr.MaxRedeemPercent, PointsValidityMonths: pr.PointsValidityMonths, SignupBonusPoints: pr.SignupBonusPoints,
					RefereeRewardAmount: pr.RefereeRewardAmount, ReferrerRewardPoints: pr.ReferrerRewardPoints, ReferrerMonthlyCap: pr.ReferrerMonthlyCap,
				}); err != nil {
					return err
				}
				if err := q.DeleteLoyaltyTiers(ctx, p.CompanyID); err != nil {
					return err
				}
				tiers := append([]Tier(nil), in.Body.Tiers...)
				sort.Slice(tiers, func(i, j int) bool { return tiers[i].MinTrips < tiers[j].MinTrips })
				for i, t := range tiers {
					perks := t.Perks
					if perks == nil {
						perks = []string{}
					}
					if _, err := q.InsertLoyaltyTier(ctx, store.InsertLoyaltyTierParams{
						CompanyID: p.CompanyID, Code: t.Code, Name: t.Name, MinTrips: t.MinTrips,
						DiscountPercent: t.DiscountPercent, Perks: perks, SortOrder: int32(i),
						CancellationFeePercent: t.CancellationFeePercent, CancellationCutoffHours: t.CancellationCutoffHours,
						FreeChange: t.FreeChange,
					}); err != nil {
						return err
					}
				}
				var err error
				out.Body, err = loadConfig(ctx, q, p.CompanyID)
				if err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "loyalty.program_updated", "loyalty_program", p.CompanyID.String(), before, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-loyalty-stats", http.MethodGet, "/v1/admin/loyalty/stats", tagAdmin, "Statistiques du programme", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleFinance),
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Members           int32 `json:"members"`
				ActiveMembers     int32 `json:"active_members" doc:"Actifs sur 90 jours"`
				PointsOutstanding int64 `json:"points_outstanding" doc:"Points en circulation"`
				Liability         int64 `json:"liability" doc:"Valeur des points en circulation (XOF) : dette envers les clients"`
			}
		}, error) {
			p := auth.MustFromContext(ctx)
			out := &struct {
				Body struct {
					Members           int32 `json:"members"`
					ActiveMembers     int32 `json:"active_members" doc:"Actifs sur 90 jours"`
					PointsOutstanding int64 `json:"points_outstanding" doc:"Points en circulation"`
					Liability         int64 `json:"liability" doc:"Valeur des points en circulation (XOF) : dette envers les clients"`
				}
			}{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				s, err := q.GetLoyaltyStats(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				out.Body.Members, out.Body.ActiveMembers, out.Body.PointsOutstanding = s.Members, s.ActiveMembers, s.PointsOutstanding
				if prog, err := q.GetLoyaltyProgram(ctx, p.CompanyID); err == nil {
					out.Body.Liability = s.PointsOutstanding * int64(prog.PointValue)
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("adjust-loyalty-points", http.MethodPost, "/v1/admin/loyalty/adjustments", tagAdmin,
		"Ajuster manuellement les points d'un client", apix.Staff, admin...)),
		"Geste commercial ou correction. Motif obligatoire, action journalisée."),
		func(ctx context.Context, in *struct {
			Body struct {
				CustomerPhone string `json:"customer_phone" minLength:"8" maxLength:"20"`
				Points        int32  `json:"points" minimum:"-100000" maximum:"100000" doc:"Positif pour créditer, négatif pour débiter"`
				Reason        string `json:"reason" minLength:"5" maxLength:"300"`
			}
		}) (*struct{ Body Account }, error) {
			p := auth.MustFromContext(ctx)
			if in.Body.Points == 0 {
				return nil, huma.Error422UnprocessableEntity("Le nombre de points doit être non nul")
			}
			ph, err := phone.Normalize(in.Body.CustomerPhone, h.Cfg.DefaultCountryCode)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("Numéro invalide")
			}
			out := &struct{ Body Account }{}
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				c, err := q.GetCustomerByPhone(ctx, &ph)
				if database.IsNotFound(err) {
					return apix.NotFound("Client")
				}
				if err != nil {
					return err
				}
				acct, err := q.EnsureLoyaltyAccount(ctx, store.EnsureLoyaltyAccountParams{CompanyID: p.CompanyID, CustomerID: c.ID})
				if err != nil {
					return err
				}
				if acct.PointsBalance+in.Body.Points < 0 {
					return huma.Error422UnprocessableEntity("Solde insuffisant pour ce débit")
				}
				staffID := p.ID
				if _, err := q.InsertLoyaltyEntry(ctx, store.InsertLoyaltyEntryParams{
					CompanyID: p.CompanyID, AccountID: acct.ID, EntryType: "adjust", Points: in.Body.Points,
					Description: in.Body.Reason, CreatedBy: &staffID,
				}); err != nil {
					return err
				}
				updated, err := q.AddLoyaltyPoints(ctx, store.AddLoyaltyPointsParams{ID: acct.ID, Delta: in.Body.Points})
				if err != nil {
					return err
				}
				prog, _ := q.GetLoyaltyProgram(ctx, p.CompanyID)
				tiers, err := q.ListLoyaltyTiers(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				out.Body = buildAccount(updated, "", prog, tiers)
				return apix.Audit(ctx, q, &p.CompanyID, "loyalty.points_adjusted", "loyalty_account", acct.ID.String(), nil,
					map[string]any{"customer_id": c.ID, "points": in.Body.Points, "reason": in.Body.Reason})
			})
			return out, apix.Err(err)
		})
}

func loadConfig(ctx context.Context, q *store.Queries, companyID uuid.UUID) (ProgramConfig, error) {
	out := ProgramConfig{Tiers: []Tier{}}
	prog, err := q.GetLoyaltyProgram(ctx, companyID)
	if err != nil && !database.IsNotFound(err) {
		return out, err
	}
	out.Program = Program{IsEnabled: prog.IsEnabled, PointsPer100: prog.PointsPer100, PointValue: prog.PointValue,
		MaxRedeemPercent: prog.MaxRedeemPercent, PointsValidityMonths: prog.PointsValidityMonths, SignupBonusPoints: prog.SignupBonusPoints,
		RefereeRewardAmount: prog.RefereeRewardAmount, ReferrerRewardPoints: prog.ReferrerRewardPoints, ReferrerMonthlyCap: prog.ReferrerMonthlyCap}
	tiers, err := q.ListLoyaltyTiers(ctx, companyID)
	for _, t := range tiers {
		out.Tiers = append(out.Tiers, toTier(t))
	}
	return out, err
}

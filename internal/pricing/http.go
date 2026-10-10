package pricing

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/platform/refgen"
	"busvoyage/internal/store"
)

const (
	tagPromo    = "Promotions"
	tagReferral = "Parrainage"
)

var (
	readers = []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleFinance}
	writers = []string{auth.RoleCompanyAdmin}
)

type Handler struct {
	DB  *database.DB
	Cfg config.Config
}

// ---------------------------------------------------------------- DTO

type PromoCode struct {
	ID               uuid.UUID   `json:"id"`
	Code             string      `json:"code"`
	Label            string      `json:"label"`
	Kind             string      `json:"kind" enum:"percent,fixed"`
	Value            int64       `json:"value" doc:"Pourcentage (1–100) ou montant en F CFA"`
	MaxDiscount      *int64      `json:"max_discount,omitempty" doc:"Plafond de la remise (pourcentage)"`
	MinAmount        int64       `json:"min_amount"`
	ValidFrom        *time.Time  `json:"valid_from,omitempty"`
	ValidTo          *time.Time  `json:"valid_to,omitempty"`
	UsageLimit       *int32      `json:"usage_limit,omitempty" doc:"Utilisations au total (absent : illimité)"`
	PerCustomerLimit int32       `json:"per_customer_limit"`
	FirstTripOnly    bool        `json:"first_trip_only"`
	Channels         []string    `json:"channels"`
	RouteIDs         []uuid.UUID `json:"route_ids"`
	CategoryCodes    []string    `json:"category_codes"`
	BatchID          *uuid.UUID  `json:"batch_id,omitempty"`
	IsActive         bool        `json:"is_active"`
	CreatedAt        time.Time   `json:"created_at"`
	Uses             int32       `json:"uses" doc:"Utilisations payées"`
	DiscountTotal    int64       `json:"discount_total" doc:"Total des remises accordées (réservations payées)"`
	Description      string      `json:"description" doc:"Remise en clair, ex. « −10 % (plafonné à 2 000 F CFA) »"`
}

func toPromo(p store.PromoCode, uses int32, total int64) PromoCode {
	return PromoCode{ID: p.ID, Code: p.Code, Label: p.Label, Kind: p.Kind, Value: p.Value, MaxDiscount: p.MaxDiscount,
		MinAmount: p.MinAmount, ValidFrom: p.ValidFrom, ValidTo: p.ValidTo, UsageLimit: p.UsageLimit,
		PerCustomerLimit: p.PerCustomerLimit, FirstTripOnly: p.FirstTripOnly, Channels: p.Channels,
		RouteIDs: nonNilUUIDs(p.RouteIds), CategoryCodes: nonNilStrings(p.CategoryCodes), BatchID: p.BatchID,
		IsActive: p.IsActive, CreatedAt: p.CreatedAt, Uses: uses, DiscountTotal: total, Description: Describe(p)}
}

// PromoInput : création ou modification d'un code (le code lui-même ne change plus après création).
type PromoInput struct {
	Label            string      `json:"label,omitempty" maxLength:"120" doc:"Usage interne (« Tabaski 2026 », « Partenaire X »)"`
	Kind             string      `json:"kind" enum:"percent,fixed"`
	Value            int64       `json:"value" minimum:"1" maximum:"1000000"`
	MaxDiscount      *int64      `json:"max_discount,omitempty" minimum:"1"`
	MinAmount        int64       `json:"min_amount,omitempty" minimum:"0"`
	ValidFrom        *time.Time  `json:"valid_from,omitempty"`
	ValidTo          *time.Time  `json:"valid_to,omitempty"`
	UsageLimit       *int32      `json:"usage_limit,omitempty" minimum:"1"`
	PerCustomerLimit int32       `json:"per_customer_limit,omitempty" minimum:"1" maximum:"100" default:"1"`
	FirstTripOnly    bool        `json:"first_trip_only,omitempty"`
	Channels         []string    `json:"channels,omitempty" doc:"web, app, counter, ussd (absent : tous)"`
	RouteIDs         []uuid.UUID `json:"route_ids,omitempty" doc:"Lignes concernées (absent : toutes)"`
	CategoryCodes    []string    `json:"category_codes,omitempty" doc:"Catégories de sièges (absent : toutes)"`
	IsActive         *bool       `json:"is_active,omitempty" doc:"Absent : actif"`
}

func (in PromoInput) validate() error {
	if in.Kind == "percent" && in.Value > 100 {
		return huma.Error422UnprocessableEntity("Un pourcentage ne peut pas dépasser 100")
	}
	if in.ValidFrom != nil && in.ValidTo != nil && !in.ValidTo.After(*in.ValidFrom) {
		return huma.Error422UnprocessableEntity("La fin de validité doit suivre le début")
	}
	for _, c := range in.Channels {
		if c != "web" && c != "app" && c != "counter" && c != "ussd" {
			return huma.Error422UnprocessableEntity("Canal inconnu : " + c)
		}
	}
	return nil
}

func (in PromoInput) channels() []string {
	if len(in.Channels) == 0 {
		return []string{"web", "app", "counter", "ussd"}
	}
	return in.Channels
}

func (in PromoInput) active() bool { return in.IsActive == nil || *in.IsActive }

func (in PromoInput) perCustomer() int32 { return max(in.PerCustomerLimit, 1) }

type PricingRule struct {
	ID            uuid.UUID  `json:"id"`
	Label         string     `json:"label"`
	RouteID       *uuid.UUID `json:"route_id,omitempty"`
	RouteName     string     `json:"route_name,omitempty"`
	MinDaysBefore int32      `json:"min_days_before" doc:"Remise si le départ a lieu dans N jours ou plus"`
	Percent       int32      `json:"percent"`
	SeatQuota     int32      `json:"seat_quota" doc:"Places à ce tarif par départ"`
	ValidFrom     *string    `json:"valid_from,omitempty" format:"date"`
	ValidTo       *string    `json:"valid_to,omitempty" format:"date"`
	ExcludedDates []string   `json:"excluded_dates" doc:"Jours de départ exclus (fêtes…)"`
	IsActive      bool       `json:"is_active"`
}

type RuleInput struct {
	Label         string     `json:"label,omitempty" maxLength:"80" default:"Réservez tôt"`
	RouteID       *uuid.UUID `json:"route_id,omitempty" doc:"Absent : toutes les lignes"`
	MinDaysBefore int32      `json:"min_days_before" minimum:"1" maximum:"365"`
	Percent       int32      `json:"percent" minimum:"1" maximum:"90"`
	SeatQuota     int32      `json:"seat_quota" minimum:"1" maximum:"100"`
	ValidFrom     string     `json:"valid_from,omitempty" format:"date"`
	ValidTo       string     `json:"valid_to,omitempty" format:"date"`
	ExcludedDates []string   `json:"excluded_dates,omitempty"`
	IsActive      *bool      `json:"is_active,omitempty"`
}

func (in RuleInput) parse() (from, to *time.Time, excluded []time.Time, err error) {
	parse := func(s string) (*time.Time, error) {
		if s == "" {
			return nil, nil
		}
		t, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("Date invalide : " + s)
		}
		return &t, nil
	}
	if from, err = parse(in.ValidFrom); err != nil {
		return
	}
	if to, err = parse(in.ValidTo); err != nil {
		return
	}
	excluded = []time.Time{}
	for _, d := range in.ExcludedDates {
		t, perr := parse(d)
		if perr != nil {
			return nil, nil, nil, perr
		}
		excluded = append(excluded, *t)
	}
	return
}

func toRule(r store.PricingRule, routeName string) PricingRule {
	date := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := t.Format(time.DateOnly)
		return &s
	}
	ex := make([]string, 0, len(r.ExcludedDates))
	for _, d := range r.ExcludedDates {
		ex = append(ex, d.Format(time.DateOnly))
	}
	return PricingRule{ID: r.ID, Label: r.Label, RouteID: r.RouteID, RouteName: routeName, MinDaysBefore: r.MinDaysBefore,
		Percent: r.Percent, SeatQuota: r.SeatQuota, ValidFrom: date(r.ValidFrom), ValidTo: date(r.ValidTo),
		ExcludedDates: ex, IsActive: r.IsActive}
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	h.registerPublic(api)
	h.registerPromoAdmin(api)
	h.registerRules(api)
	h.registerReferral(api)
}

func (h *Handler) registerPublic(api huma.API) {
	type PromoCheck struct {
		Valid       bool   `json:"valid"`
		Message     string `json:"message" doc:"Refus motivé, ou remise en clair"`
		Description string `json:"description,omitempty"`
		Discount    int64  `json:"discount" doc:"Remise estimée (hors fidélité ; calculée définitivement à la réservation)"`
	}
	huma.Register(api, apix.WithDescription(apix.Op("check-promo-code", http.MethodPost, "/v1/public/promo-codes/check", tagPromo,
		"Vérifier un code promo avant de réserver", apix.Public),
		"Estimation pour un départ et des catégories de sièges (une par passager). Le contrôle définitif "+
			"(utilisations par client, premier voyage…) est fait à la réservation."),
		func(ctx context.Context, in *struct {
			Body struct {
				DepartureID   uuid.UUID `json:"departure_id"`
				Code          string    `json:"code" minLength:"3" maxLength:"30"`
				CategoryCodes []string  `json:"category_codes" minItems:"1" maxItems:"10" doc:"Catégorie de chaque siège"`
			}
		}) (*struct{ Body PromoCheck }, error) {
			out := &struct{ Body PromoCheck }{}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := q.GetDepartureDetail(ctx, in.Body.DepartureID)
				if err != nil {
					return apix.NotFound("Départ")
				}
				cats, err := q.ListSeatCategories(ctx, d.CompanyID)
				if err != nil {
					return err
				}
				surcharge := map[string]int64{}
				for _, c := range cats {
					surcharge[c.Code] = c.SurchargeAmount
				}
				seats := make([]SeatAmount, 0, len(in.Body.CategoryCodes))
				for _, code := range in.Body.CategoryCodes {
					seats = append(seats, SeatAmount{CategoryCode: code, Amount: d.BasePrice + surcharge[code]})
				}
				pc, discount, err := CheckPromo(ctx, q, PromoRequest{CompanyID: d.CompanyID, Code: in.Body.Code, Channel: "web",
					RouteID: d.RouteID, Seats: seats, Now: time.Now()})
				if perr, ok := err.(PromoError); ok {
					out.Body.Message = perr.Message
					return nil
				}
				if err != nil {
					return err
				}
				out.Body = PromoCheck{Valid: true, Description: Describe(pc), Discount: discount,
					Message: fmt.Sprintf("Code %s appliqué : %s", pc.Code, Describe(pc))}
				return nil
			})
			return out, apix.Err(err)
		})
}

func (h *Handler) registerPromoAdmin(api huma.API) {
	huma.Register(api, apix.Op("list-promo-codes", http.MethodGet, "/v1/admin/promo-codes", tagPromo, "Codes promo", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			apix.Page
			Q       string    `query:"q" maxLength:"40" doc:"Code ou libellé"`
			BatchID uuid.UUID `query:"batch_id"`
		}) (*struct{ Body []PromoCode }, error) {
			p := auth.MustFromContext(ctx)
			params := store.ListPromoCodesParams{CompanyID: p.CompanyID, Q: apix.NilIfEmpty(strings.TrimSpace(in.Q)), Lim: in.Limit, Off: in.Offset}
			if in.BatchID != uuid.Nil {
				params.BatchID = &in.BatchID
			}
			out := &struct{ Body []PromoCode }{Body: []PromoCode{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListPromoCodes(ctx, params)
				for _, r := range rows {
					out.Body = append(out.Body, toPromo(store.PromoCode{ID: r.ID, CompanyID: r.CompanyID, Code: r.Code, Label: r.Label,
						Kind: r.Kind, Value: r.Value, MaxDiscount: r.MaxDiscount, MinAmount: r.MinAmount, ValidFrom: r.ValidFrom,
						ValidTo: r.ValidTo, UsageLimit: r.UsageLimit, PerCustomerLimit: r.PerCustomerLimit, FirstTripOnly: r.FirstTripOnly,
						Channels: r.Channels, RouteIds: r.RouteIds, CategoryCodes: r.CategoryCodes, BatchID: r.BatchID,
						IsActive: r.IsActive, CreatedAt: r.CreatedAt}, r.Uses, r.DiscountTotal))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.RequireFreshMFA(apix.Op("create-promo-code", http.MethodPost, "/v1/admin/promo-codes", tagPromo,
		"Créer un code promo", apix.Staff, writers...)),
		func(ctx context.Context, in *struct {
			Body struct {
				Code string `json:"code" pattern:"^[A-Za-z0-9-]{3,30}$" doc:"Lisible, ex. TABASKI26"`
				PromoInput
			}
		}) (*struct{ Body PromoCode }, error) {
			if err := in.Body.validate(); err != nil {
				return nil, err
			}
			p := auth.MustFromContext(ctx)
			out := &struct{ Body PromoCode }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				pc, err := insertPromo(ctx, q, p, strings.ToUpper(in.Body.Code), in.Body.PromoInput, nil)
				if database.UniqueViolation(err) != "" {
					return huma.Error409Conflict("Ce code existe déjà")
				}
				if err != nil {
					return err
				}
				out.Body = toPromo(pc, 0, 0)
				return apix.Audit(ctx, q, &p.CompanyID, "promo_code.created", "promo_code", pc.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	type PromoBatch struct {
		BatchID uuid.UUID `json:"batch_id"`
		Codes   []string  `json:"codes"`
	}
	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("create-promo-batch", http.MethodPost, "/v1/admin/promo-codes/batch", tagPromo,
		"Générer une série de codes à usage unique", apix.Staff, writers...)),
		"Pour un partenaire, un influenceur ou une campagne : N codes `PRÉFIXE-XXXXXX`, chacun utilisable une seule fois. "+
			"Export CSV : `GET /v1/admin/promo-codes/export?batch_id=`."),
		func(ctx context.Context, in *struct {
			Body struct {
				Prefix string `json:"prefix" pattern:"^[A-Za-z0-9]{2,12}$"`
				Count  int    `json:"count" minimum:"1" maximum:"500"`
				PromoInput
			}
		}) (*struct{ Body PromoBatch }, error) {
			if err := in.Body.validate(); err != nil {
				return nil, err
			}
			p := auth.MustFromContext(ctx)
			batch := uuid.New()
			one := int32(1)
			in.Body.UsageLimit, in.Body.PerCustomerLimit = &one, 1
			out := &struct{ Body PromoBatch }{Body: PromoBatch{BatchID: batch, Codes: []string{}}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				prefix := strings.ToUpper(in.Body.Prefix)
				for len(out.Body.Codes) < in.Body.Count {
					pc, err := insertPromo(ctx, q, p, refgen.Promo(prefix), in.Body.PromoInput, &batch)
					if database.UniqueViolation(err) != "" {
						continue // collision improbable : nouveau tirage
					}
					if err != nil {
						return err
					}
					out.Body.Codes = append(out.Body.Codes, pc.Code)
				}
				return apix.Audit(ctx, q, &p.CompanyID, "promo_code.batch_created", "promo_batch", batch.String(), nil,
					map[string]any{"prefix": prefix, "count": in.Body.Count, "label": in.Body.Label})
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.RequireFreshMFA(apix.Op("update-promo-code", http.MethodPut, "/v1/admin/promo-codes/{id}", tagPromo,
		"Modifier ou désactiver un code promo", apix.Staff, writers...)),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body PromoInput
		}) (*struct{ Body PromoCode }, error) {
			if err := in.Body.validate(); err != nil {
				return nil, err
			}
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body PromoCode }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetPromoCode(ctx, in.ID)
				if err != nil || before.CompanyID != p.CompanyID {
					return apix.NotFound("Code promo")
				}
				pc, err := q.UpdatePromoCode(ctx, store.UpdatePromoCodeParams{ID: in.ID, CompanyID: p.CompanyID, Label: b.Label, Kind: b.Kind,
					Value: b.Value, MaxDiscount: b.MaxDiscount, MinAmount: b.MinAmount, ValidFrom: b.ValidFrom, ValidTo: b.ValidTo,
					UsageLimit: b.UsageLimit, PerCustomerLimit: b.perCustomer(), FirstTripOnly: b.FirstTripOnly, Channels: b.channels(),
					RouteIds: nonNilUUIDs(b.RouteIDs), CategoryCodes: nonNilStrings(b.CategoryCodes), IsActive: b.active()})
				if err != nil {
					return err
				}
				out.Body = toPromo(pc, 0, 0)
				return apix.Audit(ctx, q, &p.CompanyID, "promo_code.updated", "promo_code", pc.ID.String(), toPromo(before, 0, 0), out.Body)
			})
			return out, apix.Err(err)
		})

	type PromoStats struct {
		Uses          int32 `json:"uses" doc:"Réservations payées avec ce code"`
		Pending       int32 `json:"pending" doc:"Réservations en attente de paiement"`
		Customers     int32 `json:"customers" doc:"Clients distincts"`
		DiscountTotal int64 `json:"discount_total" doc:"Coût des remises"`
		Revenue       int64 `json:"revenue" doc:"Chiffre d'affaires des réservations concernées (après remise)"`
	}
	huma.Register(api, apix.Op("get-promo-code-stats", http.MethodGet, "/v1/admin/promo-codes/{id}/stats", tagPromo,
		"Résultats d'un code promo", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body PromoStats }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body PromoStats }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if pc, err := q.GetPromoCode(ctx, in.ID); err != nil || pc.CompanyID != p.CompanyID {
					return apix.NotFound("Code promo")
				}
				s, err := q.PromoCodeStats(ctx, in.ID)
				out.Body = PromoStats{Uses: s.Uses, Pending: s.Pending, Customers: s.Customers, DiscountTotal: s.DiscountTotal, Revenue: s.Revenue}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("export-promo-codes", http.MethodGet, "/v1/admin/promo-codes/export", tagPromo,
		"Exporter des codes promo (CSV)", apix.Staff, readers...),
		func(ctx context.Context, in *struct {
			BatchID uuid.UUID `query:"batch_id"`
		}) (*struct {
			ContentType        string `header:"Content-Type"`
			ContentDisposition string `header:"Content-Disposition"`
			Body               []byte
		}, error) {
			p := auth.MustFromContext(ctx)
			params := store.ListPromoCodesParams{CompanyID: p.CompanyID, Lim: 10000}
			if in.BatchID != uuid.Nil {
				params.BatchID = &in.BatchID
			}
			var buf bytes.Buffer
			buf.WriteString("\ufeff")
			w := csv.NewWriter(&buf)
			w.Comma = ';'
			_ = w.Write([]string{"Code", "Libellé", "Remise", "Valable du", "Au", "Utilisations payées", "Actif"})
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListPromoCodes(ctx, params)
				for _, r := range rows {
					pc := store.PromoCode{Kind: r.Kind, Value: r.Value, MaxDiscount: r.MaxDiscount}
					_ = w.Write([]string{r.Code, r.Label, Describe(pc), dateFR(r.ValidFrom), dateFR(r.ValidTo),
						fmt.Sprint(r.Uses), map[bool]string{true: "oui", false: "non"}[r.IsActive]})
				}
				return err
			})
			if err != nil {
				return nil, apix.Err(err)
			}
			w.Flush()
			return &struct {
				ContentType        string `header:"Content-Type"`
				ContentDisposition string `header:"Content-Disposition"`
				Body               []byte
			}{ContentType: "text/csv; charset=utf-8", ContentDisposition: `attachment; filename="codes-promo.csv"`, Body: buf.Bytes()}, nil
		})
}

func insertPromo(ctx context.Context, q *store.Queries, p auth.Principal, code string, in PromoInput, batch *uuid.UUID) (store.PromoCode, error) {
	staffID := p.ID
	return q.InsertPromoCode(ctx, store.InsertPromoCodeParams{CompanyID: p.CompanyID, Code: code, Label: strings.TrimSpace(in.Label),
		Kind: in.Kind, Value: in.Value, MaxDiscount: in.MaxDiscount, MinAmount: in.MinAmount, ValidFrom: in.ValidFrom,
		ValidTo: in.ValidTo, UsageLimit: in.UsageLimit, PerCustomerLimit: in.perCustomer(), FirstTripOnly: in.FirstTripOnly,
		Channels: in.channels(), RouteIds: nonNilUUIDs(in.RouteIDs), CategoryCodes: nonNilStrings(in.CategoryCodes),
		BatchID: batch, IsActive: in.active(), CreatedBy: &staffID})
}

func (h *Handler) registerRules(api huma.API) {
	huma.Register(api, apix.Op("list-pricing-rules", http.MethodGet, "/v1/admin/pricing-rules", tagPromo,
		"Tarifs « réservez tôt »", apix.Staff, readers...),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []PricingRule }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []PricingRule }{Body: []PricingRule{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListPricingRules(ctx, p.CompanyID)
				for _, r := range rows {
					name := ""
					if r.RouteName != nil {
						name = *r.RouteName
					}
					out.Body = append(out.Body, toRule(store.PricingRule{ID: r.ID, CompanyID: r.CompanyID, Kind: r.Kind, Label: r.Label,
						RouteID: r.RouteID, MinDaysBefore: r.MinDaysBefore, Percent: r.Percent, SeatQuota: r.SeatQuota,
						ValidFrom: r.ValidFrom, ValidTo: r.ValidTo, ExcludedDates: r.ExcludedDates, IsActive: r.IsActive}, name))
				}
				return err
			})
			return out, apix.Err(err)
		})

	save := func(ctx context.Context, id *uuid.UUID, in RuleInput) (PricingRule, error) {
		p := auth.MustFromContext(ctx)
		from, to, excluded, err := in.parse()
		if err != nil {
			return PricingRule{}, err
		}
		active := in.IsActive == nil || *in.IsActive
		label := strings.TrimSpace(in.Label)
		if label == "" {
			label = "Réservez tôt"
		}
		var out PricingRule
		err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
			if in.RouteID != nil {
				if _, err := q.GetRoute(ctx, store.GetRouteParams{ID: *in.RouteID, CompanyID: p.CompanyID}); err != nil {
					return huma.Error422UnprocessableEntity("Ligne inconnue")
				}
			}
			var r store.PricingRule
			var err error
			if id == nil {
				r, err = q.InsertPricingRule(ctx, store.InsertPricingRuleParams{CompanyID: p.CompanyID, Label: label, RouteID: in.RouteID,
					MinDaysBefore: in.MinDaysBefore, Percent: in.Percent, SeatQuota: in.SeatQuota, ValidFrom: from, ValidTo: to,
					ExcludedDates: excluded, IsActive: active})
			} else {
				r, err = q.UpdatePricingRule(ctx, store.UpdatePricingRuleParams{ID: *id, CompanyID: p.CompanyID, Label: label, RouteID: in.RouteID,
					MinDaysBefore: in.MinDaysBefore, Percent: in.Percent, SeatQuota: in.SeatQuota, ValidFrom: from, ValidTo: to,
					ExcludedDates: excluded, IsActive: active})
				if database.IsNotFound(err) {
					return apix.NotFound("Règle")
				}
			}
			if err != nil {
				return err
			}
			out = toRule(r, "")
			return apix.Audit(ctx, q, &p.CompanyID, "pricing_rule.saved", "pricing_rule", r.ID.String(), nil, out)
		})
		return out, err
	}
	huma.Register(api, apix.RequireFreshMFA(apix.Op("create-pricing-rule", http.MethodPost, "/v1/admin/pricing-rules", tagPromo,
		"Créer un tarif « réservez tôt »", apix.Staff, writers...)),
		func(ctx context.Context, in *struct{ Body RuleInput }) (*struct{ Body PricingRule }, error) {
			r, err := save(ctx, nil, in.Body)
			return &struct{ Body PricingRule }{Body: r}, apix.Err(err)
		})
	huma.Register(api, apix.RequireFreshMFA(apix.Op("update-pricing-rule", http.MethodPut, "/v1/admin/pricing-rules/{id}", tagPromo,
		"Modifier ou désactiver un tarif « réservez tôt »", apix.Staff, writers...)),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body RuleInput
		}) (*struct{ Body PricingRule }, error) {
			r, err := save(ctx, &in.ID, in.Body)
			return &struct{ Body PricingRule }{Body: r}, apix.Err(err)
		})
}

// ---------------------------------------------------------------- Parrainage

type ReferralFriend struct {
	FirstName  string     `json:"first_name"`
	Status     string     `json:"status" enum:"pending,rewarded,rejected" doc:"pending : n'a pas encore voyagé"`
	CreatedAt  time.Time  `json:"created_at"`
	RewardedAt *time.Time `json:"rewarded_at,omitempty"`
}

type ReferralInfo struct {
	Code             string           `json:"code"`
	ShareURL         string           `json:"share_url" doc:"Lien d'invitation à partager"`
	RefereeReward    int64            `json:"referee_reward" doc:"Remise offerte au filleul sur son premier voyage"`
	ReferrerPoints   int32            `json:"referrer_points" doc:"Points gagnés quand le filleul a voyagé"`
	ReferredBy       string           `json:"referred_by,omitempty" doc:"Prénom du parrain"`
	CanEnterCode     bool             `json:"can_enter_code" doc:"Le client peut encore saisir le code d'un parrain"`
	WelcomeAvailable bool             `json:"welcome_available" doc:"Offre de bienvenue encore à utiliser"`
	Friends          []ReferralFriend `json:"friends"`
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return "Un proche"
}

func (h *Handler) referralInfo(ctx context.Context, q *store.Queries, customerID uuid.UUID) (ReferralInfo, error) {
	c, err := q.GetCustomer(ctx, customerID)
	if err != nil {
		return ReferralInfo{}, err
	}
	info := ReferralInfo{Code: c.ReferralCode, ShareURL: h.Cfg.PublicWebURL + "/connexion?parrain=" + c.ReferralCode, Friends: []ReferralFriend{}}
	if cos, err := q.ListActiveCompanies(ctx); err == nil && len(cos) > 0 {
		if prog, err := q.GetLoyaltyProgram(ctx, cos[0].ID); err == nil && prog.IsEnabled {
			info.RefereeReward, info.ReferrerPoints = prog.RefereeRewardAmount, prog.ReferrerRewardPoints
		}
	}
	ref, err := q.GetReferralByReferee(ctx, customerID)
	switch {
	case err == nil:
		if parrain, err := q.GetCustomer(ctx, ref.ReferrerID); err == nil {
			info.ReferredBy = firstName(parrain.FullName)
		}
		info.WelcomeAvailable = ref.Status == "pending"
	case database.IsNotFound(err):
		trips, err := q.CountCustomerPaidTripsAll(ctx, &customerID)
		if err != nil {
			return info, err
		}
		info.CanEnterCode = trips == 0
	default:
		return info, err
	}
	rows, err := q.ListReferralsByReferrer(ctx, customerID)
	for _, r := range rows {
		info.Friends = append(info.Friends, ReferralFriend{FirstName: firstName(r.RefereeName), Status: r.Status,
			CreatedAt: r.CreatedAt, RewardedAt: r.RewardedAt})
	}
	return info, err
}

func (h *Handler) registerReferral(api huma.API) {
	huma.Register(api, apix.Op("get-my-referral", http.MethodGet, "/v1/me/referral", tagReferral, "Mon parrainage", apix.Customer),
		func(ctx context.Context, _ *struct{}) (*struct{ Body ReferralInfo }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body ReferralInfo }{}
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = h.referralInfo(ctx, q, p.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("enter-referral-code", http.MethodPost, "/v1/me/referral", tagReferral,
		"Saisir le code de mon parrain", apix.Customer),
		"Réservé aux nouveaux voyageurs (aucun voyage payé). Le filleul obtient une remise sur son premier voyage ; "+
			"le parrain gagne des points quand ce voyage est effectué."),
		func(ctx context.Context, in *struct {
			Body struct {
				Code string `json:"code" minLength:"4" maxLength:"20"`
			}
		}) (*struct{ Body ReferralInfo }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body ReferralInfo }{}
			err := h.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
				parrain, err := q.GetCustomerByReferralCode(ctx, strings.TrimSpace(in.Body.Code))
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Code de parrainage inconnu")
				}
				if err != nil {
					return err
				}
				if parrain.ID == p.ID {
					return huma.Error422UnprocessableEntity("Vous ne pouvez pas être votre propre parrain")
				}
				trips, err := q.CountCustomerPaidTripsAll(ctx, &p.ID)
				if err != nil {
					return err
				}
				if trips > 0 {
					return huma.Error409Conflict("Le parrainage est réservé aux nouveaux voyageurs")
				}
				if _, err := q.InsertReferral(ctx, store.InsertReferralParams{ReferrerID: parrain.ID, RefereeID: p.ID}); database.IsNotFound(err) {
					return huma.Error409Conflict("Vous avez déjà un parrain")
				} else if err != nil {
					return err
				}
				out.Body, err = h.referralInfo(ctx, q, p.ID)
				return err
			})
			return out, apix.Err(err)
		})
}

// ---------------------------------------------------------------- Utilitaires

func nonNilUUIDs(v []uuid.UUID) []uuid.UUID {
	if v == nil {
		return []uuid.UUID{}
	}
	return v
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func dateFR(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.In(localTZ).Format("02/01/2006")
}

package network

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Showcase : données réelles pour la vitrine du site (accueil, services, fidélité).
type Showcase struct {
	Stats struct {
		Cities              int32 `json:"cities"`
		Agencies            int32 `json:"agencies"`
		Routes              int32 `json:"routes"`
		DeparturesNext7Days int32 `json:"departures_next_7_days"`
		PassengersCarried   int32 `json:"passengers_carried"`
	} `json:"stats"`
	Routes   []ShowcaseRoute  `json:"routes"`
	Next     []ShowcaseNext   `json:"next_departures" doc:"Prochains départs ouverts à la vente (6 au plus)"`
	Offers   []ShowcaseOffer  `json:"offers" doc:"Catégories de sièges (Standard, Premium, VIP…)"`
	Tiers    []ShowcaseTier   `json:"loyalty_tiers"`
	Loyalty  *ShowcaseLoyalty `json:"loyalty,omitempty"`
	Policies []ShowcasePolicy `json:"policies" doc:"Conditions d'annulation et de modification de chaque compagnie"`
}

type ShowcasePolicy struct {
	CompanyName             string `json:"company_name"`
	CancellationCutoffHours int32  `json:"cancellation_cutoff_hours"`
	CancellationFeePercent  int32  `json:"cancellation_fee_percent"`
	ChangeEnabled           bool   `json:"change_enabled"`
	ChangeCutoffHours       int32  `json:"change_cutoff_hours"`
	ChangeFeeAmount         int64  `json:"change_fee_amount"`
	ChangeMaxCount          int32  `json:"change_max_count"`
	ChangeWindowDays        int32  `json:"change_window_days"`
}

// ShowcaseNext : ligne du tableau des prochains départs.
type ShowcaseNext struct {
	ID              uuid.UUID `json:"id"`
	DepartureAt     time.Time `json:"departure_at"`
	OriginCity      string    `json:"origin_city"`
	OriginSlug      string    `json:"origin_slug"`
	DestinationCity string    `json:"destination_city"`
	DestinationSlug string    `json:"destination_slug"`
	Price           int64     `json:"price"`
	AvailableSeats  int32     `json:"available_seats"`
	Boarding        bool      `json:"boarding" doc:"Embarquement en cours"`
	CompanyName     string    `json:"company_name"`
}

type ShowcaseRoute struct {
	Name                string `json:"name"`
	OriginCity          string `json:"origin_city"`
	OriginSlug          string `json:"origin_slug"`
	DestinationCity     string `json:"destination_city"`
	DestinationSlug     string `json:"destination_slug"`
	FromPrice           int64  `json:"from_price"`
	DurationMinutes     int32  `json:"duration_minutes"`
	DistanceKm          *int32 `json:"distance_km,omitempty"`
	IncludedBaggageKg   int32  `json:"included_baggage_kg"`
	DeparturesNext7Days int32  `json:"departures_next_7_days"`
	CompanyName         string `json:"company_name"`
}

type ShowcaseOffer struct {
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Surcharge   int64    `json:"surcharge_amount"`
	Color       string   `json:"color"`
	Perks       []string `json:"perks"`
	CompanyName string   `json:"company_name"`
}

type ShowcaseTier struct {
	Code                    string   `json:"code"`
	Name                    string   `json:"name"`
	MinTrips                int32    `json:"min_trips"`
	DiscountPercent         int32    `json:"discount_percent"`
	Perks                   []string `json:"perks"`
	CompanyName             string   `json:"company_name"`
	CancellationFeePercent  *int32   `json:"cancellation_fee_percent,omitempty"`
	CancellationCutoffHours *int32   `json:"cancellation_cutoff_hours,omitempty"`
	FreeChange              bool     `json:"free_change"`
}

type ShowcaseLoyalty struct {
	PointsPer100      int32 `json:"points_per_100"`
	PointValue        int32 `json:"point_value"`
	SignupBonusPoints int32 `json:"signup_bonus_points"`
}

func (h *Handler) registerShowcase(api huma.API) {
	huma.Register(api, apix.Op("get-showcase", http.MethodGet, "/v1/public/showcase", tagPublic,
		"Vitrine du site : chiffres clés, lignes, offres, fidélité", apix.Public),
		func(ctx context.Context, _ *struct{}) (*struct {
			CacheControl string `header:"Cache-Control"`
			Body         Showcase
		}, error) {
			out := &struct {
				CacheControl string `header:"Cache-Control"`
				Body         Showcase
			}{CacheControl: "public, max-age=300"}
			b := &out.Body
			b.Routes, b.Offers, b.Tiers, b.Policies = []ShowcaseRoute{}, []ShowcaseOffer{}, []ShowcaseTier{}, []ShowcasePolicy{}
			b.Next = []ShowcaseNext{}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				s, err := q.ShowcaseStats(ctx)
				if err != nil {
					return err
				}
				b.Stats.Cities, b.Stats.Agencies, b.Stats.Routes = s.Cities, s.Agencies, s.Routes
				b.Stats.DeparturesNext7Days, b.Stats.PassengersCarried = s.DeparturesNext7Days, s.PassengersCarried
				routes, err := q.ShowcaseRoutes(ctx)
				if err != nil {
					return err
				}
				for _, r := range routes {
					b.Routes = append(b.Routes, ShowcaseRoute{Name: r.Name, OriginCity: r.OriginCity, OriginSlug: r.OriginSlug,
						DestinationCity: r.DestinationCity, DestinationSlug: r.DestinationSlug, FromPrice: r.BasePrice,
						DurationMinutes: r.DurationMinutes, DistanceKm: r.DistanceKm, IncludedBaggageKg: r.IncludedBaggageKg,
						DeparturesNext7Days: r.DeparturesNext7Days, CompanyName: r.CompanyName})
				}
				next, err := q.ShowcaseNextDepartures(ctx)
				if err != nil {
					return err
				}
				for _, d := range next {
					b.Next = append(b.Next, ShowcaseNext{ID: d.ID, DepartureAt: d.DepartureAt, OriginCity: d.OriginCity,
						OriginSlug: d.OriginSlug, DestinationCity: d.DestinationCity, DestinationSlug: d.DestinationSlug,
						Price: d.BasePrice, AvailableSeats: d.AvailableSeats, Boarding: d.Status == "boarding", CompanyName: d.CompanyName})
				}
				offers, err := q.ShowcaseSeatCategories(ctx)
				if err != nil {
					return err
				}
				for _, o := range offers {
					b.Offers = append(b.Offers, ShowcaseOffer{Code: o.Code, Name: o.Name, Description: o.Description,
						Surcharge: o.SurchargeAmount, Color: o.Color, Perks: o.Perks, CompanyName: o.CompanyName})
				}
				tiers, err := q.ShowcaseLoyalty(ctx)
				if err != nil {
					return err
				}
				for _, t := range tiers {
					b.Tiers = append(b.Tiers, ShowcaseTier{Code: t.Code, Name: t.Name, MinTrips: t.MinTrips,
						DiscountPercent: t.DiscountPercent, Perks: t.Perks, CompanyName: t.CompanyName,
						CancellationFeePercent: t.CancellationFeePercent, CancellationCutoffHours: t.CancellationCutoffHours, FreeChange: t.FreeChange})
					if b.Loyalty == nil {
						b.Loyalty = &ShowcaseLoyalty{PointsPer100: t.PointsPer100, PointValue: t.PointValue, SignupBonusPoints: t.SignupBonusPoints}
					}
				}
				policies, err := q.ShowcasePolicies(ctx)
				for _, p := range policies {
					b.Policies = append(b.Policies, ShowcasePolicy{CompanyName: p.Name,
						CancellationCutoffHours: p.CancellationCutoffHours, CancellationFeePercent: p.CancellationFeePercent,
						ChangeEnabled: p.ChangeEnabled, ChangeCutoffHours: p.ChangeCutoffHours, ChangeFeeAmount: p.ChangeFeeAmount,
						ChangeMaxCount: p.ChangeMaxCount, ChangeWindowDays: p.ChangeWindowDays})
				}
				return err
			})
			return out, apix.Err(err)
		})
}

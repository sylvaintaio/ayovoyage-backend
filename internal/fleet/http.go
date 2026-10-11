// Package fleet gère les catégories de sièges (VIP/Premium/Standard),
// les plans de sièges, les bus et les lignes.
package fleet

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const tagFleet = "Admin — Flotte et lignes"

type Handler struct {
	DB *database.DB
}

// Note multi-tenant : les clés étrangères PostgreSQL ne sont pas soumises au RLS.
// Toute référence (agence, plan, catégorie…) reçue du client est donc vérifiée
// explicitement comme appartenant à la compagnie de l'utilisateur.

// ---------------------------------------------------------------- DTO

type SeatCategory struct {
	ID              uuid.UUID `json:"id"`
	Code            string    `json:"code" example:"vip"`
	Name            string    `json:"name" example:"VIP"`
	Description     string    `json:"description"`
	SurchargeAmount int64     `json:"surcharge_amount" doc:"Supplément par rapport au tarif de base (XOF)"`
	Color           string    `json:"color" example:"#b45309"`
	Perks           []string  `json:"perks" example:"[\"Embarquement prioritaire\",\"Bouteille d'eau\"]"`
	SortOrder       int32     `json:"sort_order"`
}

func ToSeatCategory(c store.SeatCategory) SeatCategory {
	return SeatCategory{ID: c.ID, Code: c.Code, Name: c.Name, Description: c.Description, SurchargeAmount: c.SurchargeAmount,
		Color: c.Color, Perks: c.Perks, SortOrder: c.SortOrder}
}

type SeatCategoryInput struct {
	Code            string   `json:"code" pattern:"^[a-z0-9_]{2,30}$" doc:"Non modifiable après création"`
	Name            string   `json:"name" minLength:"2" maxLength:"60"`
	Description     string   `json:"description,omitempty" maxLength:"500"`
	SurchargeAmount int64    `json:"surcharge_amount" minimum:"0"`
	Color           string   `json:"color,omitempty" pattern:"^#[0-9a-fA-F]{6}$" default:"#64748b"`
	Perks           []string `json:"perks,omitempty" maxItems:"20"`
	SortOrder       int32    `json:"sort_order,omitempty"`
}

type LayoutCell struct {
	Row            int32      `json:"row" minimum:"0"`
	Col            int32      `json:"col" minimum:"0"`
	Kind           string     `json:"kind" enum:"seat,aisle,driver,door,toilet,empty"`
	SeatLabel      string     `json:"seat_label,omitempty" maxLength:"6" example:"1A"`
	CategoryID     *uuid.UUID `json:"category_id,omitempty" doc:"Obligatoire pour un siège"`
	ExtraSurcharge int64      `json:"extra_surcharge,omitempty" minimum:"0" doc:"Supplément propre à ce siège"`
	Attributes     []string   `json:"attributes,omitempty" enum:"window,aisle,family,accessible,front,legroom"`
}

type SeatLayout struct {
	ID        uuid.UUID    `json:"id"`
	Name      string       `json:"name"`
	Rows      int32        `json:"rows"`
	Cols      int32        `json:"cols"`
	SeatCount int32        `json:"seat_count"`
	Cells     []LayoutCell `json:"cells,omitempty"`
	InUse     bool         `json:"in_use" doc:"Utilisé par au moins un départ (structure figée)"`
}

type SeatLayoutInput struct {
	Name  string       `json:"name" minLength:"2" maxLength:"80" example:"Yutong 70 places 2+2"`
	Rows  int32        `json:"rows" minimum:"1" maximum:"40"`
	Cols  int32        `json:"cols" minimum:"1" maximum:"8"`
	Cells []LayoutCell `json:"cells" minItems:"1" maxItems:"320" doc:"Cases non vides du plan (les cases absentes sont vides)"`
}

type Bus struct {
	ID           uuid.UUID  `json:"id"`
	PlateNumber  string     `json:"plate_number"`
	InternalCode string     `json:"internal_code"`
	Model        string     `json:"model"`
	LayoutID     uuid.UUID  `json:"layout_id"`
	LayoutName   string     `json:"layout_name,omitempty"`
	SeatCount    int32      `json:"seat_count,omitempty"`
	Amenities    []string   `json:"amenities"`
	Status       string     `json:"status" enum:"active,maintenance,out_of_service"`
	HomeAgencyID *uuid.UUID `json:"home_agency_id,omitempty"`
	Notes        string     `json:"notes"`
}

type BusInput struct {
	PlateNumber  string     `json:"plate_number" minLength:"4" maxLength:"20"`
	InternalCode string     `json:"internal_code,omitempty" maxLength:"30"`
	Model        string     `json:"model,omitempty" maxLength:"80"`
	LayoutID     uuid.UUID  `json:"layout_id"`
	Amenities    []string   `json:"amenities,omitempty" maxItems:"20" example:"[\"climatisation\",\"wifi\",\"usb\"]"`
	Status       string     `json:"status,omitempty" enum:"active,maintenance,out_of_service" default:"active"`
	HomeAgencyID *uuid.UUID `json:"home_agency_id,omitempty"`
	Notes        string     `json:"notes,omitempty" maxLength:"2000"`
}

func toBus(b store.Bus) Bus {
	return Bus{ID: b.ID, PlateNumber: b.PlateNumber, InternalCode: b.InternalCode, Model: b.Model, LayoutID: b.LayoutID,
		Amenities: b.Amenities, Status: b.Status, HomeAgencyID: b.HomeAgencyID, Notes: b.Notes}
}

type Route struct {
	ID                    uuid.UUID   `json:"id"`
	OriginAgencyID        uuid.UUID   `json:"origin_agency_id" doc:"Agence du premier arrêt"`
	OriginAgencyName      string      `json:"origin_agency_name,omitempty"`
	OriginCity            string      `json:"origin_city,omitempty"`
	DestinationAgencyID   *uuid.UUID  `json:"destination_agency_id,omitempty" doc:"Agence du dernier arrêt (absente si la ligne finit hors agence)"`
	DestinationAgencyName string      `json:"destination_agency_name,omitempty" doc:"Nom du dernier arrêt"`
	DestinationCity       string      `json:"destination_city,omitempty"`
	Name                  string      `json:"name"`
	BasePrice             int64       `json:"base_price" doc:"Prix complet, Standard (XOF)"`
	PricingMode           string      `json:"pricing_mode" enum:"flat,grid" doc:"flat : prix unique quelle que soit la descente ; grid : grille de ville à ville"`
	DurationMinutes       int32       `json:"duration_minutes"`
	DistanceKm            *int32      `json:"distance_km,omitempty"`
	IncludedBaggageKg     int32       `json:"included_baggage_kg"`
	IsActive              bool        `json:"is_active"`
	Stops                 []RouteStop `json:"stops"`
	Fares                 []RouteFare `json:"fares"`
}

func toRoute(r store.Route) Route {
	return Route{ID: r.ID, OriginAgencyID: r.OriginAgencyID, DestinationAgencyID: r.DestinationAgencyID, Name: r.Name,
		BasePrice: r.BasePrice, PricingMode: r.PricingMode, DurationMinutes: r.DurationMinutes, DistanceKm: r.DistanceKm,
		IncludedBaggageKg: r.IncludedBaggageKg, IsActive: r.IsActive, Stops: []RouteStop{}, Fares: []RouteFare{}}
}

// withItinerary complète une ligne avec ses arrêts et sa grille.
func withItinerary(ctx context.Context, q *store.Queries, rt Route) (Route, error) {
	stops, fares, err := loadItinerary(ctx, q, rt.ID)
	if err != nil {
		return rt, err
	}
	rt.Stops, rt.Fares = stops, fares
	if n := len(stops); n > 0 {
		rt.OriginCity, rt.DestinationCity = stops[0].CityName, stops[n-1].CityName
		rt.OriginAgencyName, rt.DestinationAgencyName = stops[0].StopName, stops[n-1].StopName
	}
	return rt, nil
}

type RouteInput struct {
	Name              string           `json:"name" minLength:"2" maxLength:"120" example:"Cotonou → Parakou"`
	BasePrice         int64            `json:"base_price" minimum:"1" doc:"Prix complet, Standard (XOF)"`
	PricingMode       string           `json:"pricing_mode,omitempty" enum:"flat,grid" default:"flat"`
	Stops             []RouteStopInput `json:"stops,omitempty" maxItems:"40" doc:"Arrêts dans l'ordre de passage (au moins 2)"`
	Fares             []RouteFare      `json:"fares,omitempty" maxItems:"800" doc:"Grille (mode grid) : cases absentes = prix complet"`
	DistanceKm        *int32           `json:"distance_km,omitempty" minimum:"1"`
	IncludedBaggageKg int32            `json:"included_baggage_kg" minimum:"0" maximum:"200" doc:"Bagage inclus (kg), ex. 20"`
	IsActive          bool             `json:"is_active"`
	// Ancienne forme (2 agences), acceptée tant que des clients l'utilisent : ignorée si stops est fourni.
	OriginAgencyID      *uuid.UUID `json:"origin_agency_id,omitempty" doc:"Ancienne forme : agence de départ"`
	DestinationAgencyID *uuid.UUID `json:"destination_agency_id,omitempty" doc:"Ancienne forme : agence d'arrivée"`
	DurationMinutes     int32      `json:"duration_minutes,omitempty" minimum:"0" maximum:"4320" doc:"Ancienne forme : durée"`
}

// stopsInput : arrêts fournis, sinon ancienne forme convertie en 2 arrêts.
func (b RouteInput) stopsInput(ctx context.Context, q *store.Queries, companyID uuid.UUID) ([]RouteStopInput, error) {
	if len(b.Stops) > 0 {
		return b.Stops, nil
	}
	if b.OriginAgencyID == nil || b.DestinationAgencyID == nil || b.DurationMinutes <= 0 {
		return nil, huma.Error422UnprocessableEntity("Indiquez les arrêts de la ligne")
	}
	if *b.OriginAgencyID == *b.DestinationAgencyID {
		return nil, huma.Error422UnprocessableEntity("L'agence de départ et l'agence d'arrivée doivent être différentes")
	}
	for _, id := range []*uuid.UUID{b.OriginAgencyID, b.DestinationAgencyID} {
		if err := ensureAgency(ctx, q, companyID, id); err != nil {
			return nil, err
		}
	}
	return legacyStops(ctx, q, *b.OriginAgencyID, *b.DestinationAgencyID, b.DurationMinutes)
}

// ---------------------------------------------------------------- Validation

func validateLayout(in SeatLayoutInput, categories map[uuid.UUID]bool) (int32, error) {
	seen := map[[2]int32]bool{}
	labels := map[string]bool{}
	var seats int32
	for i, c := range in.Cells {
		if c.Row >= in.Rows || c.Col >= in.Cols {
			return 0, huma.Error422UnprocessableEntity(fmt.Sprintf("Case %d hors du plan (%d×%d)", i, in.Rows, in.Cols))
		}
		key := [2]int32{c.Row, c.Col}
		if seen[key] {
			return 0, huma.Error422UnprocessableEntity(fmt.Sprintf("Case en double : rangée %d, colonne %d", c.Row, c.Col))
		}
		seen[key] = true
		if c.Kind != "seat" {
			continue
		}
		if c.SeatLabel == "" || c.CategoryID == nil {
			return 0, huma.Error422UnprocessableEntity(fmt.Sprintf("Le siège en %d/%d doit avoir un numéro et une catégorie", c.Row, c.Col))
		}
		if !categories[*c.CategoryID] {
			return 0, huma.Error422UnprocessableEntity("Catégorie de siège inconnue : " + c.CategoryID.String())
		}
		if labels[c.SeatLabel] {
			return 0, huma.Error422UnprocessableEntity("Numéro de siège en double : " + c.SeatLabel)
		}
		labels[c.SeatLabel] = true
		seats++
	}
	if seats == 0 {
		return 0, huma.Error422UnprocessableEntity("Le plan doit contenir au moins un siège")
	}
	return seats, nil
}

func insertCells(ctx context.Context, q *store.Queries, companyID, layoutID uuid.UUID, cells []LayoutCell) error {
	for _, c := range cells {
		if c.Kind == "empty" {
			continue
		}
		p := store.InsertSeatLayoutCellParams{
			CompanyID: companyID, LayoutID: layoutID, RowIndex: c.Row, ColIndex: c.Col, Kind: c.Kind,
			ExtraSurcharge: c.ExtraSurcharge, Attributes: []string{},
		}
		if c.Kind == "seat" {
			p.SeatLabel, p.CategoryID = &c.SeatLabel, c.CategoryID
			if c.Attributes != nil {
				p.Attributes = c.Attributes
			}
		}
		if err := q.InsertSeatLayoutCell(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func categorySet(ctx context.Context, q *store.Queries, companyID uuid.UUID) (map[uuid.UUID]bool, error) {
	cats, err := q.ListSeatCategories(ctx, companyID)
	m := make(map[uuid.UUID]bool, len(cats))
	for _, c := range cats {
		m[c.ID] = true
	}
	return m, err
}

func ensureAgency(ctx context.Context, q *store.Queries, companyID uuid.UUID, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *id, CompanyID: companyID}); err != nil {
		if database.IsNotFound(err) {
			return huma.Error422UnprocessableEntity("Agence inconnue : " + id.String())
		}
		return err
	}
	return nil
}

func ensureLayout(ctx context.Context, q *store.Queries, companyID, id uuid.UUID) error {
	if _, err := q.GetSeatLayout(ctx, store.GetSeatLayoutParams{ID: id, CompanyID: companyID}); err != nil {
		if database.IsNotFound(err) {
			return huma.Error422UnprocessableEntity("Plan de sièges inconnu")
		}
		return err
	}
	return nil
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	admin := []string{auth.RoleCompanyAdmin}

	// --- Catégories de sièges
	huma.Register(api, apix.Op("list-seat-categories", http.MethodGet, "/v1/admin/seat-categories", tagFleet, "Lister les catégories de sièges", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []SeatCategory }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []SeatCategory }{Body: []SeatCategory{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListSeatCategories(ctx, p.CompanyID)
				for _, r := range rows {
					out.Body = append(out.Body, ToSeatCategory(r))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("create-seat-category", http.MethodPost, "/v1/admin/seat-categories", tagFleet, "Créer une catégorie de siège", apix.Staff, admin...),
		func(ctx context.Context, in *struct{ Body SeatCategoryInput }) (*struct{ Body SeatCategory }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body SeatCategory }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				c, err := q.CreateSeatCategory(ctx, store.CreateSeatCategoryParams{
					CompanyID: p.CompanyID, Code: b.Code, Name: b.Name, Description: b.Description,
					SurchargeAmount: b.SurchargeAmount, Color: b.Color, Perks: emptyIfNil(b.Perks), SortOrder: b.SortOrder,
				})
				if err != nil {
					return err
				}
				out.Body = ToSeatCategory(c)
				return apix.Audit(ctx, q, &p.CompanyID, "seat_category.created", "seat_category", c.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("update-seat-category", http.MethodPut, "/v1/admin/seat-categories/{id}", tagFleet, "Modifier une catégorie (tarif, avantages)", apix.Staff, admin...),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body SeatCategoryInput
		}) (*struct{ Body SeatCategory }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body SeatCategory }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetSeatCategory(ctx, store.GetSeatCategoryParams{ID: in.ID, CompanyID: p.CompanyID})
				if err != nil {
					return err
				}
				c, err := q.UpdateSeatCategory(ctx, store.UpdateSeatCategoryParams{
					ID: in.ID, CompanyID: p.CompanyID, Name: b.Name, Description: b.Description,
					SurchargeAmount: b.SurchargeAmount, Color: b.Color, Perks: emptyIfNil(b.Perks), SortOrder: b.SortOrder,
				})
				if err != nil {
					return err
				}
				out.Body = ToSeatCategory(c)
				return apix.Audit(ctx, q, &p.CompanyID, "seat_category.updated", "seat_category", c.ID.String(), ToSeatCategory(before), out.Body)
			})
			return out, apix.Err(err)
		})

	// --- Plans de sièges
	huma.Register(api, apix.Op("list-seat-layouts", http.MethodGet, "/v1/admin/seat-layouts", tagFleet, "Lister les plans de sièges", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []SeatLayout }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []SeatLayout }{Body: []SeatLayout{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListSeatLayouts(ctx, p.CompanyID)
				for _, l := range rows {
					out.Body = append(out.Body, SeatLayout{ID: l.ID, Name: l.Name, Rows: l.Rows, Cols: l.Cols, SeatCount: l.SeatCount})
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-seat-layout", http.MethodGet, "/v1/admin/seat-layouts/{id}", tagFleet, "Détail d'un plan de sièges", apix.Staff),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body SeatLayout }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body SeatLayout }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = loadLayout(ctx, q, p.CompanyID, in.ID)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-seat-layout", http.MethodPost, "/v1/admin/seat-layouts", tagFleet,
		"Créer un plan de sièges", apix.Staff, admin...),
		"Le plan est une grille. Chaque case non vide est un siège, une allée, le poste chauffeur, une porte ou des toilettes. "+
			"Chaque siège a un numéro, une catégorie (VIP/Premium/Standard) et éventuellement un supplément propre."),
		func(ctx context.Context, in *struct{ Body SeatLayoutInput }) (*struct{ Body SeatLayout }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body SeatLayout }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				cats, err := categorySet(ctx, q, p.CompanyID)
				if err != nil {
					return err
				}
				seats, err := validateLayout(in.Body, cats)
				if err != nil {
					return err
				}
				l, err := q.CreateSeatLayout(ctx, store.CreateSeatLayoutParams{
					CompanyID: p.CompanyID, Name: in.Body.Name, Rows: in.Body.Rows, Cols: in.Body.Cols, SeatCount: seats,
				})
				if err != nil {
					return err
				}
				if err := insertCells(ctx, q, p.CompanyID, l.ID, in.Body.Cells); err != nil {
					return err
				}
				out.Body, err = loadLayout(ctx, q, p.CompanyID, l.ID)
				if err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "seat_layout.created", "seat_layout", l.ID.String(), nil,
					map[string]any{"name": l.Name, "seat_count": seats})
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("update-seat-layout", http.MethodPut, "/v1/admin/seat-layouts/{id}", tagFleet,
		"Modifier un plan de sièges", apix.Staff, admin...),
		"Refusé (409) si le plan est déjà utilisé par un départ : créez alors une nouvelle version du plan."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body SeatLayoutInput
		}) (*struct{ Body SeatLayout }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body SeatLayout }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if err := ensureLayout(ctx, q, p.CompanyID, in.ID); err != nil {
					return err
				}
				inUse, err := q.IsSeatLayoutInUse(ctx, in.ID)
				if err != nil {
					return err
				}
				if inUse {
					return huma.Error409Conflict("Ce plan est utilisé par des départs : créez une nouvelle version")
				}
				cats, err := categorySet(ctx, q, p.CompanyID)
				if err != nil {
					return err
				}
				seats, err := validateLayout(in.Body, cats)
				if err != nil {
					return err
				}
				if _, err := q.UpdateSeatLayout(ctx, store.UpdateSeatLayoutParams{
					ID: in.ID, CompanyID: p.CompanyID, Name: in.Body.Name, Rows: in.Body.Rows, Cols: in.Body.Cols, SeatCount: seats,
				}); err != nil {
					return err
				}
				if err := q.DeleteSeatLayoutCells(ctx, in.ID); err != nil {
					return err
				}
				if err := insertCells(ctx, q, p.CompanyID, in.ID, in.Body.Cells); err != nil {
					return err
				}
				out.Body, err = loadLayout(ctx, q, p.CompanyID, in.ID)
				if err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "seat_layout.updated", "seat_layout", in.ID.String(), nil,
					map[string]any{"name": in.Body.Name, "seat_count": seats})
			})
			return out, apix.Err(err)
		})

	// --- Bus
	huma.Register(api, apix.Op("list-buses", http.MethodGet, "/v1/admin/buses", tagFleet, "Lister les bus", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Bus }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Bus }{Body: []Bus{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListBuses(ctx, p.CompanyID)
				for _, r := range rows {
					b := toBus(r.Bus)
					b.LayoutName, b.SeatCount = r.LayoutName, r.SeatCount
					out.Body = append(out.Body, b)
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("create-bus", http.MethodPost, "/v1/admin/buses", tagFleet, "Ajouter un bus", apix.Staff, admin...),
		func(ctx context.Context, in *struct{ Body BusInput }) (*struct{ Body Bus }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body Bus }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if err := ensureLayout(ctx, q, p.CompanyID, b.LayoutID); err != nil {
					return err
				}
				if err := ensureAgency(ctx, q, p.CompanyID, b.HomeAgencyID); err != nil {
					return err
				}
				bus, err := q.CreateBus(ctx, store.CreateBusParams{
					CompanyID: p.CompanyID, PlateNumber: b.PlateNumber, InternalCode: b.InternalCode, Model: b.Model,
					LayoutID: b.LayoutID, Amenities: emptyIfNil(b.Amenities), Status: b.Status, HomeAgencyID: b.HomeAgencyID, Notes: b.Notes,
				})
				if err != nil {
					return err
				}
				out.Body = toBus(bus)
				return apix.Audit(ctx, q, &p.CompanyID, "bus.created", "bus", bus.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("update-bus", http.MethodPut, "/v1/admin/buses/{id}", tagFleet, "Modifier un bus (statut, maintenance…)", apix.Staff, admin...),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body BusInput
		}) (*struct{ Body Bus }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body Bus }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetBus(ctx, store.GetBusParams{ID: in.ID, CompanyID: p.CompanyID})
				if err != nil {
					return err
				}
				if err := ensureLayout(ctx, q, p.CompanyID, b.LayoutID); err != nil {
					return err
				}
				if err := ensureAgency(ctx, q, p.CompanyID, b.HomeAgencyID); err != nil {
					return err
				}
				bus, err := q.UpdateBus(ctx, store.UpdateBusParams{
					ID: in.ID, CompanyID: p.CompanyID, PlateNumber: b.PlateNumber, InternalCode: b.InternalCode, Model: b.Model,
					LayoutID: b.LayoutID, Amenities: emptyIfNil(b.Amenities), Status: b.Status, HomeAgencyID: b.HomeAgencyID, Notes: b.Notes,
				})
				if err != nil {
					return err
				}
				out.Body = toBus(bus)
				return apix.Audit(ctx, q, &p.CompanyID, "bus.updated", "bus", bus.ID.String(), toBus(before), out.Body)
			})
			return out, apix.Err(err)
		})

	// --- Lignes
	huma.Register(api, apix.Op("list-routes", http.MethodGet, "/v1/admin/routes", tagFleet, "Lister les lignes", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Route }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Route }{Body: []Route{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListRoutes(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				for _, r := range rows {
					rt, err := withItinerary(ctx, q, toRoute(r))
					if err != nil {
						return err
					}
					out.Body = append(out.Body, rt)
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("create-route", http.MethodPost, "/v1/admin/routes", tagFleet, "Créer une ligne", apix.Staff, admin...),
		func(ctx context.Context, in *struct{ Body RouteInput }) (*struct{ Body Route }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body Route }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				stops, err := b.stopsInput(ctx, q, p.CompanyID)
				if err != nil {
					return err
				}
				first, err := q.GetStopPoint(ctx, store.GetStopPointParams{ID: stops[0].StopPointID, CompanyID: p.CompanyID})
				if err != nil || first.AgencyID == nil {
					return huma.Error422UnprocessableEntity("Le premier arrêt doit être une agence où l'on monte")
				}
				r, err := q.CreateRoute(ctx, store.CreateRouteParams{
					CompanyID: p.CompanyID, OriginAgencyID: *first.AgencyID, Name: b.Name, BasePrice: b.BasePrice,
					DurationMinutes: 1, DistanceKm: b.DistanceKm, IncludedBaggageKg: b.IncludedBaggageKg, IsActive: b.IsActive,
				})
				if err != nil {
					return err
				}
				if r, err = saveItinerary(ctx, q, p.CompanyID, r.ID, b.BasePrice, b.PricingMode, stops, b.Fares); err != nil {
					return err
				}
				if out.Body, err = withItinerary(ctx, q, toRoute(r)); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "route.created", "route", r.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("update-route", http.MethodPut, "/v1/admin/routes/{id}", tagFleet,
		"Modifier une ligne (arrêts, tarifs, grille…)", apix.Staff, admin...),
		"Les nouveaux arrêts, tarifs et grille s'appliquent aux départs créés ensuite : les départs existants "+
			"gardent leurs arrêts et les billets vendus ne changent pas. Sans `stops`, les arrêts sont conservés."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body RouteInput
		}) (*struct{ Body Route }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body Route }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetRoute(ctx, store.GetRouteParams{ID: in.ID, CompanyID: p.CompanyID})
				if err != nil {
					return err
				}
				prev, err := withItinerary(ctx, q, toRoute(before))
				if err != nil {
					return err
				}
				r, err := q.UpdateRoute(ctx, store.UpdateRouteParams{
					ID: in.ID, CompanyID: p.CompanyID, Name: b.Name, BasePrice: b.BasePrice, DurationMinutes: before.DurationMinutes,
					DistanceKm: b.DistanceKm, IncludedBaggageKg: b.IncludedBaggageKg, IsActive: b.IsActive,
				})
				if err != nil {
					return err
				}
				// Arrêts fournis : remplacés. Sinon conservés (la grille et le mode peuvent changer seuls).
				stops := b.Stops
				if len(stops) == 0 {
					for _, s := range prev.Stops {
						stops = append(stops, RouteStopInput{StopPointID: s.StopPointID, OffsetMinutes: s.OffsetMinutes,
							PauseMinutes: s.PauseMinutes, CanBoard: s.CanBoard, CanAlight: s.CanAlight})
					}
				}
				mode, fares := b.PricingMode, b.Fares
				if mode == "" {
					mode, fares = before.PricingMode, prev.Fares
				}
				if r, err = saveItinerary(ctx, q, p.CompanyID, r.ID, b.BasePrice, mode, stops, fares); err != nil {
					return err
				}
				if out.Body, err = withItinerary(ctx, q, toRoute(r)); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "route.updated", "route", r.ID.String(), prev, out.Body)
			})
			return out, apix.Err(err)
		})
}

func loadLayout(ctx context.Context, q *store.Queries, companyID, id uuid.UUID) (SeatLayout, error) {
	l, err := q.GetSeatLayout(ctx, store.GetSeatLayoutParams{ID: id, CompanyID: companyID})
	if err != nil {
		return SeatLayout{}, err
	}
	cells, err := q.ListSeatLayoutCells(ctx, id)
	if err != nil {
		return SeatLayout{}, err
	}
	inUse, err := q.IsSeatLayoutInUse(ctx, id)
	if err != nil {
		return SeatLayout{}, err
	}
	out := SeatLayout{ID: l.ID, Name: l.Name, Rows: l.Rows, Cols: l.Cols, SeatCount: l.SeatCount, InUse: inUse, Cells: []LayoutCell{}}
	for _, c := range cells {
		lc := LayoutCell{Row: c.RowIndex, Col: c.ColIndex, Kind: c.Kind, CategoryID: c.CategoryID,
			ExtraSurcharge: c.ExtraSurcharge, Attributes: c.Attributes}
		if c.SeatLabel != nil {
			lc.SeatLabel = *c.SeatLabel
		}
		out.Cells = append(out.Cells, lc)
	}
	return out, nil
}

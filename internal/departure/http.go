// Package departure gère les départs : recherche publique, plan de sièges
// en temps réel, programmation et exploitation (statuts, sièges bloqués).
package departure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/booking"
	"busvoyage/internal/fleet"
	"busvoyage/internal/itinerary"
	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/pricing"
	"busvoyage/internal/segment"
	"busvoyage/internal/store"
)

const (
	tagPublic = "Public — Recherche de départs"
	tagAdmin  = "Admin — Départs"
)

// Heure locale du Bénin (UTC+1, sans heure d'été) pour interpréter les dates saisies.
var LocalTZ = time.FixedZone("WAT", 3600)

type Handler struct {
	DB       *database.DB
	Bookings *booking.Service
	Queue    *jobs.Queue
	Parcels  ParcelHook
}

// ParcelHook : colis et bagages suivent le statut du départ (parti, arrivé, annulé).
type ParcelHook interface {
	OnDepartureStatus(ctx context.Context, q *store.Queries, departureID uuid.UUID, status string) error
	// Journal des arrêts : départ d'une agence (colis chargés là partent), arrivée à une agence (déchargement).
	OnStopDeparture(ctx context.Context, q *store.Queries, departureID, agencyID uuid.UUID) error
	OnStopArrival(ctx context.Context, q *store.Queries, departureID, agencyID uuid.UUID) error
}

// ---------------------------------------------------------------- DTO

// SearchResult : un départ vu par le voyageur, pour son trajet (arrêt de montée → arrêt de descente).
// « origin » = arrêt de montée, « destination » = arrêt de descente ; les heures et le prix sont
// ceux de ce trajet (un voyageur qui monte à Bohicon voit l'heure de passage à Bohicon).
type SearchResult struct {
	ID                    uuid.UUID       `json:"id"`
	CompanyID             uuid.UUID       `json:"company_id"`
	CompanyName           string          `json:"company_name"`
	DepartureAt           time.Time       `json:"departure_at" doc:"Heure prévue à l'arrêt de montée"`
	ArrivalEta            time.Time       `json:"arrival_eta" doc:"Heure prévue à l'arrêt de descente"`
	DurationMinutes       int32           `json:"duration_minutes" doc:"Durée du trajet du voyageur"`
	BasePrice             int64           `json:"base_price" doc:"Prix Standard de ce trajet (XOF)"`
	Status                string          `json:"status"`
	RouteName             string          `json:"route_name"`
	OriginCity            string          `json:"origin_city" doc:"Ville de montée"`
	OriginAgencyID        uuid.UUID       `json:"origin_agency_id" doc:"Agence de montée"`
	OriginAgencyName      string          `json:"origin_agency_name"`
	DestinationCity       string          `json:"destination_city" doc:"Ville de descente"`
	DestinationAgencyID   *uuid.UUID      `json:"destination_agency_id,omitempty" doc:"Agence de descente, si l'arrêt en est une"`
	DestinationAgencyName string          `json:"destination_agency_name" doc:"Arrêt de descente"`
	BoardingSeq           int             `json:"boarding_seq" doc:"Indice de l'arrêt de montée (à renvoyer à la réservation)"`
	AlightingSeq          int             `json:"alighting_seq" doc:"Indice de l'arrêt de descente"`
	BusDepartureAt        time.Time       `json:"bus_departure_at" doc:"Heure de départ du car (premier arrêt)"`
	BusModel              string          `json:"bus_model"`
	BusAmenities          []string        `json:"bus_amenities"`
	SeatCount             int32           `json:"seat_count"`
	AvailableSeats        int32           `json:"available_seats"`
	IncludedBaggageKg     int32           `json:"included_baggage_kg"`
	EarlyBird             *EarlyBirdOffer `json:"early_bird,omitempty" doc:"Tarif « réservez tôt » en cours sur ce départ"`
}

// EarlyBirdOffer : remise « réservez tôt » et places restantes à ce tarif.
type EarlyBirdOffer struct {
	Label     string `json:"label"`
	Percent   int32  `json:"percent"`
	SeatsLeft int    `json:"seats_left"`
}

// earlyBirds : règles « réservez tôt » chargées une fois par compagnie pendant une requête.
type earlyBirds struct {
	q     *store.Queries
	rules map[uuid.UUID][]store.PricingRule
}

func (e *earlyBirds) offer(ctx context.Context, companyID, routeID, departureID uuid.UUID, at time.Time) (*EarlyBirdOffer, error) {
	rules, ok := e.rules[companyID]
	if !ok {
		var err error
		if rules, err = e.q.ListActivePricingRules(ctx, companyID); err != nil {
			return nil, err
		}
		e.rules[companyID] = rules
	}
	r := pricing.MatchEarlyBird(rules, routeID, at, time.Now())
	if r == nil {
		return nil, nil
	}
	used, err := e.q.CountEarlyBirdSeats(ctx, departureID)
	if err != nil {
		return nil, err
	}
	left := int(r.SeatQuota) - int(used)
	if left <= 0 {
		return nil, nil
	}
	return &EarlyBirdOffer{Label: r.Label, Percent: r.Percent, SeatsLeft: left}, nil
}

type CategoryPrice struct {
	fleet.SeatCategory
	Price     int64 `json:"price" doc:"Tarif de base + supplément de la catégorie"`
	Available int32 `json:"available" doc:"Sièges libres dans cette catégorie"`
}

type SeatCell struct {
	ID            uuid.UUID  `json:"id"`
	Row           int32      `json:"row"`
	Col           int32      `json:"col"`
	Kind          string     `json:"kind" enum:"seat,aisle,driver,door,toilet,empty"`
	SeatLabel     string     `json:"seat_label,omitempty"`
	CategoryID    *uuid.UUID `json:"category_id,omitempty"`
	Price         int64      `json:"price,omitempty" doc:"Prix total du siège (hors réduction de fidélité)"`
	Attributes    []string   `json:"attributes,omitempty"`
	State         string     `json:"state" enum:"none,available,held,sold,boarded,blocked" doc:"none = case qui n'est pas un siège"`
	BookingID     *uuid.UUID `json:"booking_id,omitempty" doc:"Personnel uniquement"`
	PassengerName *string    `json:"passenger_name,omitempty" doc:"Personnel uniquement"`
}

type SeatMap struct {
	Rows  int32      `json:"rows"`
	Cols  int32      `json:"cols"`
	Cells []SeatCell `json:"cells"`
}

type DepartureDetail struct {
	SearchResult
	OriginAgencyAddress string          `json:"origin_agency_address" doc:"Adresse de l'arrêt de montée"`
	Stops               []DepartureStop `json:"stops" doc:"Arrêts du départ : montée possible si can_board, descente si can_alight"`
	Categories          []CategoryPrice `json:"categories"`
	SeatMap             SeatMap         `json:"seat_map" doc:"États des sièges sur le trajet demandé"`
}

type AdminDeparture struct {
	ID           uuid.UUID  `json:"id"`
	RouteID      uuid.UUID  `json:"route_id"`
	RouteName    string     `json:"route_name"`
	BusID        uuid.UUID  `json:"bus_id"`
	PlateNumber  string     `json:"plate_number"`
	DepartureAt  time.Time  `json:"departure_at"`
	ArrivalEta   time.Time  `json:"arrival_eta"`
	BasePrice    int64      `json:"base_price"`
	Status       string     `json:"status" enum:"scheduled,boarding,departed,arrived,cancelled"`
	DriverID     *uuid.UUID `json:"driver_id,omitempty"`
	AgentID      *uuid.UUID `json:"agent_id,omitempty"`
	Notes        string     `json:"notes"`
	SeatCount    int32      `json:"seat_count"`
	SoldSeats    int32      `json:"sold_seats"`
	BoardedSeats int32      `json:"boarded_seats"`
	PeakSeats    int32      `json:"peak_seats" doc:"Charge maximale sur un tronçon : places libres = seat_count - peak_seats (sold_seats compte les passagers, revente comprise)"`
	// Fiche du départ uniquement :
	Stops  []DepartureStop `json:"stops,omitempty" doc:"Arrêts et heures prévues"`
	Crew   []CrewMember    `json:"crew,omitempty" doc:"Contrôleur, chauffeur et autres membres"`
	Visits []StopVisitView `json:"visits,omitempty" doc:"Journal des arrêts (heures réelles, arrêts imprévus)"`
}

// ---------------------------------------------------------------- Plan de sièges

// LoadSeatMap construit le plan d'un départ avec l'état de chaque siège.
// includePassengers : informations nominatives réservées au personnel.
// fromSeq, toSeq : trajet considéré (un siège pris ailleurs sur la ligne est libre ici).
func LoadSeatMap(ctx context.Context, q *store.Queries, companyID, departureID, layoutID uuid.UUID, basePrice int64,
	fromSeq, toSeq int, includePassengers bool) (SeatMap, []CategoryPrice, error) {
	layout, err := q.GetSeatLayout(ctx, store.GetSeatLayoutParams{ID: layoutID, CompanyID: companyID})
	if err != nil {
		return SeatMap{}, nil, err
	}
	rows, err := q.GetDepartureSeatMap(ctx, store.GetDepartureSeatMapParams{DepartureID: departureID, LayoutID: layoutID,
		FromSeq: int32(fromSeq), ToSeq: int32(toSeq)})
	if err != nil {
		return SeatMap{}, nil, err
	}
	cats, err := q.ListSeatCategories(ctx, companyID)
	if err != nil {
		return SeatMap{}, nil, err
	}
	byID := map[uuid.UUID]store.SeatCategory{}
	available := map[uuid.UUID]int32{}
	for _, c := range cats {
		byID[c.ID] = c
	}
	sm := SeatMap{Rows: layout.Rows, Cols: layout.Cols, Cells: make([]SeatCell, 0, len(rows))}
	used := map[uuid.UUID]bool{}
	for _, r := range rows {
		cell := SeatCell{ID: r.ID, Row: r.RowIndex, Col: r.ColIndex, Kind: r.Kind, State: r.State}
		if r.Kind == "seat" && r.CategoryID != nil {
			cell.SeatLabel = *r.SeatLabel
			cell.CategoryID = r.CategoryID
			cell.Attributes = r.Attributes
			cell.Price = basePrice + byID[*r.CategoryID].SurchargeAmount + r.ExtraSurcharge
			used[*r.CategoryID] = true
			if r.State == "available" {
				available[*r.CategoryID]++
			}
		}
		if includePassengers && r.BookingID != "" {
			id, name := uuid.MustParse(r.BookingID), r.PassengerName
			cell.BookingID, cell.PassengerName = &id, &name
		}
		sm.Cells = append(sm.Cells, cell)
	}
	prices := []CategoryPrice{}
	for _, c := range cats {
		if used[c.ID] {
			prices = append(prices, CategoryPrice{SeatCategory: fleet.ToSeatCategory(c), Price: basePrice + c.SurchargeAmount, Available: available[c.ID]})
		}
	}
	return sm, prices, nil
}

// ---------------------------------------------------------------- Statuts

var transitions = map[string][]string{
	"scheduled": {"boarding", "departed", "cancelled"},
	"boarding":  {"departed", "cancelled"},
	"departed":  {"arrived"},
}

// ChangeStatus applique une transition de statut (personnel de la compagnie).
func (h *Handler) ChangeStatus(ctx context.Context, p auth.Principal, id uuid.UUID, status, reason string) (store.Departure, int, error) {
	var (
		d         store.Departure
		cancelled int
	)
	err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
		before, err := q.GetDeparture(ctx, id)
		if database.IsNotFound(err) {
			return apix.NotFound("Départ")
		}
		if err != nil {
			return err
		}
		allowed := false
		for _, s := range transitions[before.Status] {
			allowed = allowed || s == status
		}
		if !allowed {
			return huma.Error409Conflict("Transition impossible : " + before.Status + " → " + status)
		}
		d, err = q.SetDepartureStatus(ctx, store.SetDepartureStatusParams{ID: id, CompanyID: p.CompanyID, Status: status})
		if err != nil {
			return err
		}
		if status == "cancelled" {
			if reason == "" {
				reason = "Départ annulé par la compagnie"
			}
			if cancelled, err = h.Bookings.CancelDepartureBookings(ctx, tx, q, id, reason); err != nil {
				return err
			}
		}
		if h.Parcels != nil {
			if err := h.Parcels.OnDepartureStatus(ctx, q, id, status); err != nil {
				return err
			}
		}
		if status != "boarding" {
			if err := h.Queue.InsertTx(ctx, tx, jobs.DepartureStatusArgs{DepartureID: id, Status: status}); err != nil {
				return err
			}
		}
		return apix.Audit(ctx, q, &p.CompanyID, "departure.status_changed", "departure", id.String(),
			map[string]any{"status": before.Status}, map[string]any{"status": status, "reason": reason, "bookings_cancelled": cancelled})
	})
	return d, cancelled, err
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	huma.Register(api, apix.WithDescription(apix.Op("search-departures", http.MethodGet, "/v1/public/departures/search", tagPublic,
		"Rechercher des départs", apix.Public),
		"La date est interprétée à l'heure locale (Afrique de l'Ouest, UTC+1). Les départs dont la vente est close sont exclus."),
		func(ctx context.Context, in *struct {
			From      string    `query:"from" required:"true" doc:"Slug de la ville de départ" example:"cotonou"`
			To        string    `query:"to" required:"true" doc:"Slug de la ville d'arrivée" example:"parakou"`
			Date      string    `query:"date" required:"true" format:"date" example:"2026-10-12"`
			CompanyID uuid.UUID `query:"company_id"`
		}) (*struct{ Body []SearchResult }, error) {
			day, err := time.ParseInLocation(time.DateOnly, in.Date, LocalTZ)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("Date invalide (AAAA-MM-JJ)")
			}
			params := store.SearchDeparturesParams{FromCity: in.From, ToCity: in.To, FromTs: day, ToTs: day.AddDate(0, 0, 1)}
			if in.CompanyID != uuid.Nil {
				params.CompanyID = &in.CompanyID
			}
			out := &struct{ Body []SearchResult }{Body: []SearchResult{}}
			err = h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.SearchDepartures(ctx, params)
				if err != nil {
					return err
				}
				eb := &earlyBirds{q: q, rules: map[uuid.UUID][]store.PricingRule{}}
				prices := map[uuid.UUID]itinerary.Pricing{}
				for _, r := range rows {
					offer, err := eb.offer(ctx, r.CompanyID, r.RouteID, r.ID, r.DepartureAt)
					if err != nil {
						return err
					}
					pr, ok := prices[r.RouteID]
					if !ok {
						route, err := q.GetRoute(ctx, store.GetRouteParams{ID: r.RouteID, CompanyID: r.CompanyID})
						if err != nil {
							return err
						}
						if pr, err = segment.Pricing(ctx, q, route); err != nil {
							return err
						}
						prices[r.RouteID] = pr
					}
					out.Body = append(out.Body, SearchResult{ID: r.ID, CompanyID: r.CompanyID, CompanyName: r.CompanyName,
						DepartureAt: r.BoardingAt, ArrivalEta: r.AlightingAt, DurationMinutes: int32(r.AlightingAt.Sub(r.BoardingAt).Minutes()),
						BasePrice: itinerary.SegmentPrice(pr, r.BasePrice, r.BoardingCityID, r.AlightingCityID),
						Status:    r.Status, RouteName: r.RouteName, OriginCity: r.BoardingCity,
						OriginAgencyID: derefID(r.BoardingAgencyID), OriginAgencyName: r.BoardingStopName, DestinationCity: r.AlightingCity,
						DestinationAgencyName: r.AlightingStopName, BoardingSeq: int(r.BoardingSeq), AlightingSeq: int(r.AlightingSeq),
						BusDepartureAt: r.DepartureAt, BusModel: r.BusModel, BusAmenities: r.BusAmenities, SeatCount: r.SeatCount,
						AvailableSeats: max(r.AvailableSeats, 0), IncludedBaggageKg: r.IncludedBaggageKg, EarlyBird: offer})
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("get-public-departure", http.MethodGet, "/v1/public/departures/{id}", tagPublic,
		"Détail d'un départ avec plan de sièges et tarifs par catégorie", apix.Public),
		"États des sièges sur le trajet demandé : available, held (bloqué pendant un paiement), sold, boarded, blocked. "+
			"Le plan reflète en temps réel les ventes de tous les canaux (application, site, guichet). "+
			"Sans `boarding`/`alighting` : premier arrêt → dernier arrêt."),
		func(ctx context.Context, in *struct {
			ID        uuid.UUID `path:"id"`
			Boarding  int       `query:"boarding" default:"-1" doc:"Indice de l'arrêt de montée (boarding_seq de la recherche)"`
			Alighting int       `query:"alighting" default:"-1" doc:"Indice de l'arrêt de descente"`
		}) (*struct{ Body DepartureDetail }, error) {
			out := &struct{ Body DepartureDetail }{}
			err := h.DB.Tx(ctx, database.Public(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := q.GetDepartureDetail(ctx, in.ID)
				if database.IsNotFound(err) {
					return apix.NotFound("Départ")
				}
				if err != nil {
					return err
				}
				dep := store.Departure{ID: d.ID, CompanyID: d.CompanyID, RouteID: d.RouteID, BasePrice: d.BasePrice}
				trip, err := segment.Plan(ctx, q, dep, optSeq(in.Boarding), optSeq(in.Alighting), nil)
				if err != nil {
					return err
				}
				b, a := trip.BoardingStop(), trip.AlightingStop()
				out.Body.SearchResult = SearchResult{ID: d.ID, CompanyID: d.CompanyID, CompanyName: d.CompanyName,
					DepartureAt: b.ScheduledAt, ArrivalEta: a.ScheduledAt, DurationMinutes: int32(a.ScheduledAt.Sub(b.ScheduledAt).Minutes()),
					BasePrice: trip.UnitPrice, Status: d.Status, RouteName: d.RouteName, OriginCity: b.CityName,
					OriginAgencyID: derefID(b.AgencyID), OriginAgencyName: b.Name, DestinationCity: a.CityName,
					DestinationAgencyID: a.AgencyID, DestinationAgencyName: a.Name, BoardingSeq: trip.Boarding, AlightingSeq: trip.Alighting,
					BusDepartureAt: d.DepartureAt, BusModel: d.BusModel, BusAmenities: d.BusAmenities, SeatCount: d.SeatCount,
					IncludedBaggageKg: d.IncludedBaggageKg}
				out.Body.OriginAgencyAddress = b.Address
				if out.Body.Stops, err = LoadStops(ctx, q, d.ID); err != nil {
					return err
				}
				eb := &earlyBirds{q: q, rules: map[uuid.UUID][]store.PricingRule{}}
				if out.Body.EarlyBird, err = eb.offer(ctx, d.CompanyID, d.RouteID, d.ID, d.DepartureAt); err != nil {
					return err
				}
				out.Body.SeatMap, out.Body.Categories, err = LoadSeatMap(ctx, q, d.CompanyID, d.ID, d.LayoutID, trip.UnitPrice,
					trip.Boarding, trip.Alighting, false)
				for _, c := range out.Body.SeatMap.Cells {
					if c.State == "available" {
						out.Body.AvailableSeats++
					}
				}
				return err
			})
			return out, apix.Err(err)
		})

	h.registerAdmin(api)
}

func (h *Handler) registerAdmin(api huma.API) {
	planners := []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager}

	huma.Register(api, apix.Op("list-departures", http.MethodGet, "/v1/admin/departures", tagAdmin, "Lister les départs", apix.Staff),
		func(ctx context.Context, in *struct {
			From           string    `query:"from" required:"true" format:"date" doc:"Date de début (incluse)"`
			To             string    `query:"to" format:"date" doc:"Date de fin (incluse). Par défaut : date de début"`
			RouteID        uuid.UUID `query:"route_id"`
			OriginAgencyID uuid.UUID `query:"origin_agency_id"`
			Status         string    `query:"status" enum:"scheduled,boarding,departed,arrived,cancelled,"`
		}) (*struct{ Body []AdminDeparture }, error) {
			p := auth.MustFromContext(ctx)
			from, err := time.ParseInLocation(time.DateOnly, in.From, LocalTZ)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("Date de début invalide")
			}
			to := from
			if in.To != "" {
				if to, err = time.ParseInLocation(time.DateOnly, in.To, LocalTZ); err != nil {
					return nil, huma.Error422UnprocessableEntity("Date de fin invalide")
				}
			}
			if to.Sub(from) > 62*24*time.Hour || to.Before(from) {
				return nil, huma.Error422UnprocessableEntity("Période invalide (62 jours maximum)")
			}
			params := store.ListDeparturesParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to.AddDate(0, 0, 1), Status: apix.NilIfEmpty(in.Status)}
			if in.RouteID != uuid.Nil {
				params.RouteID = &in.RouteID
			}
			if in.OriginAgencyID != uuid.Nil {
				params.OriginAgencyID = &in.OriginAgencyID
			}
			out := &struct{ Body []AdminDeparture }{}
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				var err error
				out.Body, err = ListForStaff(ctx, q, params)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-departure", http.MethodPost, "/v1/admin/departures", tagAdmin,
		"Programmer un départ", apix.Staff, planners...),
		"Le plan de sièges du bus est figé sur le départ. L'heure d'arrivée est calculée depuis la durée de la ligne. "+
			"Refusé si le bus est indisponible ou déjà affecté sur ce créneau."),
		func(ctx context.Context, in *struct {
			Body struct {
				RouteID     uuid.UUID         `json:"route_id"`
				BusID       uuid.UUID         `json:"bus_id"`
				DepartureAt time.Time         `json:"departure_at" doc:"Date et heure (RFC 3339, ex. 2026-10-12T07:00:00+01:00)"`
				BasePrice   int64             `json:"base_price,omitempty" minimum:"0" doc:"Par défaut : tarif de la ligne"`
				DriverID    *uuid.UUID        `json:"driver_id,omitempty" doc:"Chauffeur"`
				AgentID     *uuid.UUID        `json:"agent_id,omitempty" doc:"Contrôleur à bord (scanne et vend en route)"`
				Crew        []CrewMemberInput `json:"crew,omitempty" maxItems:"6" doc:"Autres membres : second chauffeur, aide…"`
				Notes       string            `json:"notes,omitempty" maxLength:"1000"`
			}
		}) (*struct {
			Status int
			Body   AdminDeparture
		}, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			if b.DepartureAt.Before(time.Now()) {
				return nil, huma.Error422UnprocessableEntity("La date de départ doit être dans le futur")
			}
			out := &struct {
				Status int
				Body   AdminDeparture
			}{Status: http.StatusCreated}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				route, err := q.GetRoute(ctx, store.GetRouteParams{ID: b.RouteID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Ligne inconnue")
				}
				if err != nil {
					return err
				}
				if !route.IsActive {
					return huma.Error422UnprocessableEntity("Cette ligne est désactivée")
				}
				bus, err := q.GetBus(ctx, store.GetBusParams{ID: b.BusID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Bus inconnu")
				}
				if err != nil {
					return err
				}
				if bus.Status != "active" {
					return huma.Error409Conflict("Ce bus n'est pas disponible (statut : " + bus.Status + ")")
				}
				arrival := b.DepartureAt.Add(time.Duration(route.DurationMinutes) * time.Minute)
				if err := crewConflict(ctx, q, p.CompanyID, nil, b.DepartureAt, arrival, b.AgentID, b.DriverID, b.Crew); err != nil {
					return err
				}
				overlap, err := q.BusHasOverlappingDeparture(ctx, store.BusHasOverlappingDepartureParams{
					BusID: bus.ID, DepartureAt: b.DepartureAt, ArrivalEta: arrival,
				})
				if err != nil {
					return err
				}
				if overlap {
					return huma.Error409Conflict("Ce bus est déjà affecté à un départ sur ce créneau")
				}
				price := b.BasePrice
				if price == 0 {
					price = route.BasePrice
				}
				d, err := q.CreateDeparture(ctx, store.CreateDepartureParams{
					CompanyID: p.CompanyID, RouteID: route.ID, BusID: bus.ID, LayoutID: bus.LayoutID,
					DepartureAt: b.DepartureAt, ArrivalEta: arrival, BasePrice: price,
					DriverID: b.DriverID, AgentID: b.AgentID, Notes: b.Notes,
				})
				if err != nil {
					return err
				}
				// Copie figée des arrêts de la ligne (horaires compris) : modifier la ligne ensuite ne la touche pas.
				if err := q.CopyRouteStopsToDeparture(ctx, store.CopyRouteStopsToDepartureParams{
					DepartureID: d.ID, DepartureAt: d.DepartureAt, RouteID: route.ID}); err != nil {
					return err
				}
				if err := saveCrew(ctx, q, p.CompanyID, d.ID, b.Crew); err != nil {
					return err
				}
				out.Body = AdminDeparture{ID: d.ID, RouteID: d.RouteID, RouteName: route.Name, BusID: d.BusID, PlateNumber: bus.PlateNumber,
					DepartureAt: d.DepartureAt, ArrivalEta: d.ArrivalEta, BasePrice: d.BasePrice, Status: d.Status,
					DriverID: d.DriverID, AgentID: d.AgentID, Notes: d.Notes}
				return apix.Audit(ctx, q, &p.CompanyID, "departure.created", "departure", d.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-recurring-departures", http.MethodPost, "/v1/admin/departures/recurring", tagAdmin,
		"Programmer des départs récurrents", apix.Staff, planners...),
		"Exemple : tous les jours à 07:00 du 1er au 31. Heure du Bénin. 92 jours et 200 départs au plus par demande. "+
			"Les dates passées ou où le bus est déjà affecté sont ignorées et listées dans `skipped`. "+
			"`dry_run: true` renvoie l'aperçu sans rien créer."),
		func(ctx context.Context, in *struct {
			Body struct {
				RouteID   uuid.UUID         `json:"route_id"`
				BusID     uuid.UUID         `json:"bus_id"`
				Time      string            `json:"time" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" example:"07:00" doc:"Heure de départ (heure du Bénin)"`
				FromDate  string            `json:"from_date" format:"date"`
				ToDate    string            `json:"to_date" format:"date"`
				Weekdays  []int             `json:"weekdays,omitempty" maxItems:"7" doc:"1 = lundi … 7 = dimanche ; vide = tous les jours"`
				BasePrice int64             `json:"base_price,omitempty" minimum:"0" doc:"Par défaut : tarif de la ligne"`
				DriverID  *uuid.UUID        `json:"driver_id,omitempty"`
				AgentID   *uuid.UUID        `json:"agent_id,omitempty" doc:"Contrôleur à bord"`
				Crew      []CrewMemberInput `json:"crew,omitempty" maxItems:"6"`
				Notes     string            `json:"notes,omitempty" maxLength:"1000"`
				DryRun    bool              `json:"dry_run,omitempty" doc:"Aperçu sans création"`
			}
		}) (*struct{ Body RecurringResult }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			dates, err := recurringDates(b.FromDate, b.ToDate, b.Time, b.Weekdays)
			if err != nil {
				return nil, err
			}
			out := &struct{ Body RecurringResult }{Body: RecurringResult{Created: []AdminDeparture{}, Skipped: []SkippedDeparture{}}}
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				route, err := q.GetRoute(ctx, store.GetRouteParams{ID: b.RouteID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Ligne inconnue")
				}
				if err != nil {
					return err
				}
				if !route.IsActive {
					return huma.Error422UnprocessableEntity("Cette ligne est désactivée")
				}
				bus, err := q.GetBus(ctx, store.GetBusParams{ID: b.BusID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Bus inconnu")
				}
				if err != nil {
					return err
				}
				if bus.Status != "active" {
					return huma.Error409Conflict("Ce bus n'est pas disponible (statut : " + bus.Status + ")")
				}
				price := b.BasePrice
				if price == 0 {
					price = route.BasePrice
				}
				for _, at := range dates {
					arrival := at.Add(time.Duration(route.DurationMinutes) * time.Minute)
					if at.Before(time.Now()) {
						out.Body.Skipped = append(out.Body.Skipped, SkippedDeparture{DepartureAt: at, Reason: "Date passée"})
						continue
					}
					overlap, err := q.BusHasOverlappingDeparture(ctx, store.BusHasOverlappingDepartureParams{
						BusID: bus.ID, DepartureAt: at, ArrivalEta: arrival,
					})
					if err != nil {
						return err
					}
					if overlap {
						out.Body.Skipped = append(out.Body.Skipped, SkippedDeparture{DepartureAt: at, Reason: "Bus déjà affecté sur ce créneau"})
						continue
					}
					if err := crewConflict(ctx, q, p.CompanyID, nil, at, arrival, b.AgentID, b.DriverID, b.Crew); err != nil {
						var se huma.StatusError
						if errors.As(err, &se) && se.GetStatus() == http.StatusConflict {
							out.Body.Skipped = append(out.Body.Skipped, SkippedDeparture{DepartureAt: at, Reason: se.Error()})
							continue
						}
						return err
					}
					d := store.Departure{RouteID: route.ID, BusID: bus.ID, DepartureAt: at, ArrivalEta: arrival, BasePrice: price,
						Status: "scheduled", DriverID: b.DriverID, AgentID: b.AgentID, Notes: b.Notes}
					if !b.DryRun {
						if d, err = q.CreateDeparture(ctx, store.CreateDepartureParams{
							CompanyID: p.CompanyID, RouteID: route.ID, BusID: bus.ID, LayoutID: bus.LayoutID,
							DepartureAt: at, ArrivalEta: arrival, BasePrice: price,
							DriverID: b.DriverID, AgentID: b.AgentID, Notes: b.Notes,
						}); err != nil {
							return err
						}
						if err := q.CopyRouteStopsToDeparture(ctx, store.CopyRouteStopsToDepartureParams{
							DepartureID: d.ID, DepartureAt: d.DepartureAt, RouteID: route.ID}); err != nil {
							return err
						}
						if err := saveCrew(ctx, q, p.CompanyID, d.ID, b.Crew); err != nil {
							return err
						}
					}
					out.Body.Created = append(out.Body.Created, AdminDeparture{ID: d.ID, RouteID: d.RouteID, RouteName: route.Name, BusID: d.BusID,
						PlateNumber: bus.PlateNumber, DepartureAt: d.DepartureAt, ArrivalEta: d.ArrivalEta, BasePrice: d.BasePrice, Status: d.Status,
						DriverID: d.DriverID, AgentID: d.AgentID, Notes: d.Notes})
				}
				if b.DryRun || len(out.Body.Created) == 0 {
					return nil
				}
				return apix.Audit(ctx, q, &p.CompanyID, "departure.recurring_created", "route", route.ID.String(), nil,
					map[string]any{"bus_id": bus.ID, "time": b.Time, "from": b.FromDate, "to": b.ToDate, "weekdays": b.Weekdays,
						"created": len(out.Body.Created), "skipped": len(out.Body.Skipped)})
			})
			out.Body.DryRun = b.DryRun
			return out, apix.Err(err)
		})

	huma.Register(api, apix.Op("get-departure", http.MethodGet, "/v1/admin/departures/{id}", tagAdmin,
		"Détail d'un départ (plan nominatif)", apix.Staff),
		func(ctx context.Context, in *struct {
			ID        uuid.UUID `path:"id"`
			Boarding  int       `query:"boarding" default:"-1" doc:"Plan et prix sur ce tronçon (guichet) ; absent : tout le trajet"`
			Alighting int       `query:"alighting" default:"-1"`
		}) (*struct {
			Body struct {
				Departure    AdminDeparture  `json:"departure"`
				Categories   []CategoryPrice `json:"categories"`
				SeatMap      SeatMap         `json:"seat_map"`
				BoardingSeq  int             `json:"boarding_seq" doc:"Tronçon du plan"`
				AlightingSeq int             `json:"alighting_seq"`
				UnitPrice    int64           `json:"unit_price" doc:"Prix de base du tronçon (avant catégorie de siège)"`
			}
		}, error) {
			p := auth.MustFromContext(ctx)
			out := &struct {
				Body struct {
					Departure    AdminDeparture  `json:"departure"`
					Categories   []CategoryPrice `json:"categories"`
					SeatMap      SeatMap         `json:"seat_map"`
					BoardingSeq  int             `json:"boarding_seq" doc:"Tronçon du plan"`
					AlightingSeq int             `json:"alighting_seq"`
					UnitPrice    int64           `json:"unit_price" doc:"Prix de base du tronçon (avant catégorie de siège)"`
				}
			}{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := q.GetDeparture(ctx, in.ID)
				if database.IsNotFound(err) {
					return apix.NotFound("Départ")
				}
				if err != nil {
					return err
				}
				list, err := ListForStaff(ctx, q, store.ListDeparturesParams{CompanyID: p.CompanyID,
					FromTs: d.DepartureAt, ToTs: d.DepartureAt.Add(time.Second), RouteID: &d.RouteID})
				if err != nil {
					return err
				}
				for _, x := range list {
					if x.ID == d.ID {
						out.Body.Departure = x
					}
				}
				if out.Body.Departure.Stops, err = LoadStops(ctx, q, d.ID); err != nil {
					return err
				}
				if out.Body.Departure.Crew, err = LoadCrew(ctx, q, d); err != nil {
					return err
				}
				if out.Body.Departure.Visits, err = LoadVisits(ctx, q, d.ID); err != nil {
					return err
				}
				// Sans tronçon : tout le trajet (un siège occupé sur au moins un tronçon apparaît pris).
				trip, err := segment.Plan(ctx, q, d, optSeq(in.Boarding), optSeq(in.Alighting), nil)
				if err != nil {
					return err
				}
				out.Body.BoardingSeq, out.Body.AlightingSeq, out.Body.UnitPrice = trip.Boarding, trip.Alighting, trip.UnitPrice
				out.Body.SeatMap, out.Body.Categories, err = LoadSeatMap(ctx, q, p.CompanyID, d.ID, d.LayoutID, trip.UnitPrice,
					trip.Boarding, trip.Alighting, true)
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("update-departure", http.MethodPut, "/v1/admin/departures/{id}", tagAdmin,
		"Modifier un départ (horaire, tarif, équipage)", apix.Staff, planners...),
		"Le bus et le plan ne sont pas modifiables une fois le départ créé. Un changement d'horaire déclenche une notification des voyageurs."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				DepartureAt time.Time         `json:"departure_at"`
				BasePrice   int64             `json:"base_price" minimum:"1" doc:"S'applique aux nouvelles ventes uniquement"`
				DriverID    *uuid.UUID        `json:"driver_id,omitempty"`
				AgentID     *uuid.UUID        `json:"agent_id,omitempty" doc:"Contrôleur à bord"`
				Crew        []CrewMemberInput `json:"crew,omitempty" maxItems:"6" doc:"Absent : autres membres inchangés ; [] : aucun"`
				Notes       string            `json:"notes,omitempty" maxLength:"1000"`
			}
		}) (*struct{ Body AdminDeparture }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body AdminDeparture }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				before, err := q.GetDeparture(ctx, in.ID)
				if database.IsNotFound(err) {
					return apix.NotFound("Départ")
				}
				if err != nil {
					return err
				}
				if before.Status != "scheduled" && before.Status != "boarding" {
					return huma.Error409Conflict("Départ déjà parti ou annulé")
				}
				arrival := b.DepartureAt.Add(before.ArrivalEta.Sub(before.DepartureAt))
				crew := b.Crew
				if crew == nil { // champ absent : autres membres conservés
					rows, err := q.ListDepartureCrew(ctx, before.ID)
					if err != nil {
						return err
					}
					crew = []CrewMemberInput{}
					for _, m := range rows {
						crew = append(crew, CrewMemberInput{StaffID: m.StaffID, Role: m.Role})
					}
				}
				if err := crewConflict(ctx, q, p.CompanyID, &before.ID, b.DepartureAt, arrival, b.AgentID, b.DriverID, crew); err != nil {
					return err
				}
				overlap, err := q.BusHasOverlappingDeparture(ctx, store.BusHasOverlappingDepartureParams{
					BusID: before.BusID, ExcludeID: &before.ID, DepartureAt: b.DepartureAt, ArrivalEta: arrival,
				})
				if err != nil {
					return err
				}
				if overlap {
					return huma.Error409Conflict("Le bus est déjà affecté à un départ sur ce nouveau créneau")
				}
				d, err := q.UpdateDeparture(ctx, store.UpdateDepartureParams{ID: in.ID, CompanyID: p.CompanyID,
					DepartureAt: b.DepartureAt, ArrivalEta: arrival, BasePrice: b.BasePrice,
					DriverID: b.DriverID, AgentID: b.AgentID, Notes: b.Notes})
				if err != nil {
					return err
				}
				if err := saveCrew(ctx, q, p.CompanyID, d.ID, crew); err != nil {
					return err
				}
				if !d.DepartureAt.Equal(before.DepartureAt) {
					// Tous les arrêts sont décalés d'autant (heures prévues des voyageurs en aval comprises).
					if err := q.RescheduleDepartureStops(ctx, store.RescheduleDepartureStopsParams{DepartureID: d.ID,
						NewDepartureAt: d.DepartureAt, OldDepartureAt: before.DepartureAt}); err != nil {
						return err
					}
					if err := h.Queue.InsertTx(ctx, tx, jobs.DepartureStatusArgs{DepartureID: d.ID, Status: "rescheduled"}); err != nil {
						return err
					}
				}
				out.Body = AdminDeparture{ID: d.ID, RouteID: d.RouteID, BusID: d.BusID, DepartureAt: d.DepartureAt, ArrivalEta: d.ArrivalEta,
					BasePrice: d.BasePrice, Status: d.Status, DriverID: d.DriverID, AgentID: d.AgentID, Notes: d.Notes}
				return apix.Audit(ctx, q, &p.CompanyID, "departure.updated", "departure", d.ID.String(),
					map[string]any{"departure_at": before.DepartureAt, "base_price": before.BasePrice},
					map[string]any{"departure_at": d.DepartureAt, "base_price": d.BasePrice})
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("change-departure-status", http.MethodPost, "/v1/admin/departures/{id}/status", tagAdmin,
		"Changer le statut d'un départ", apix.Staff, planners...),
		"Transitions : scheduled → boarding → departed → arrived ; annulation possible avant le départ. "+
			"L'annulation annule toutes les réservations et crée les demandes de remboursement intégral."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Status string `json:"status" enum:"boarding,departed,arrived,cancelled"`
				Reason string `json:"reason,omitempty" maxLength:"300"`
			}
		}) (*struct {
			Body struct {
				Status            string `json:"status"`
				BookingsCancelled int    `json:"bookings_cancelled"`
			}
		}, error) {
			p := auth.MustFromContext(ctx)
			d, n, err := h.ChangeStatus(ctx, p, in.ID, in.Body.Status, in.Body.Reason)
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct {
				Body struct {
					Status            string `json:"status"`
					BookingsCancelled int    `json:"bookings_cancelled"`
				}
			}{}
			out.Body.Status, out.Body.BookingsCancelled = d.Status, n
			return out, nil
		})

	huma.Register(api, apix.Op("block-departure-seat", http.MethodPost, "/v1/admin/departures/{id}/blocked-seats", tagAdmin,
		"Bloquer un siège (réserve, panne, personnel…)", apix.Staff, planners...),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				SeatID uuid.UUID `json:"seat_id"`
				Reason string    `json:"reason,omitempty" maxLength:"200"`
			}
		}) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := q.GetDeparture(ctx, in.ID)
				if database.IsNotFound(err) {
					return apix.NotFound("Départ")
				}
				if err != nil {
					return err
				}
				// Un blocage vaut pour tout le trajet : le siège doit être libre sur tous les tronçons.
				last, err := q.DepartureLastSeq(ctx, d.ID)
				if err != nil {
					return err
				}
				sm, err := q.GetDepartureSeatMap(ctx, store.GetDepartureSeatMapParams{DepartureID: d.ID, LayoutID: d.LayoutID,
					FromSeq: 0, ToSeq: last})
				if err != nil {
					return err
				}
				for _, c := range sm {
					if c.ID == in.Body.SeatID && c.Kind == "seat" {
						if c.State == "sold" || c.State == "held" || c.State == "boarded" {
							return huma.Error409Conflict("Ce siège est déjà vendu ou en cours de paiement")
						}
						staffID := p.ID
						if err := q.BlockDepartureSeat(ctx, store.BlockDepartureSeatParams{DepartureID: d.ID, SeatCellID: c.ID,
							CompanyID: p.CompanyID, Reason: in.Body.Reason, BlockedBy: &staffID}); err != nil {
							return err
						}
						return apix.Audit(ctx, q, &p.CompanyID, "departure.seat_blocked", "departure", d.ID.String(), nil,
							map[string]any{"seat": c.SeatLabel, "reason": in.Body.Reason})
					}
				}
				return huma.Error422UnprocessableEntity("Siège inconnu pour ce départ")
			})
			return nil, apix.Err(err)
		})

	huma.Register(api, apix.Op("unblock-departure-seat", http.MethodDelete, "/v1/admin/departures/{id}/blocked-seats/{seat_id}", tagAdmin,
		"Débloquer un siège", apix.Staff, planners...),
		func(ctx context.Context, in *struct {
			ID     uuid.UUID `path:"id"`
			SeatID uuid.UUID `path:"seat_id"`
		}) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if err := q.UnblockDepartureSeat(ctx, store.UnblockDepartureSeatParams{DepartureID: in.ID, SeatCellID: in.SeatID, CompanyID: p.CompanyID}); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "departure.seat_unblocked", "departure", in.ID.String(), nil,
					map[string]any{"seat_id": in.SeatID})
			})
			return nil, apix.Err(err)
		})
}

// ListForStaff renvoie les départs avec leurs compteurs (vendus, embarqués).
func ListForStaff(ctx context.Context, q *store.Queries, params store.ListDeparturesParams) ([]AdminDeparture, error) {
	rows, err := q.ListDepartures(ctx, params)
	out := make([]AdminDeparture, 0, len(rows))
	for _, r := range rows {
		out = append(out, AdminDeparture{ID: r.ID, RouteID: r.RouteID, RouteName: r.RouteName, BusID: r.BusID, PlateNumber: r.PlateNumber,
			DepartureAt: r.DepartureAt, ArrivalEta: r.ArrivalEta, BasePrice: r.BasePrice, Status: r.Status,
			DriverID: r.DriverID, AgentID: r.AgentID, Notes: r.Notes, SeatCount: r.SeatCount, SoldSeats: r.SoldSeats, BoardedSeats: r.BoardedSeats, PeakSeats: r.PeakSeats})
	}
	return out, err
}

func ensureStaff(ctx context.Context, q *store.Queries, companyID uuid.UUID, ids ...*uuid.UUID) error {
	for _, id := range ids {
		if id == nil {
			continue
		}
		st, err := q.GetStaff(ctx, *id)
		if err != nil || st.CompanyID != companyID || !st.IsActive {
			return huma.Error422UnprocessableEntity("Membre du personnel inconnu ou inactif : " + id.String())
		}
	}
	return nil
}

type SkippedDeparture struct {
	DepartureAt time.Time `json:"departure_at"`
	Reason      string    `json:"reason"`
}

type RecurringResult struct {
	DryRun  bool               `json:"dry_run"`
	Created []AdminDeparture   `json:"created" doc:"Départs créés (ou qui le seraient, en aperçu)"`
	Skipped []SkippedDeparture `json:"skipped"`
}

const (
	maxRecurringDays       = 92
	maxRecurringDepartures = 200
)

// beninTZ : heure du Bénin (UTC+1, sans heure d'été).
var beninTZ = time.FixedZone("WAT", 3600)

// recurringDates calcule les dates de départ d'une programmation récurrente.
func recurringDates(from, to, hhmm string, weekdays []int) ([]time.Time, error) {
	start, err := time.ParseInLocation(time.DateOnly, from, beninTZ)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Date de début invalide")
	}
	end, err := time.ParseInLocation(time.DateOnly, to, beninTZ)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Date de fin invalide")
	}
	if end.Before(start) {
		return nil, huma.Error422UnprocessableEntity("La date de fin doit suivre la date de début")
	}
	if end.Sub(start) > maxRecurringDays*24*time.Hour {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("Période limitée à %d jours par programmation", maxRecurringDays))
	}
	clock, err := time.Parse("15:04", hhmm)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Heure invalide (HH:MM)")
	}
	days := map[time.Weekday]bool{}
	for _, d := range weekdays {
		if d < 1 || d > 7 {
			return nil, huma.Error422UnprocessableEntity("Jours de la semaine : 1 (lundi) à 7 (dimanche)")
		}
		days[time.Weekday(d%7)] = true
	}
	var out []time.Time
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if len(days) > 0 && !days[d.Weekday()] {
			continue
		}
		out = append(out, time.Date(d.Year(), d.Month(), d.Day(), clock.Hour(), clock.Minute(), 0, 0, beninTZ))
	}
	if len(out) == 0 {
		return nil, huma.Error422UnprocessableEntity("Aucune date ne correspond aux jours choisis")
	}
	if len(out) > maxRecurringDepartures {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("%d départs au plus par programmation", maxRecurringDepartures))
	}
	return out, nil
}

func derefID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// optSeq : paramètre d'indice d'arrêt facultatif (-1 = non fourni).
func optSeq(v int) *int {
	if v < 0 {
		return nil
	}
	return &v
}

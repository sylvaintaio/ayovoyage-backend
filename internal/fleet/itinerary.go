package fleet

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"busvoyage/internal/itinerary"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// RouteStop : arrêt d'une ligne, dans l'ordre de passage.
type RouteStop struct {
	Seq           int        `json:"seq"`
	StopPointID   uuid.UUID  `json:"stop_point_id"`
	StopName      string     `json:"stop_name"`
	CityID        uuid.UUID  `json:"city_id"`
	CityName      string     `json:"city_name"`
	AgencyID      *uuid.UUID `json:"agency_id,omitempty"`
	OffsetMinutes int        `json:"offset_minutes" doc:"Minutes depuis l'heure de départ"`
	PauseMinutes  int        `json:"pause_minutes"`
	CanBoard      bool       `json:"can_board" doc:"Montée sur réservation (agences)"`
	CanAlight     bool       `json:"can_alight"`
}

type RouteStopInput struct {
	StopPointID   uuid.UUID `json:"stop_point_id"`
	OffsetMinutes int       `json:"offset_minutes" minimum:"0" maximum:"4320" doc:"Minutes depuis l'heure de départ (0 pour le premier arrêt)"`
	PauseMinutes  int       `json:"pause_minutes,omitempty" minimum:"0" maximum:"180"`
	CanBoard      bool      `json:"can_board"`
	CanAlight     bool      `json:"can_alight"`
}

// RouteFare : case de la grille de prix, de ville à ville (mode « grid »).
type RouteFare struct {
	FromCityID uuid.UUID `json:"from_city_id"`
	ToCityID   uuid.UUID `json:"to_city_id"`
	Price      int64     `json:"price" minimum:"1"`
}

// loadItinerary : arrêts et grille d'une ligne.
func loadItinerary(ctx context.Context, q *store.Queries, routeID uuid.UUID) ([]RouteStop, []RouteFare, error) {
	rows, err := q.ListRouteStops(ctx, routeID)
	if err != nil {
		return nil, nil, err
	}
	stops := make([]RouteStop, 0, len(rows))
	for _, r := range rows {
		stops = append(stops, RouteStop{Seq: int(r.Seq), StopPointID: r.StopPointID, StopName: r.StopName, CityID: r.CityID,
			CityName: r.CityName, AgencyID: r.AgencyID, OffsetMinutes: int(r.OffsetMinutes), PauseMinutes: int(r.PauseMinutes),
			CanBoard: r.CanBoard, CanAlight: r.CanAlight})
	}
	fareRows, err := q.ListRouteFares(ctx, routeID)
	if err != nil {
		return nil, nil, err
	}
	fares := make([]RouteFare, 0, len(fareRows))
	for _, f := range fareRows {
		fares = append(fares, RouteFare{FromCityID: f.FromCityID, ToCityID: f.ToCityID, Price: f.Price})
	}
	return stops, fares, nil
}

// legacyStops : ancienne forme « agence de départ → agence d'arrivée » (formulaire non encore migré).
func legacyStops(ctx context.Context, q *store.Queries, origin, destination uuid.UUID, duration int32) ([]RouteStopInput, error) {
	o, err := q.GetStopPointByAgency(ctx, &origin)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Agence de départ sans point d'arrêt")
	}
	d, err := q.GetStopPointByAgency(ctx, &destination)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("Agence d'arrivée sans point d'arrêt")
	}
	return []RouteStopInput{
		{StopPointID: o.ID, OffsetMinutes: 0, CanBoard: true},
		{StopPointID: d.ID, OffsetMinutes: int(duration), CanAlight: true},
	}, nil
}

// saveItinerary remplace les arrêts et la grille d'une ligne après validation, puis recalcule
// ses extrémités et sa durée. Les départs déjà créés gardent leur copie des arrêts.
func saveItinerary(ctx context.Context, q *store.Queries, companyID, routeID uuid.UUID, fullPrice int64,
	mode string, in []RouteStopInput, fares []RouteFare) (store.Route, error) {
	stops := make([]itinerary.Stop, len(in))
	for i, s := range in {
		sp, err := q.GetStopPoint(ctx, store.GetStopPointParams{ID: s.StopPointID, CompanyID: companyID})
		if database.IsNotFound(err) {
			return store.Route{}, huma.Error422UnprocessableEntity(fmt.Sprintf("Arrêt n° %d : point d'arrêt inconnu", i+1))
		}
		if err != nil {
			return store.Route{}, err
		}
		if !sp.IsActive {
			return store.Route{}, huma.Error422UnprocessableEntity(fmt.Sprintf("Arrêt n° %d : « %s » est désactivé", i+1, sp.Name))
		}
		stops[i] = itinerary.Stop{Seq: i, StopPointID: sp.ID, CityID: sp.CityID, AgencyID: sp.AgencyID,
			OffsetMinutes: s.OffsetMinutes, PauseMinutes: s.PauseMinutes, CanBoard: s.CanBoard, CanAlight: s.CanAlight}
	}
	if err := itinerary.Validate(stops); err != nil {
		return store.Route{}, huma.Error422UnprocessableEntity(err.Error())
	}
	if mode != "grid" {
		mode, fares = "flat", nil
	}
	if err := validateFares(stops, fares, fullPrice); err != nil {
		return store.Route{}, huma.Error422UnprocessableEntity(err.Error())
	}

	if err := q.DeleteRouteStops(ctx, routeID); err != nil {
		return store.Route{}, err
	}
	for _, s := range stops {
		if err := q.InsertRouteStop(ctx, store.InsertRouteStopParams{RouteID: routeID, CompanyID: companyID, Seq: int32(s.Seq),
			StopPointID: s.StopPointID, OffsetMinutes: int32(s.OffsetMinutes), PauseMinutes: int32(s.PauseMinutes),
			CanBoard: s.CanBoard, CanAlight: s.CanAlight}); err != nil {
			return store.Route{}, err
		}
	}
	if err := q.DeleteRouteFares(ctx, routeID); err != nil {
		return store.Route{}, err
	}
	for _, f := range fares {
		if err := q.InsertRouteFare(ctx, store.InsertRouteFareParams{RouteID: routeID, CompanyID: companyID,
			FromCityID: f.FromCityID, ToCityID: f.ToCityID, Price: f.Price}); err != nil {
			return store.Route{}, err
		}
	}
	last := stops[len(stops)-1]
	return q.SetRouteItinerary(ctx, store.SetRouteItineraryParams{ID: routeID, CompanyID: companyID,
		OriginAgencyID: *stops[0].AgencyID, DestinationAgencyID: last.AgencyID,
		DurationMinutes: int32(itinerary.DurationMinutes(stops)), PricingMode: mode})
}

// validateFares : chaque case relie une ville de montée à une ville desservie plus loin,
// sans doublon ni montant supérieur au prix complet.
func validateFares(stops []itinerary.Stop, fares []RouteFare, fullPrice int64) error {
	firstBoard, lastAlight := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	for i, s := range stops {
		if _, ok := firstBoard[s.CityID]; s.CanBoard && !ok {
			firstBoard[s.CityID] = i
		}
		if s.CanAlight {
			lastAlight[s.CityID] = i
		}
	}
	seen := map[itinerary.FarePair]bool{}
	for _, f := range fares {
		key := itinerary.FarePair{From: f.FromCityID, To: f.ToCityID}
		b, okB := firstBoard[f.FromCityID]
		a, okA := lastAlight[f.ToCityID]
		switch {
		case seen[key]:
			return fmt.Errorf("Grille : trajet en double")
		case !okB || !okA || a <= b || f.FromCityID == f.ToCityID:
			return fmt.Errorf("Grille : trajet impossible sur cette ligne (montée puis descente plus loin)")
		case f.Price > fullPrice:
			return fmt.Errorf("Grille : un trajet ne peut pas coûter plus que le prix complet (%d F CFA)", fullPrice)
		}
		seen[key] = true
	}
	return nil
}

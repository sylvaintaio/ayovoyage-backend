// Package segment : trajet d'un voyageur sur un départ (arrêt de montée → arrêt de descente),
// à partir de la copie figée des arrêts du départ. Utilisé par la recherche, la page d'un
// départ, la réservation, le guichet, le changement de billet et le paiement en agence.
package segment

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"busvoyage/internal/itinerary"
	"busvoyage/internal/store"
)

// Stop : arrêt d'un départ avec son heure prévue.
type Stop struct {
	itinerary.Stop
	Name        string
	CityName    string
	Address     string
	Latitude    *float64
	Longitude   *float64
	ScheduledAt time.Time
}

// LoadStops : arrêts d'un départ, dans l'ordre de passage.
func LoadStops(ctx context.Context, q *store.Queries, departureID uuid.UUID) ([]Stop, error) {
	rows, err := q.ListDepartureStops(ctx, departureID)
	if err != nil {
		return nil, err
	}
	out := make([]Stop, len(rows))
	for i, r := range rows {
		out[i] = Stop{
			Stop: itinerary.Stop{Seq: int(r.Seq), StopPointID: r.StopPointID, CityID: r.CityID, AgencyID: r.AgencyID,
				PauseMinutes: int(r.PauseMinutes), CanBoard: r.CanBoard, CanAlight: r.CanAlight},
			Name: r.StopName, CityName: r.CityName, Address: r.Address, Latitude: r.Latitude, Longitude: r.Longitude,
			ScheduledAt: r.ScheduledAt,
		}
	}
	return out, nil
}

func plain(stops []Stop) []itinerary.Stop {
	out := make([]itinerary.Stop, len(stops))
	for i, s := range stops {
		out[i] = s.Stop
	}
	return out
}

// Trip : trajet retenu, avec son prix Standard (avant catégorie, remises et points).
type Trip struct {
	Boarding  int
	Alighting int
	Stops     []Stop
	UnitPrice int64
}

func (t Trip) BoardingStop() Stop  { return t.Stops[t.Boarding] }
func (t Trip) AlightingStop() Stop { return t.Stops[t.Alighting] }

// Last : indice du dernier arrêt (tout le trajet = [0, Last]).
func Last(stops []Stop) int { return len(stops) - 1 }

// Resolve choisit et vérifie le trajet :
//   - montée : celle demandée, sinon l'arrêt de l'agence du vendeur (guichet), sinon le premier arrêt ;
//   - descente : celle demandée, sinon le dernier arrêt.
func Resolve(stops []Stop, boarding, alighting *int, sellerAgency *uuid.UUID) (int, int, error) {
	if len(stops) < 2 {
		return 0, 0, huma.Error409Conflict("Arrêts du départ introuvables")
	}
	b, a := 0, Last(stops)
	if boarding != nil {
		b = *boarding
	} else if sellerAgency != nil {
		for _, s := range stops {
			if s.CanBoard && s.AgencyID != nil && *s.AgencyID == *sellerAgency {
				b = s.Seq
				break
			}
		}
	}
	if alighting != nil {
		a = *alighting
	}
	if err := itinerary.CheckSegment(plain(stops), b, a); err != nil {
		return 0, 0, huma.Error422UnprocessableEntity(err.Error())
	}
	return b, a, nil
}

// Pricing : mode de tarification et grille actuelle de la ligne.
func Pricing(ctx context.Context, q *store.Queries, route store.Route) (itinerary.Pricing, error) {
	p := itinerary.Pricing{Mode: route.PricingMode, FullPrice: route.BasePrice, Grid: map[itinerary.FarePair]int64{}}
	if route.PricingMode != "grid" {
		return p, nil
	}
	rows, err := q.ListRouteFares(ctx, route.ID)
	if err != nil {
		return p, err
	}
	for _, f := range rows {
		p.Grid[itinerary.FarePair{From: f.FromCityID, To: f.ToCityID}] = f.Price
	}
	return p, nil
}

// Plan : trajet complet (arrêts, montée, descente, prix) pour un départ.
func Plan(ctx context.Context, q *store.Queries, d store.Departure, boarding, alighting *int, sellerAgency *uuid.UUID) (Trip, error) {
	stops, err := LoadStops(ctx, q, d.ID)
	if err != nil {
		return Trip{}, err
	}
	b, a, err := Resolve(stops, boarding, alighting, sellerAgency)
	if err != nil {
		return Trip{}, err
	}
	route, err := q.GetRoute(ctx, store.GetRouteParams{ID: d.RouteID, CompanyID: d.CompanyID})
	if err != nil {
		return Trip{}, err
	}
	pr, err := Pricing(ctx, q, route)
	if err != nil {
		return Trip{}, err
	}
	return Trip{Boarding: b, Alighting: a, Stops: stops,
		UnitPrice: itinerary.SegmentPrice(pr, d.BasePrice, stops[b].CityID, stops[a].CityID)}, nil
}

// Departed : le car a-t-il quitté cet arrêt (journal des arrêts) ?
func Departed(ctx context.Context, q *store.Queries, departureID uuid.UUID, seq int) (bool, error) {
	s := int32(seq)
	return q.StopDeparted(ctx, store.StopDepartedParams{DepartureID: departureID, Seq: &s})
}

// OpenForSale : la vente est-elle encore possible pour cette montée ?
// Départ programmé ou en embarquement : oui. Car parti : seulement pour monter plus loin,
// tant qu'il n'a pas quitté l'arrêt de montée.
func OpenForSale(ctx context.Context, q *store.Queries, d store.Departure, boarding int) error {
	switch d.Status {
	case "scheduled", "boarding":
	case "departed":
		if boarding == 0 {
			return huma.Error409Conflict("Le car est déjà parti de cet arrêt")
		}
	default:
		return huma.Error409Conflict("Ce départ n'est plus ouvert à la vente")
	}
	gone, err := Departed(ctx, q, d.ID, boarding)
	if err != nil {
		return err
	}
	if gone {
		return huma.Error409Conflict("Le car est déjà parti de cet arrêt")
	}
	return nil
}

// ForBooking remplace, dans le détail d'un départ, les extrémités de la ligne par le trajet
// du voyageur : arrêt et heure de montée, arrêt et heure de descente. À appeler partout où un
// départ est présenté pour une réservation (fiche, billet, e-mails, rappels, facture).
func ForBooking(ctx context.Context, q *store.Queries, d *store.GetDepartureDetailRow, bookingID uuid.UUID) error {
	t, err := q.GetBookingTripView(ctx, bookingID)
	if err != nil {
		return err
	}
	d.DepartureAt, d.ArrivalEta = t.BoardingAt, t.AlightingAt
	d.OriginCity, d.OriginAgencyName, d.OriginAgencyAddress = t.BoardingCity, t.BoardingStop, t.BoardingAddress
	if t.BoardingAgencyID != nil {
		d.OriginAgencyID = *t.BoardingAgencyID
	}
	d.DestinationCity, d.DestinationAgencyName, d.DestinationAgencyID = t.AlightingCity, t.AlightingStop, t.AlightingAgencyID
	d.DurationMinutes = int32(t.AlightingAt.Sub(t.BoardingAt).Minutes())
	return nil
}

// Package itinerary : règles métier des lignes à arrêts (sans base de données).
//
// Une ligne est une suite ordonnée de points d'arrêt. Une réservation va d'un arrêt de
// montée à un arrêt de descente ; un siège n'est occupé que sur ce tronçon.
// Voir docs/lignes-arrets-equipage.md.
package itinerary

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Stop : arrêt d'une ligne (ou d'un départ, avec son heure prévue).
type Stop struct {
	Seq           int
	StopPointID   uuid.UUID
	CityID        uuid.UUID
	AgencyID      *uuid.UUID // point d'arrêt situé à une agence
	OffsetMinutes int        // depuis l'heure de départ
	PauseMinutes  int
	CanBoard      bool
	CanAlight     bool
}

// Validate applique les règles de cohérence d'une ligne :
// au moins 2 arrêts, numérotés 0..n-1 ; premier arrêt = agence où l'on monte ;
// dernier arrêt = descente possible ; heures croissantes ; point d'arrêt unique ;
// montée seulement en agence (la montée ailleurs passe par la vente à bord) ;
// au moins une descente possible après chaque montée.
func Validate(stops []Stop) error {
	if len(stops) < 2 {
		return errors.New("Une ligne compte au moins 2 arrêts")
	}
	seen := map[uuid.UUID]bool{}
	for i, s := range stops {
		if s.Seq != i {
			return fmt.Errorf("Arrêts mal numérotés (attendu %d, reçu %d)", i, s.Seq)
		}
		if seen[s.StopPointID] {
			return fmt.Errorf("Arrêt n° %d : point d'arrêt déjà présent sur la ligne", i+1)
		}
		seen[s.StopPointID] = true
		if s.CanBoard && s.AgencyID == nil {
			return fmt.Errorf("Arrêt n° %d : la montée sur réservation n'est possible qu'en agence", i+1)
		}
		if s.OffsetMinutes < 0 || s.PauseMinutes < 0 || s.PauseMinutes > 180 {
			return fmt.Errorf("Arrêt n° %d : horaire ou pause invalide", i+1)
		}
		if i > 0 && s.OffsetMinutes <= stops[i-1].OffsetMinutes+stops[i-1].PauseMinutes {
			return fmt.Errorf("Arrêt n° %d : l'heure doit suivre celle de l'arrêt précédent (pause comprise)", i+1)
		}
	}
	first, last := stops[0], stops[len(stops)-1]
	switch {
	case first.OffsetMinutes != 0:
		return errors.New("Le premier arrêt est l'heure de départ (décalage 0)")
	case !first.CanBoard || first.AgencyID == nil:
		return errors.New("Le premier arrêt doit être une agence où l'on monte")
	case first.CanAlight:
		return errors.New("On ne descend pas au premier arrêt")
	case !last.CanAlight:
		return errors.New("Le dernier arrêt doit permettre de descendre")
	case last.CanBoard:
		return errors.New("On ne monte pas au dernier arrêt")
	}
	return nil
}

// DurationMinutes : durée totale prévue (heure d'arrivée au dernier arrêt).
func DurationMinutes(stops []Stop) int {
	if len(stops) == 0 {
		return 0
	}
	return stops[len(stops)-1].OffsetMinutes
}

// Schedule : heure prévue à chaque arrêt pour un départ donné.
func Schedule(departureAt time.Time, stops []Stop) []time.Time {
	out := make([]time.Time, len(stops))
	for i, s := range stops {
		out[i] = departureAt.Add(time.Duration(s.OffsetMinutes) * time.Minute)
	}
	return out
}

// CheckSegment vérifie un trajet montée → descente sur les arrêts d'un départ.
func CheckSegment(stops []Stop, boarding, alighting int) error {
	if boarding < 0 || alighting >= len(stops) || boarding >= alighting {
		return errors.New("Trajet invalide : l'arrêt de descente doit suivre l'arrêt de montée")
	}
	if !stops[boarding].CanBoard {
		return errors.New("On ne peut pas monter à cet arrêt sur réservation")
	}
	if !stops[alighting].CanAlight {
		return errors.New("On ne peut pas descendre à cet arrêt")
	}
	if stops[boarding].CityID == stops[alighting].CityID {
		return errors.New("La montée et la descente sont dans la même ville")
	}
	return nil
}

// FarePair : clé de la grille de prix (de ville à ville).
type FarePair struct{ From, To uuid.UUID }

// Pricing : tarification d'une ligne.
type Pricing struct {
	Mode      string // "flat" (prix unique) ou "grid" (grille de ville à ville)
	FullPrice int64  // prix complet de la ligne (routes.base_price)
	Grid      map[FarePair]int64
}

// RoundTo : arrondi des prix calculés (100 F CFA).
const RoundTo = 100

// SegmentPrice : prix Standard d'un trajet pour un départ dont le prix complet est departurePrice
// (égal au prix de la ligne sauf prix spécial).
//   - prix unique : le prix du départ, quelle que soit la descente ;
//   - grille : case de la grille (prix complet par défaut), mise à l'échelle du prix spécial
//     du départ s'il diffère du prix de la ligne, arrondie à 100 F.
func SegmentPrice(p Pricing, departurePrice int64, fromCity, toCity uuid.UUID) int64 {
	if p.Mode != "grid" || p.FullPrice <= 0 {
		return departurePrice
	}
	fare, ok := p.Grid[FarePair{fromCity, toCity}]
	if !ok || fare <= 0 {
		fare = p.FullPrice
	}
	if departurePrice == p.FullPrice {
		return fare
	}
	scaled := (fare*departurePrice + p.FullPrice/2) / p.FullPrice
	rounded := (scaled + RoundTo/2) / RoundTo * RoundTo
	if rounded < RoundTo {
		rounded = RoundTo
	}
	return rounded
}

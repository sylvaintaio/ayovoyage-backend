package boarding

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"busvoyage/internal/auth"
	"busvoyage/internal/departure"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// access : ce qu'un membre du personnel peut faire sur un départ depuis l'application Agent.
type access struct {
	allowed bool
	// phones : numéros des passagers visibles (contrôleur, encadrement ; pas le chauffeur).
	phones bool
}

// crewWindow : un membre d'équipage n'accède à ses départs que de la veille au lendemain
// (heure du Bénin), pour ne pas parcourir l'historique des voyageurs.
func inCrewWindow(departureAt, now time.Time) bool {
	day := func(t time.Time) time.Time {
		l := t.In(departure.LocalTZ)
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, departure.LocalTZ)
	}
	diff := day(departureAt).Sub(day(now))
	return diff >= -24*time.Hour && diff <= 24*time.Hour
}

// departureAccess applique le périmètre de chaque rôle (voir docs/lignes-arrets-equipage.md) :
//   - administrateur : tout ;
//   - chef d'agence : départs qui partent de son agence ou y arrivent (toute la compagnie sans agence) ;
//   - contrôleur, chauffeur : départs où ils sont affectés, de la veille au lendemain ;
//     seul le contrôleur (agent du départ) voit les téléphones des passagers.
func departureAccess(p auth.Principal, d store.Departure, origin, destination uuid.UUID, now time.Time) access {
	switch p.Role {
	case auth.RoleCompanyAdmin:
		return access{allowed: true, phones: true}
	case auth.RoleAgencyManager:
		ok := p.AgencyID == nil || *p.AgencyID == origin || *p.AgencyID == destination
		return access{allowed: ok, phones: ok}
	case auth.RoleBoardingAgent, auth.RoleDriver:
		isController := d.AgentID != nil && *d.AgentID == p.ID
		isDriver := d.DriverID != nil && *d.DriverID == p.ID
		if !(isController || isDriver) || !inCrewWindow(d.DepartureAt, now) {
			return access{}
		}
		return access{allowed: true, phones: isController}
	}
	return access{}
}

// loadDeparture charge un départ dans le périmètre de l'agent. Hors périmètre : 404,
// pour ne pas confirmer l'existence du départ. open : l'embarquement doit être encore possible.
func (h *Handler) loadDeparture(ctx context.Context, q *store.Queries, p auth.Principal, id uuid.UUID, open bool) (store.Departure, access, error) {
	d, err := q.GetDeparture(ctx, id)
	if database.IsNotFound(err) {
		return d, access{}, apix.NotFound("Départ")
	}
	if err != nil {
		return d, access{}, err
	}
	r, err := q.GetRoute(ctx, store.GetRouteParams{ID: d.RouteID, CompanyID: p.CompanyID})
	if database.IsNotFound(err) {
		return d, access{}, apix.NotFound("Départ")
	}
	if err != nil {
		return d, access{}, err
	}
	a := departureAccess(p, d, r.OriginAgencyID, r.DestinationAgencyID, time.Now())
	if !a.allowed {
		return d, a, apix.NotFound("Départ")
	}
	if open && (d.Status == "cancelled" || d.Status == "arrived") {
		return d, a, huma.Error409Conflict("Ce départ n'accepte plus d'embarquement")
	}
	return d, a, nil
}

package boarding

import (
	"context"
	"slices"
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
	// controller : contrôleur du départ (journal des arrêts, vente à bord).
	controller bool
	// agency : chef d'agence limité aux passagers qui montent ou descendent à son agence.
	agency *uuid.UUID
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
//   - chef d'agence : départs qui s'arrêtent à son agence (toute la compagnie sans agence) ;
//   - contrôleur (agent du départ) : ses départs, de la veille au lendemain, téléphones compris ;
//   - chauffeur et autres membres d'équipage : leurs départs, même fenêtre, sans téléphones.
func departureAccess(p auth.Principal, d store.Departure, servedAgencies []uuid.UUID, crewMember bool, now time.Time) access {
	switch p.Role {
	case auth.RoleCompanyAdmin:
		return access{allowed: true, phones: true}
	case auth.RoleAgencyManager:
		ok := p.AgencyID == nil || slices.Contains(servedAgencies, *p.AgencyID)
		return access{allowed: ok, phones: ok, agency: p.AgencyID}
	case auth.RoleBoardingAgent, auth.RoleDriver:
		isController := d.AgentID != nil && *d.AgentID == p.ID
		isDriver := d.DriverID != nil && *d.DriverID == p.ID
		if !(isController || isDriver || crewMember) || !inCrewWindow(d.DepartureAt, now) {
			return access{}
		}
		return access{allowed: true, phones: isController, controller: isController}
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
	stops, err := q.ListDepartureStops(ctx, d.ID)
	if err != nil {
		return d, access{}, err
	}
	served := make([]uuid.UUID, 0, len(stops))
	for _, st := range stops {
		if st.AgencyID != nil {
			served = append(served, *st.AgencyID)
		}
	}
	crew, err := q.IsCrewMember(ctx, store.IsCrewMemberParams{DepartureID: d.ID, StaffID: p.ID})
	if err != nil {
		return d, access{}, err
	}
	a := departureAccess(p, d, served, crew, time.Now())
	if !a.allowed {
		return d, a, apix.NotFound("Départ")
	}
	if open && (d.Status == "cancelled" || d.Status == "arrived") {
		return d, a, huma.Error409Conflict("Ce départ n'accepte plus d'embarquement")
	}
	return d, a, nil
}

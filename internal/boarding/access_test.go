package boarding

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"busvoyage/internal/auth"
	"busvoyage/internal/departure"
	"busvoyage/internal/store"
)

func TestDepartureAccess(t *testing.T) {
	controller, driver, stranger := uuid.New(), uuid.New(), uuid.New()
	origin, destination, otherAgency := uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, departure.LocalTZ)
	dep := func(at time.Time) store.Departure {
		return store.Departure{ID: uuid.New(), AgentID: &controller, DriverID: &driver, DepartureAt: at}
	}
	today := dep(now.Add(2 * time.Hour))

	cases := []struct {
		name   string
		p      auth.Principal
		d      store.Departure
		want   bool
		phones bool
	}{
		{"administrateur : tout", auth.Principal{ID: stranger, Role: auth.RoleCompanyAdmin}, today, true, true},
		{"contrôleur affecté", auth.Principal{ID: controller, Role: auth.RoleBoardingAgent}, today, true, true},
		{"chauffeur affecté : sans téléphones", auth.Principal{ID: driver, Role: auth.RoleDriver}, today, true, false},
		{"contrôleur non affecté : refusé", auth.Principal{ID: stranger, Role: auth.RoleBoardingAgent}, today, false, false},
		{"chauffeur non affecté : refusé", auth.Principal{ID: stranger, Role: auth.RoleDriver}, today, false, false},
		{"contrôleur, départ de la veille au soir", auth.Principal{ID: controller, Role: auth.RoleBoardingAgent},
			dep(time.Date(2026, 10, 10, 22, 0, 0, 0, departure.LocalTZ)), true, true},
		{"contrôleur, départ du lendemain", auth.Principal{ID: controller, Role: auth.RoleBoardingAgent},
			dep(time.Date(2026, 10, 12, 6, 0, 0, 0, departure.LocalTZ)), true, true},
		{"contrôleur, départ d'il y a 3 jours : refusé", auth.Principal{ID: controller, Role: auth.RoleBoardingAgent},
			dep(now.AddDate(0, 0, -3)), false, false},
		{"contrôleur, départ dans 2 jours : refusé", auth.Principal{ID: controller, Role: auth.RoleBoardingAgent},
			dep(now.AddDate(0, 0, 2)), false, false},
		{"chef de l'agence de départ", auth.Principal{ID: stranger, Role: auth.RoleAgencyManager, AgencyID: &origin}, today, true, true},
		{"chef de l'agence d'arrivée", auth.Principal{ID: stranger, Role: auth.RoleAgencyManager, AgencyID: &destination}, today, true, true},
		{"chef d'une autre agence : refusé", auth.Principal{ID: stranger, Role: auth.RoleAgencyManager, AgencyID: &otherAgency}, today, false, false},
		{"caissier : refusé", auth.Principal{ID: stranger, Role: auth.RoleCashier}, today, false, false},
		{"comptable : refusé", auth.Principal{ID: stranger, Role: auth.RoleFinance}, today, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := departureAccess(c.p, c.d, origin, destination, now)
			if got.allowed != c.want || got.phones != c.phones {
				t.Fatalf("accès = %+v, attendu allowed=%v phones=%v", got, c.want, c.phones)
			}
		})
	}
}

func TestInCrewWindow(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 30, 0, 0, departure.LocalTZ)
	// En UTC il est encore le 10 : la fenêtre se calcule bien en heure du Bénin.
	if !inCrewWindow(time.Date(2026, 10, 12, 23, 0, 0, 0, departure.LocalTZ), now) {
		t.Fatal("le lendemain doit être accessible")
	}
	if inCrewWindow(time.Date(2026, 10, 9, 23, 59, 0, 0, departure.LocalTZ), now) {
		t.Fatal("l'avant-veille doit être refusée")
	}
}

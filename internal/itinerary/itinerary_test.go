package itinerary

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	cotonou, bohicon, dassa, parakou = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	agJonquet, agBohicon             = uuid.New(), uuid.New()
)

// Cotonou (agence) → Bohicon (agence, pause 15 min) → Dassa (carrefour) → Parakou (gare routière).
func line() []Stop {
	return []Stop{
		{Seq: 0, StopPointID: uuid.New(), CityID: cotonou, AgencyID: &agJonquet, OffsetMinutes: 0, CanBoard: true},
		{Seq: 1, StopPointID: uuid.New(), CityID: bohicon, AgencyID: &agBohicon, OffsetMinutes: 150, PauseMinutes: 15, CanBoard: true, CanAlight: true},
		{Seq: 2, StopPointID: uuid.New(), CityID: dassa, OffsetMinutes: 240, CanAlight: true},
		{Seq: 3, StopPointID: uuid.New(), CityID: parakou, OffsetMinutes: 420, CanAlight: true},
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(line()); err != nil {
		t.Fatalf("ligne valide refusée : %v", err)
	}
	cases := map[string]func([]Stop) []Stop{
		"au moins 2":         func(s []Stop) []Stop { return s[:1] },
		"premier = agence":   func(s []Stop) []Stop { s[0].AgencyID = nil; return s },
		"montée en agence":   func(s []Stop) []Stop { s[2].CanBoard = true; return s },
		"heures croissantes": func(s []Stop) []Stop { s[2].OffsetMinutes = 160; return s }, // avant la fin de la pause de Bohicon
		"dernier descente":   func(s []Stop) []Stop { s[3].CanAlight = false; return s },
		"point unique":       func(s []Stop) []Stop { s[2].StopPointID = s[1].StopPointID; return s },
		"numérotation":       func(s []Stop) []Stop { s[2].Seq = 5; return s },
		"départ à 0":         func(s []Stop) []Stop { s[0].OffsetMinutes = 10; return s },
		"pas de montée fin":  func(s []Stop) []Stop { s[3].AgencyID = &agBohicon; s[3].CanBoard = true; return s },
	}
	for name, mutate := range cases {
		if err := Validate(mutate(line())); err == nil {
			t.Errorf("%s : ligne incohérente acceptée", name)
		}
	}
}

func TestSchedule(t *testing.T) {
	dep := time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)
	got := Schedule(dep, line())
	if !got[1].Equal(dep.Add(150*time.Minute)) || !got[3].Equal(dep.Add(7*time.Hour)) {
		t.Fatalf("horaires %v", got)
	}
	if DurationMinutes(line()) != 420 {
		t.Fatal("durée totale")
	}
}

func TestCheckSegment(t *testing.T) {
	s := line()
	if err := CheckSegment(s, 0, 2); err != nil {
		t.Fatalf("Cotonou → Dassa refusé : %v", err)
	}
	if err := CheckSegment(s, 1, 3); err != nil {
		t.Fatalf("Bohicon → Parakou refusé : %v", err)
	}
	for _, c := range [][2]int{{2, 3}, {1, 1}, {3, 1}, {0, 4}, {-1, 2}} {
		if CheckSegment(s, c[0], c[1]) == nil {
			t.Errorf("trajet %v accepté", c)
		}
	}
	s[2].CityID = bohicon
	if err := CheckSegment(s, 1, 2); err == nil || !strings.Contains(err.Error(), "même ville") {
		t.Errorf("même ville acceptée : %v", err)
	}
}

func TestSegmentPrice(t *testing.T) {
	flat := Pricing{Mode: "flat", FullPrice: 8000}
	if SegmentPrice(flat, 8000, cotonou, dassa) != 8000 {
		t.Fatal("prix unique : même prix quelle que soit la descente")
	}
	if SegmentPrice(flat, 9000, cotonou, dassa) != 9000 {
		t.Fatal("prix unique : prix spécial du départ")
	}
	grid := Pricing{Mode: "grid", FullPrice: 8000, Grid: map[FarePair]int64{
		{cotonou, bohicon}: 3000, {cotonou, dassa}: 5000, {bohicon, parakou}: 5500,
	}}
	if got := SegmentPrice(grid, 8000, cotonou, dassa); got != 5000 {
		t.Fatalf("grille Cotonou → Dassa = %d", got)
	}
	if got := SegmentPrice(grid, 8000, cotonou, parakou); got != 8000 {
		t.Fatalf("case absente : prix complet attendu, reçu %d", got)
	}
	// Prix spécial +25 % (10 000 au lieu de 8 000) : 5 000 → 6 250 → arrondi 6 300.
	if got := SegmentPrice(grid, 10000, cotonou, dassa); got != 6300 {
		t.Fatalf("grille mise à l'échelle = %d", got)
	}
}

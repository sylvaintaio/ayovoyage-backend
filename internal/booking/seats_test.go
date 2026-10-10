package booking

import (
	"testing"

	"github.com/google/uuid"

	"busvoyage/internal/store"
)

func seat(row, col int32, label string, cat uuid.UUID, state string, attrs ...string) store.GetDepartureSeatMapRow {
	return store.GetDepartureSeatMapRow{ID: uuid.New(), RowIndex: row, ColIndex: col, Kind: "seat",
		SeatLabel: &label, CategoryID: &cat, State: state, Attributes: attrs}
}

func TestPickSeats(t *testing.T) {
	vip, std := uuid.New(), uuid.New()
	cats := map[uuid.UUID]store.SeatCategory{vip: {ID: vip, Code: "vip"}, std: {ID: std, Code: "standard"}}
	m := []store.GetDepartureSeatMapRow{
		seat(0, 0, "1A", vip, "sold", "window"),
		seat(0, 1, "1B", vip, "available", "aisle"),
		seat(1, 0, "2A", std, "available", "window"),
		seat(1, 1, "2B", std, "available", "aisle"),
		seat(2, 0, "3A", std, "held", "window"),
		seat(2, 1, "3B", std, "available", "aisle"),
	}

	got := pickSeats(m, cats, SeatPreferences{CategoryCode: "vip"}, 1, nil)
	if len(got) != 1 || *got[0].SeatLabel != "1B" {
		t.Fatalf("VIP : attendu 1B, obtenu %v", got)
	}
	if got := pickSeats(m, cats, SeatPreferences{CategoryCode: "vip"}, 2, nil); got != nil {
		t.Fatal("VIP : un seul siège libre, la demande de 2 doit échouer")
	}
	got = pickSeats(m, cats, SeatPreferences{CategoryCode: "standard", Position: "window"}, 1, nil)
	if *got[0].SeatLabel != "2A" {
		t.Fatalf("fenêtre : attendu 2A, obtenu %s", *got[0].SeatLabel)
	}
	got = pickSeats(m, cats, SeatPreferences{CategoryCode: "standard", Together: true}, 2, nil)
	if *got[0].SeatLabel != "2A" || *got[1].SeatLabel != "2B" {
		t.Fatalf("ensemble : attendu 2A+2B, obtenu %s+%s", *got[0].SeatLabel, *got[1].SeatLabel)
	}
	// Les sièges déjà choisis explicitement sont exclus.
	got = pickSeats(m, cats, SeatPreferences{CategoryCode: "standard"}, 1, map[uuid.UUID]bool{m[2].ID: true})
	if *got[0].SeatLabel != "2B" {
		t.Fatalf("exclusion : attendu 2B, obtenu %s", *got[0].SeatLabel)
	}
}

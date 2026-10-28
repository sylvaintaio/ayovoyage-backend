package departure

import (
	"testing"
	"time"
)

func TestScheduleDates(t *testing.T) {
	// Lundi 2 novembre 2026, 23 h 30 UTC = mardi 3 novembre, 00 h 30 au Bénin.
	now := time.Date(2026, 11, 2, 23, 30, 0, 0, time.UTC)
	today := todayBenin(now)
	if today.Format(time.DateOnly) != "2026-11-03" {
		t.Fatalf("jour du Bénin : %s", today)
	}
	if isoWeekday(today) != 2 {
		t.Fatalf("mardi attendu, obtenu %d", isoWeekday(today))
	}
	if isoWeekday(time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)) != 7 {
		t.Fatal("dimanche = 7")
	}
	at := atBenin(today, "07:05")
	if at.UTC().Format(time.RFC3339) != "2026-11-03T06:05:00Z" {
		t.Fatalf("07:05 au Bénin = 06:05 UTC, obtenu %s", at.UTC())
	}
	// Date PostgreSQL (minuit UTC) : même jour civil.
	if d := dateOnly(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)); !d.Equal(today) {
		t.Fatalf("jour civil : %s", d)
	}
}

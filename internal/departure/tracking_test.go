package departure

import (
	"testing"
	"time"
)

func TestDelayOf(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	stops := []DepartureStop{{Seq: 0, ScheduledAt: t0}, {Seq: 1, ScheduledAt: t0.Add(2 * time.Hour), PauseMinutes: 15}, {Seq: 2, ScheduledAt: t0.Add(5 * time.Hour)}}
	seq := func(n int32) *int32 { return &n }
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	if got := delayOf(stops, nil); got != 0 {
		t.Fatalf("pas encore parti : %d", got)
	}
	visits := []StopVisitView{
		{Seq: seq(0), ArrivedAt: t0.Add(-10 * time.Minute), DepartedAt: at(20 * time.Minute)},
		{Reason: "police", ArrivedAt: t0.Add(time.Hour), DepartedAt: at(time.Hour + 10*time.Minute)},
		{Seq: seq(1), ArrivedAt: t0.Add(2*time.Hour + 30*time.Minute), DepartedAt: at(2*time.Hour + 40*time.Minute)},
	}
	// Bohicon : prévu 08:00 + 15 min de pause, reparti à 08:40 → 25 min de retard (le dernier départ compte).
	if got := delayOf(stops, visits); got != 25 {
		t.Fatalf("retard = %d, attendu 25", got)
	}
}

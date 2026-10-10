package departure

import "testing"

func TestRecurringDates(t *testing.T) {
	// Octobre 2026 : le 1er est un jeudi. Lundis, mercredis et vendredis à 07:00.
	dates, err := recurringDates("2026-10-01", "2026-10-11", "07:00", []int{1, 3, 5})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range dates {
		got = append(got, d.Format("Mon 02 15:04 -07:00"))
	}
	want := []string{"Fri 02 07:00 +01:00", "Mon 05 07:00 +01:00", "Wed 07 07:00 +01:00", "Fri 09 07:00 +01:00"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("date %d : %s, attendu %s", i, got[i], want[i])
		}
	}
	all, _ := recurringDates("2026-10-01", "2026-10-31", "22:30", nil)
	if len(all) != 31 {
		t.Errorf("tous les jours : %d dates", len(all))
	}
	// Dimanche = 7.
	sun, _ := recurringDates("2026-10-01", "2026-10-31", "08:00", []int{7})
	if len(sun) != 4 || sun[0].Day() != 4 {
		t.Errorf("dimanches : %v", sun)
	}
	for _, bad := range [][3]string{{"2026-10-10", "2026-10-01", "07:00"}, {"2026-01-01", "2026-12-31", "07:00"}, {"2026-10-01", "2026-10-02", "7h"}} {
		if _, err := recurringDates(bad[0], bad[1], bad[2], nil); err == nil {
			t.Errorf("%v devrait être refusé", bad)
		}
	}
}

package pricing

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"busvoyage/internal/store"
)

func TestMatchEarlyBird(t *testing.T) {
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, localTZ)
	route, other := uuid.New(), uuid.New()
	tabaski := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	rules := []store.PricingRule{
		{ID: uuid.New(), Kind: "early_bird", IsActive: true, MinDaysBefore: 7, Percent: 10},
		{ID: uuid.New(), Kind: "early_bird", IsActive: true, MinDaysBefore: 14, Percent: 20, RouteID: &route,
			ExcludedDates: []time.Time{tabaski}},
		{ID: uuid.New(), Kind: "early_bird", IsActive: false, MinDaysBefore: 1, Percent: 50},
	}
	cases := []struct {
		name    string
		route   uuid.UUID
		depart  time.Time
		percent int32
	}{
		{"trop proche", route, now.AddDate(0, 0, 3), 0},
		{"7 jours, règle générale", route, now.AddDate(0, 0, 8), 10},
		{"15 jours, règle de la ligne (plus avantageuse)", route, now.AddDate(0, 0, 15), 20},
		{"15 jours, autre ligne", other, now.AddDate(0, 0, 15), 10},
		{"date exclue : repli sur la règle générale", route, time.Date(2026, 10, 30, 7, 0, 0, 0, localTZ), 10},
	}
	for _, c := range cases {
		r := MatchEarlyBird(rules, c.route, c.depart, now)
		got := int32(0)
		if r != nil {
			got = r.Percent
		}
		if got != c.percent {
			t.Errorf("%s : %d %% au lieu de %d %%", c.name, got, c.percent)
		}
	}
}

func TestDescribe(t *testing.T) {
	maxD := int64(2000)
	if got := Describe(store.PromoCode{Kind: "percent", Value: 10, MaxDiscount: &maxD}); got != "−10 % (plafonné à 2 000 F CFA)" {
		t.Errorf("Describe = %q", got)
	}
	if got := Describe(store.PromoCode{Kind: "fixed", Value: 1500}); got != "−1 500 F CFA" {
		t.Errorf("Describe = %q", got)
	}
}

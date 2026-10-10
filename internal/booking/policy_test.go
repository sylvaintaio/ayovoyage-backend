package booking

import (
	"testing"

	"busvoyage/internal/store"
)

func i32(v int32) *int32 { return &v }

func TestEffectiveRules(t *testing.T) {
	company := store.Company{CancellationCutoffHours: 6, CancellationFeePercent: 10, ChangeFeeAmount: 1000}

	if r := effectiveRules(company, nil); r.cancelCutoffHours != 6 || r.cancelFeePercent != 10 || r.changeFee != 1000 || r.tierName != "" {
		t.Errorf("sans niveau : %+v", r)
	}

	silver := &store.LoyaltyTier{Name: "Argent", CancellationFeePercent: i32(5), CancellationCutoffHours: i32(3)}
	if r := effectiveRules(company, silver); r.cancelCutoffHours != 3 || r.cancelFeePercent != 5 || r.changeFee != 1000 || r.tierName != "Argent" {
		t.Errorf("Argent : %+v", r)
	}

	platinum := &store.LoyaltyTier{Name: "Platine", CancellationFeePercent: i32(0), CancellationCutoffHours: i32(1), FreeChange: true}
	if r := effectiveRules(company, platinum); r.cancelFeePercent != 0 || r.cancelCutoffHours != 1 || r.changeFee != 0 || !r.freeChange {
		t.Errorf("Platine : %+v", r)
	}

	// Un niveau ne peut jamais dégrader la règle de la compagnie.
	worse := &store.LoyaltyTier{Name: "Test", CancellationFeePercent: i32(30), CancellationCutoffHours: i32(48)}
	if r := effectiveRules(company, worse); r.cancelFeePercent != 10 || r.cancelCutoffHours != 6 || r.tierName != "" {
		t.Errorf("niveau moins favorable appliqué : %+v", r)
	}
}

func TestPaidValue(t *testing.T) {
	// 2 billets à 10 000, remise fidélité 1 000, 2 000 payés en points.
	b := store.Booking{SubtotalAmount: 20000, DiscountAmount: 1000, LoyaltyPointsAmount: 2000, TotalAmount: 17000}
	if got := paidValue(b); got != 17000 {
		t.Errorf("paidValue = %d, attendu 17000", got)
	}
	// Réservation issue d'une modification : les frais (inclus dans total_amount) ne comptent pas.
	b = store.Booking{SubtotalAmount: 12000, DiscountAmount: 0, ChangeFeeAmount: 1000, TotalAmount: 3000}
	if got := paidValue(b); got != 12000 {
		t.Errorf("paidValue (modifiée) = %d, attendu 12000", got)
	}
}

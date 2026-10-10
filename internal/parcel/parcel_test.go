package parcel

import (
	"encoding/base64"
	"strings"
	"testing"

	"busvoyage/internal/store"
)

func TestPrice(t *testing.T) {
	small := store.ParcelTariff{Category: "small", Label: "Petit colis", BasePrice: 2500, IncludedKg: 5, PricePerKg: 300, MaxWeightKg: 10, InsurancePercent: 1}
	for _, tc := range []struct {
		weight   float64
		declared int64
		total    int64
	}{
		{3, 0, 2500},         // sous le poids inclus
		{5, 0, 2500},         // exactement le poids inclus
		{6.2, 0, 2500 + 600}, // 1,2 kg en plus → 2 kg facturés
		{8, 50000, 2500 + 900 + 500},
		{7, 12345, 3125}, // 2500 + 600 + 124 = 3224 ? arrondi au-dessus à 25 F
	} {
		q, err := price(small, tc.weight, tc.declared)
		if err != nil {
			t.Fatalf("%v kg : %v", tc.weight, err)
		}
		want := tc.total
		if tc.weight == 7 {
			want = roundUp(2500+600+124, 25)
		}
		if q.Total != want {
			t.Errorf("%v kg, valeur %d : %d, attendu %d (%+v)", tc.weight, tc.declared, q.Total, want, q)
		}
	}
	if _, err := price(small, 12, 0); err == nil || !strings.Contains(err.Error(), "10 kg") {
		t.Errorf("poids maximal non contrôlé : %v", err)
	}
	if _, err := price(small, 0, 0); err == nil {
		t.Error("poids nul accepté")
	}
}

func TestShortName(t *testing.T) {
	for in, want := range map[string]string{"Awa Dossou": "Awa D.", "Koffi": "Koffi", " jean  marc  élégbédé ": "jean É.", "": ""} {
		if got := shortName(in); got != want {
			t.Errorf("shortName(%q) = %q, attendu %q", in, got, want)
		}
	}
}

func TestTrackingFromScan(t *testing.T) {
	for in, want := range map[string]string{
		"https://busvoyage.bj/colis/suivi/CL7K3M9QXT":      "CL7K3M9QXT",
		"http://localhost:4200/colis/suivi/cl7k3m9qxt?x=1": "CL7K3M9QXT",
		"  cl7k3m9qxt ": "CL7K3M9QXT",
	} {
		if got := TrackingFromScan(in); got != want {
			t.Errorf("%q → %q, attendu %q", in, got, want)
		}
	}
}

func TestDecodePhoto(t *testing.T) {
	ok := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff})
	if ct, data, err := decodePhoto(ok); err != nil || ct != "image/jpeg" || len(data) != 3 {
		t.Fatalf("photo valide refusée : %v", err)
	}
	for _, bad := range []string{"data:text/html;base64,PGI+", "pas une image", "data:image/png;base64,@@@"} {
		if _, _, err := decodePhoto(bad); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
	big := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, 2<<20+1))
	if _, _, err := decodePhoto(big); err == nil {
		t.Error("photo de plus de 2 Mo acceptée")
	}
}

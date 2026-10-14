package ticket

import (
	"context"
	"testing"
)

func TestCompanyLogoRefusesUnsafeURLs(t *testing.T) {
	for _, u := range []string{
		"http://exemple.bj/logo.png",     // pas HTTPS
		"https://127.0.0.1/logo.png",     // le serveur lui-même
		"https://10.0.0.5/logo.png",      // réseau interne
		"https://169.254.169.254/latest", // métadonnées d'hébergeur
		"https://[::1]/logo.png",
		"   ",
	} {
		if companyLogo(context.Background(), &u) != nil {
			t.Errorf("%q : logo accepté, il devait être refusé", u)
		}
	}
	if companyLogo(context.Background(), nil) != nil {
		t.Error("sans logo : nil attendu")
	}
}

func TestMoney(t *testing.T) {
	for in, want := range map[int64]string{500: "500 F CFA", 12500: "12 500 F CFA", 1250000: "1 250 000 F CFA"} {
		if got := money(in, "XOF"); got != want {
			t.Errorf("money(%d) = %q, attendu %q", in, got, want)
		}
	}
}

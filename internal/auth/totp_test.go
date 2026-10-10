package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// Vecteurs de la RFC 6238 (annexe B), SHA-1, tronqués à 6 chiffres.
func TestTOTPRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, tc := range []struct {
		unix int64
		code string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"},
	} {
		step, ok := ValidateTOTP(secret, tc.code, time.Unix(tc.unix, 0), 0)
		if !ok {
			t.Errorf("t=%d : code %s refusé", tc.unix, tc.code)
		}
		if _, again := ValidateTOTP(secret, tc.code, time.Unix(tc.unix, 0), step); again {
			t.Errorf("t=%d : rejeu accepté", tc.unix)
		}
	}
	if _, ok := ValidateTOTP(secret, "287082", time.Unix(59+120, 0), 0); ok {
		t.Error("code trop ancien accepté")
	}
	if _, ok := ValidateTOTP(secret, "287 082", time.Unix(59+25, 0), 0); !ok {
		t.Error("décalage de 30 s et espace devraient être tolérés")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes := NewRecoveryCodes(10)
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 9 || c[4] != '-' || seen[c] {
			t.Fatalf("code invalide ou dupliqué : %q", c)
		}
		seen[c] = true
	}
	if HashRecoveryCode(codes[0]) != HashRecoveryCode(" "+strings.ToLower(strings.ReplaceAll(codes[0], "-", ""))+" ") {
		t.Error("la saisie doit être tolérante (casse, tiret, espaces)")
	}
}

func TestSecretBox(t *testing.T) {
	box, _ := NewSecretBox(make([]byte, 32))
	sealed := box.Seal("JBSWY3DPEHPK3PXP")
	if strings.Contains(sealed, "JBSWY3DP") {
		t.Fatal("secret en clair")
	}
	if got, err := box.Open(sealed); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	other, _ := NewSecretBox(append(make([]byte, 31), 1))
	if _, err := other.Open(sealed); err == nil {
		t.Error("une autre clé ne doit pas déchiffrer")
	}
}

package phone

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"01 97 00 00 01":    "+2290197000001", // format local béninois (10 chiffres)
		"+229 01 97000001":  "+2290197000001",
		"00229 0197000001":  "+2290197000001",
		"2290197000001":     "+2290197000001",
		"+33 6 12 34 56 78": "+33612345678",
	}
	for in, want := range cases {
		got, err := Normalize(in, "229")
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v ; attendu %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "+0123", "12"} {
		if _, err := Normalize(bad, "229"); err == nil {
			t.Errorf("Normalize(%q) aurait dû échouer", bad)
		}
	}
	if m := Mask("+2290197000001"); m != "+22901****0001" {
		t.Errorf("Mask = %q", m)
	}
}

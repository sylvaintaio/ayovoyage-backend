package config

import "testing"

func TestSMSConfigValidate(t *testing.T) {
	ok := SMSConfig{Driver: "http", MaxSegments: 3, HTTPURL: "https://api.example/sms", HTTPMethod: "POST",
		HTTPFormat: "json", HTTPBody: `{"to":"{{to}}"}`, HTTPAuth: "bearer", HTTPToken: "t", HTTPPhoneFormat: "e164"}
	if errs := ok.validate(); len(errs) != 0 {
		t.Fatalf("configuration valide refusée : %v", errs)
	}
	bad := ok
	bad.HTTPURL, bad.HTTPToken, bad.SenderID = "", "", "NOM-BEAUCOUP-TROP-LONG"
	if errs := bad.validate(); len(errs) != 3 {
		t.Fatalf("3 erreurs attendues, obtenu %v", errs)
	}
	if errs := (SMSConfig{Driver: "log", MaxSegments: 3}).validate(); len(errs) != 0 {
		t.Fatalf("pilote log : %v", errs)
	}
}

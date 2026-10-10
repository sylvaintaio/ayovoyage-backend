package config

import "testing"

func TestEmailFromFollowsBrandName(t *testing.T) {
	t.Setenv("EMAIL_FROM", "")
	t.Setenv("MAIL_FROM_ADDRESS", "no-reply@exemple.bj")
	t.Setenv("MAIL_FROM_NAME", "Ancien nom")
	t.Setenv("BRAND_NAME", "Trans Express")
	if got := emailFrom(); got != `"Trans Express" <no-reply@exemple.bj>` {
		t.Errorf("expéditeur = %s", got)
	}
	t.Setenv("BRAND_NAME", "")
	if got := emailFrom(); got != `"Ancien nom" <no-reply@exemple.bj>` {
		t.Errorf("sans BRAND_NAME, MAIL_FROM_NAME attendu : %s", got)
	}
	t.Setenv("MAIL_FROM_NAME", "")
	if got := emailFrom(); got != `"BUS-VOYAGE" <no-reply@exemple.bj>` {
		t.Errorf("nom par défaut attendu : %s", got)
	}
}

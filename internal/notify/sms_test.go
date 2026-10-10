package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"busvoyage/internal/config"
)

func TestPrepareSMS(t *testing.T) {
	got := PrepareSMS("Votre billet « Cotonou → Parakou » est confirmé : siège 12A, départ à 07 h 00.", true, 3)
	want := `Votre billet "Cotonou -> Parakou" est confirme : siege 12A, depart a 07 h 00.`
	if got != want {
		t.Fatalf("PrepareSMS = %q, attendu %q", got, want)
	}
	if n, unicode := SMSSegments(got); n != 1 || unicode {
		t.Fatalf("1 SMS GSM attendu, obtenu %d (unicode=%v)", n, unicode)
	}
}

func TestPrepareSMSTruncates(t *testing.T) {
	long := strings.Repeat("Départ confirmé. ", 40) // ≈ 680 caractères
	got := PrepareSMS(long, true, 2)
	if n, _ := SMSSegments(got); n > 2 {
		t.Fatalf("%d SMS au lieu de 2 au plus", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("troncature sans points de suspension : %q", got[len(got)-10:])
	}
}

func TestSMSSegments(t *testing.T) {
	cases := []struct {
		text     string
		segments int
		unicode  bool
	}{
		{strings.Repeat("a", 160), 1, false},
		{strings.Repeat("a", 161), 2, false},
		{strings.Repeat("a", 306), 2, false},
		{strings.Repeat("a", 307), 3, false},
		{strings.Repeat("[", 80), 1, false}, // caractères étendus : 2 unités
		{strings.Repeat("[", 81), 2, false},
		{"Prix : 5 000 F → payé", 1, true},
		{strings.Repeat("→", 71), 2, true},
	}
	for _, c := range cases {
		n, u := SMSSegments(c.text)
		if n != c.segments || u != c.unicode {
			t.Errorf("SMSSegments(%d car.) = %d,%v ; attendu %d,%v", len([]rune(c.text)), n, u, c.segments, c.unicode)
		}
	}
}

func TestPreparedSMSNormalizesNumber(t *testing.T) {
	rec := &recordingSMS{}
	p := preparedSMS{inner: rec, countryCode: "229", ascii: true, maxSegments: 3}
	if err := p.SendSMS(context.Background(), "01 97 00 00 00", "Bonjour"); err != nil {
		t.Fatal(err)
	}
	if rec.to != "+2290197000000" {
		t.Fatalf("numéro transmis %q", rec.to)
	}
	if err := p.SendSMS(context.Background(), "abc", "Bonjour"); err == nil {
		t.Fatal("un numéro invalide doit être refusé")
	}
	if !isLogSender(preparedSMS{inner: LogSMS{}}) {
		t.Fatal("le pilote log enveloppé doit rester « simulé »")
	}
}

type recordingSMS struct{ to, message string }

func (r *recordingSMS) SendSMS(_ context.Context, to, message string) error {
	r.to, r.message = to, message
	return nil
}

func TestHTTPSMSJSONBearer(t *testing.T) {
	var gotAuth, gotType string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"status":"success","id":"42"}`))
	}))
	defer srv.Close()
	h := NewHTTPSMS(config.SMSConfig{
		SenderID: "BUSVOYAGE", HTTPURL: srv.URL + "/sms", HTTPMethod: "POST", HTTPFormat: "json",
		HTTPBody: `{"from":"{{sender}}","to":"{{to}}","text":"{{message}}"}`, HTTPAuth: "bearer", HTTPToken: "secret",
		HTTPPhoneFormat: "digits", HTTPSuccess: `"success"`, HTTPTimeout: 5 * time.Second,
	})
	if err := h.SendSMS(context.Background(), "+2290197000000", `Code "1234"`); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" || gotType != "application/json" {
		t.Fatalf("en-têtes : %q %q", gotAuth, gotType)
	}
	if body["to"] != "2290197000000" || body["text"] != `Code "1234"` || body["from"] != "BUSVOYAGE" {
		t.Fatalf("corps : %v", body)
	}
}

func TestHTTPSMSGetQueryAndErrors(t *testing.T) {
	var query url.Values
	status := http.StatusOK
	reply := "OK: 1 message"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	cfg := config.SMSConfig{
		HTTPURL:    srv.URL + "/send?user={{user}}&pass={{password}}&to={{to}}&msg={{message}}",
		HTTPMethod: "GET", HTTPFormat: "none", HTTPAuth: "none", HTTPUser: "bv", HTTPPassword: "p&ss",
		HTTPPhoneFormat: "national", HTTPSuccess: "OK", HTTPTimeout: 5 * time.Second,
	}
	if err := NewHTTPSMS(cfg).SendSMS(context.Background(), "+2290197000000", "Bus a 7 h & quai 2"); err != nil {
		t.Fatal(err)
	}
	if query.Get("to") != "0197000000" || query.Get("msg") != "Bus a 7 h & quai 2" || query.Get("pass") != "p&ss" {
		t.Fatalf("paramètres : %v", query)
	}

	reply = "ERROR: credit"
	if err := NewHTTPSMS(cfg).SendSMS(context.Background(), "+2290197000000", "x"); err == nil {
		t.Fatal("réponse sans le texte de succès : erreur attendue")
	}
	status, reply = http.StatusUnauthorized, `{"error":"bad key"}`
	err := NewHTTPSMS(cfg).SendSMS(context.Background(), "+2290197000000", "x")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("erreur HTTP attendue, obtenu %v", err)
	}
	if strings.Contains(err.Error(), "p&ss") {
		t.Fatal("le mot de passe ne doit jamais apparaître dans l'erreur")
	}
}

func TestRegisteredSMSDriver(t *testing.T) {
	RegisterSMSDriver("test-fournisseur", func(config.SMSConfig) (SMSSender, error) { return &recordingSMS{}, nil })
	s, err := newSMSDriver(config.SMSConfig{Driver: "test-fournisseur"})
	if err != nil || s == nil {
		t.Fatalf("pilote enregistré introuvable : %v", err)
	}
	if _, err := newSMSDriver(config.SMSConfig{Driver: "inconnu"}); err == nil {
		t.Fatal("pilote inconnu : erreur attendue")
	}
}

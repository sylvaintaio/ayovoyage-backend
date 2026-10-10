package notify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOTPEmail(t *testing.T) {
	brand := Brand{Name: "BUS-VOYAGE", WebURL: "https://busvoyage.bj", SupportPhone: "+229 01 00 00 00 00", WhatsApp: "+229 01 00 00 00 00", SupportEmail: "contact@busvoyage.bj"}
	m := OTPEmail(brand, "mariam@exemple.bj", "424561", 5)
	if !strings.HasPrefix(m.Subject, "424561 ") {
		t.Fatalf("le code doit être en tête du sujet (visible dans la notification) : %q", m.Subject)
	}
	for _, want := range []string{">424561<", "5&nbsp;minutes", "Votre code : 424561", "https://wa.me/2290100000000"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("HTML sans %q", want)
		}
	}
	if !strings.Contains(m.Text, "424561") {
		t.Error("la version texte doit contenir le code")
	}
	dump(t, "otp.html", m.HTML)
}

func TestBookingEmailEscapesHTML(t *testing.T) {
	m := BookingEmail(Brand{Name: "Trans Express"}, "a@b.bj", BookingView{
		CustomerName: "<script>x</script>", Reference: "BV7K3M9QXT", Origin: "Cotonou", Destination: "Parakou",
		DepartureDate: "vendredi 9 octobre 2026", DepartureTime: "07:00", Agency: "Cotonou — Jonquet",
		Passengers: []Passenger{{Seat: "1A", Name: "Awa Dossou", Ticket: "TK-4HWDYWR3KV"}, {Seat: "1B", Name: "Koffi Dossou", Ticket: "TK-GYJNTBK4FJ"}},
		Amount:     25000, Currency: "XOF", Link: "https://exemple.bj/compte",
	})
	if strings.Contains(m.HTML, "<script>") {
		t.Fatal("le nom du client doit être échappé")
	}
	if !strings.Contains(m.HTML, "25\u202f000\u00a0F\u00a0CFA") || !strings.Contains(m.HTML, "2 passagers") {
		t.Fatal("montant ou aperçu mal formatés")
	}
	dump(t, "booking.html", m.HTML)
}

// dump écrit le rendu pour inspection visuelle (BV_DUMP_DIR=…).
func dump(t *testing.T, name, html string) {
	if dir := os.Getenv("BV_DUMP_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFormatMoneyAndDate(t *testing.T) {
	if got := FormatMoney(1250000, "XOF"); got != "1\u202f250\u202f000\u00a0F\u00a0CFA" {
		t.Errorf("FormatMoney = %q", got)
	}
	if got := FrenchDate(time.Date(2026, 10, 9, 7, 0, 0, 0, time.UTC)); got != "vendredi 9 octobre 2026" {
		t.Errorf("FrenchDate = %q", got)
	}
}

func TestWelcomeEmail(t *testing.T) {
	brand := Brand{Name: "BUS-VOYAGE", WebURL: "https://busvoyage.bj/", SupportPhone: "+229 01 00 00 00 00", SupportEmail: "contact@busvoyage.bj"}
	m := WelcomeEmail(brand, "awa@exemple.bj", "Awa <b>Dossou</b>")
	if m.Subject != "Bienvenue chez BUS-VOYAGE, Awa" {
		t.Errorf("sujet = %q", m.Subject)
	}
	for _, want := range []string{"Bienvenue à bord, Awa", `href="https://busvoyage.bj/compte"`, "Réserver mon premier voyage"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("HTML sans %q", want)
		}
	}
	if strings.Contains(m.HTML, "<b>") {
		t.Error("le nom doit être échappé")
	}
	if !strings.Contains(m.Text, "https://busvoyage.bj/compte") {
		t.Error("la version texte doit contenir le lien vers l'espace client")
	}
	dump(t, "welcome.html", m.HTML)

	if anon := WelcomeEmail(brand, "x@exemple.bj", "  "); anon.Subject != "Bienvenue chez BUS-VOYAGE" || !strings.Contains(anon.HTML, ">Bienvenue à bord<") {
		t.Errorf("sans nom : %q", anon.Subject)
	}
}

func TestReminderEmail(t *testing.T) {
	brand := Brand{Name: "Trans Express", WebURL: "https://busvoyage.bj"}
	m := ReminderEmail(brand, "awa@exemple.bj", ReminderView{CustomerName: "Awa <i>", Reference: "BV123", Origin: "Cotonou",
		Destination: "Parakou", When: "demain", DepartureDate: "vendredi 9 octobre 2026", DepartureTime: "07:00",
		Agency: "Cotonou — Jonquet", AgencyAddress: "Carrefour Jonquet", Seats: []string{"1A", "1B"}, BaggageKg: 20,
		Link: "https://busvoyage.bj/compte/reservations/x"})
	if m.Subject != "Rappel : Cotonou → Parakou demain à 07:00" {
		t.Errorf("sujet = %q", m.Subject)
	}
	for _, want := range []string{"Votre voyage, c’est demain", "1A, 1B", "20 kg", "Carrefour Jonquet", "Afficher mes billets"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("HTML sans %q", want)
		}
	}
	if strings.Contains(m.HTML, "<i>") || !strings.Contains(m.Text, "BV123") {
		t.Error("nom non échappé ou référence absente du texte")
	}
	dump(t, "reminder.html", m.HTML)
}

func TestDeliveryStatus(t *testing.T) {
	if s, _ := DeliveryStatus(LogEmail{}, nil); s != "simulated" {
		t.Errorf("pilote log : %s", s)
	}
	if s, _ := DeliveryStatus(SMTPEmail{}, nil); s != "sent" {
		t.Errorf("SMTP : %s", s)
	}
	if s, msg := DeliveryStatus(SMTPEmail{}, errors.New("refusé")); s != "failed" || msg != "refusé" {
		t.Errorf("erreur : %s %s", s, msg)
	}
}

func TestSameDayReminder(t *testing.T) {
	m := ReminderEmail(Brand{Name: "Trans"}, "a@b.bj", ReminderView{CustomerName: "Awa", Reference: "BV1", Origin: "Cotonou",
		Destination: "Parakou", When: "aujourd’hui", DepartureDate: "jeudi 8 octobre 2026", DepartureTime: "15:00",
		Agency: "Jonquet", Seats: []string{"1A"}, Link: "https://x", SameDay: true})
	if m.Subject != "Départ à 15:00 : Cotonou → Parakou" || !strings.Contains(m.HTML, "Votre bus part à 15:00") {
		t.Errorf("rappel du jour : %q", m.Subject)
	}
}

func TestReminderHours(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 8, h, 30, 0, 0, localTZ) }
	for h, want := range map[int][2]bool{5: {false, false}, 6: {false, true}, 7: {true, true}, 20: {true, true}, 21: {false, true}, 22: {false, false}} {
		if got := [2]bool{reminderHours(at(h)), dayReminderHours(at(h))}; got != want {
			t.Errorf("%d h 30 : veille/jour = %v, attendu %v", h, got, want)
		}
	}
}

func TestPayLaterMessages(t *testing.T) {
	v := PayLaterView{Event: "reserved", CustomerName: "Awa", Reference: "BVNWND3TE9", Origin: "Cotonou", Destination: "Natitingou",
		DepartureDate: "dimanche 11 octobre 2026", DepartureTime: "06h00", DateShort: "11/10", Amount: 15000, Currency: "XOF",
		PayBy: "samedi 10 octobre 2026 à 09h46", PayByShort: "10/10 09h46", Agencies: []string{"Cotonou — Jonquet (Rue 112)"},
		Link: "https://exemple.bj/compte/reservations/9748a2c5-500e-4345-9f2a-0a1b2c3d4e5f"}
	for _, event := range []string{"reserved", "reminder", "expired"} {
		v.Event = event
		sms := PrepareSMS(PayLaterSMS("Trans Express", v), true, 3)
		if n, unicode := SMSSegments(sms); n > 2 || unicode {
			t.Errorf("%s : %d SMS (unicode=%v) : %q", event, n, unicode, sms)
		}
		if !strings.Contains(sms, "BVNWND3TE9") {
			t.Errorf("%s : référence absente du SMS", event)
		}
		m := PayLaterEmail(Brand{Name: "Trans Express"}, "a@b.bj", v)
		if !strings.Contains(m.Subject, "BVNWND3TE9") {
			t.Errorf("%s : sujet %q", event, m.Subject)
		}
		if event != "expired" && (!strings.Contains(m.HTML, "Rue 112") || !strings.Contains(m.Text, "10 octobre")) {
			t.Errorf("%s : agences ou date limite absentes", event)
		}
	}
}

func TestReviewInvite(t *testing.T) {
	link := "https://exemple.bj/avis/AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	sms := PrepareSMS(ReviewInviteSMS("Trans Express", "Cotonou-Parakou", link), true, 3)
	if n, _ := SMSSegments(sms); n > 1 {
		t.Errorf("invitation SMS sur %d messages : %q", n, sms)
	}
	m := ReviewInviteEmail(Brand{Name: "Trans Express"}, "a@b.bj", "Awa Dossou", "Cotonou → Parakou", "vendredi 9 octobre 2026", link)
	if !strings.Contains(m.HTML, link) || !strings.Contains(m.Text, "Bonjour Awa") {
		t.Error("lien ou salutation absents")
	}
}

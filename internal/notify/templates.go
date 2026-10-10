package notify

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Brand : identité et coordonnées affichées dans les e-mails.
type Brand struct {
	Name         string
	WebURL       string
	SupportPhone string
	SupportEmail string
	WhatsApp     string
}

// Les e-mails sont construits en tableaux avec styles en ligne : c'est la seule
// mise en page fiable dans Gmail, Outlook, Yahoo et les applications mobiles.
// Aucune image ni police externe : rendu immédiat, même en réseau lent.
const (
	cInk    = "#121614"
	cMuted  = "#4b544f"
	cSoft   = "#7d8681"
	cLine   = "#e1e3df"
	cGreen  = "#0b4d3a"
	cNight  = "#0a2a20"
	cTint   = "#f0f5f3"
	cAccent = "#f5b700"
	cBg     = "#f4f4f1"
	font    = "-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"
)

func esc(s string) string { return html.EscapeString(s) }

// layout : structure commune. preheader = texte d'aperçu affiché dans la boîte de réception.
func layout(b Brand, title, preheader, body string) string {
	var contact []string
	if b.SupportPhone != "" {
		contact = append(contact, `Tél. <a href="tel:`+esc(strings.ReplaceAll(b.SupportPhone, " ", ""))+`" style="color:`+cMuted+`">`+esc(b.SupportPhone)+`</a>`)
	}
	if b.WhatsApp != "" {
		wa := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, b.WhatsApp)
		contact = append(contact, `<a href="https://wa.me/`+wa+`" style="color:`+cMuted+`">WhatsApp</a>`)
	}
	if b.SupportEmail != "" {
		contact = append(contact, `<a href="mailto:`+esc(b.SupportEmail)+`" style="color:`+cMuted+`">`+esc(b.SupportEmail)+`</a>`)
	}
	site := ""
	if b.WebURL != "" {
		site = `<a href="` + esc(b.WebURL) + `" style="color:` + cGreen + `;font-weight:bold;text-decoration:none">` + esc(strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(b.WebURL, "https://"), "http://"), "/")) + `</a>`
	}

	return `<!doctype html>
<html lang="fr" xmlns="http://www.w3.org/1999/xhtml">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light">
<meta name="supported-color-schemes" content="light">
<title>` + esc(title) + `</title>
</head>
<body style="margin:0;padding:0;background:` + cBg + `;font-family:` + font + `;color:` + cInk + `;-webkit-text-size-adjust:100%">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent">` + esc(preheader) + strings.Repeat("&#847;&zwnj;&nbsp;", 40) + `</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:` + cBg + `">
<tr><td align="center" style="padding:28px 12px">
<table role="presentation" width="560" cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:560px">
<tr><td style="padding:0 4px 16px">
  <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
    <td style="width:32px;height:29px;background:` + cGreen + `;border-bottom:3px solid ` + cAccent + `;border-radius:4px;color:#ffffff;font-size:13px;font-weight:bold;text-align:center;line-height:30px">` + esc(initials(b.Name)) + `</td>
    <td style="padding-left:10px;font-size:16px;font-weight:bold;color:` + cInk + `;letter-spacing:.3px">` + esc(b.Name) + `</td>
  </tr></table>
</td></tr>
<tr><td style="background:#ffffff;border:1px solid ` + cLine + `;border-radius:8px;overflow:hidden">
  <div style="height:6px;background:` + cAccent + `;border-top-left-radius:8px;border-top-right-radius:8px"></div>
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
  <tr><td style="padding:32px 32px 28px">` + body + `</td></tr>
  </table>
</td></tr>
<tr><td style="padding:20px 8px 0;text-align:center;font-size:12px;line-height:19px;color:` + cSoft + `">
  ` + strings.Join(contact, ` &nbsp;·&nbsp; `) + `<br>
  ` + site + `<br>
  <span style="color:` + cSoft + `">E-mail automatique, merci de ne pas y répondre. © ` + fmt.Sprint(time.Now().Year()) + ` ` + esc(b.Name) + `</span>
</td></tr>
</table>
</td></tr></table>
</body></html>`
}

func initials(name string) string {
	var out []rune
	for _, w := range strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '-' }) {
		if len(out) == 2 {
			break
		}
		out = append(out, []rune(strings.ToUpper(w))[0])
	}
	if len(out) == 0 {
		return "•"
	}
	return string(out)
}

func h1(text string) string {
	return `<h1 style="margin:0 0 8px;font-size:24px;line-height:30px;font-weight:800;color:` + cInk + `">` + esc(text) + `</h1>`
}

func para(text string) string {
	return `<p style="margin:0 0 20px;font-size:15px;line-height:23px;color:` + cMuted + `">` + text + `</p>`
}

func button(label, href string) string {
	return `<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:4px 0 24px"><tr>
<td style="border-radius:6px;background:` + cAccent + `"><a href="` + esc(href) + `" style="display:inline-block;padding:14px 26px;font-size:15px;font-weight:bold;color:` + cInk + `;text-decoration:none;border-radius:6px">` + esc(label) + `</a></td>
</tr></table>`
}

// FormatMoney : 25000 → « 25 000 F CFA ».
func FormatMoney(amount int64, currency string) string {
	s := fmt.Sprint(amount)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteString(" ")
		}
		b.WriteRune(r)
	}
	if currency == "XOF" || currency == "" {
		return b.String() + " F CFA"
	}
	return b.String() + " " + currency
}

// ---------------------------------------------------------------- Code de connexion

// OTPEmail : e-mail contenant le code de connexion. Le code figure en tête du
// sujet et de l'aperçu pour être lisible directement dans la notification.
func OTPEmail(b Brand, to, code string, validMinutes int) Email {
	text := fmt.Sprintf("Votre code de connexion %s : %s\n\nIl est valable %d minutes. Ne le communiquez à personne : nous ne vous le demanderons jamais.\n\nSi vous n'êtes pas à l'origine de cette demande, ignorez cet e-mail : aucun compte ne sera modifié.\n\n%s",
		b.Name, code, validMinutes, b.Name)
	body := h1("Votre code de connexion") +
		para(fmt.Sprintf("Saisissez ce code sur le site pour vous connecter. Il est valable <strong style=\"color:%s\">%d&nbsp;minutes</strong>.", cInk, validMinutes)) +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px"><tr>
<td align="center" style="padding:20px 12px;background:` + cTint + `;border:1px dashed ` + cGreen + `;border-radius:6px">
<span style="font-family:'SFMono-Regular',Menlo,Consolas,'Courier New',monospace;font-size:34px;line-height:40px;font-weight:bold;letter-spacing:10px;color:` + cGreen + `">` + esc(code) + `</span>
</td></tr></table>` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
<td style="padding:14px 16px;background:#fff3c4;border-left:4px solid #e0a600;border-radius:4px;font-size:13px;line-height:20px;color:#8a5a00">
<strong>Ne communiquez jamais ce code.</strong> Nos équipes ne vous le demanderont ni par téléphone, ni par SMS, ni sur WhatsApp.
</td></tr></table>
<p style="margin:20px 0 0;font-size:13px;line-height:20px;color:` + cSoft + `">Vous n’êtes pas à l’origine de cette demande ? Ignorez simplement cet e-mail : aucun compte ne sera modifié.</p>`
	return Email{
		To:      to,
		Subject: fmt.Sprintf("%s est votre code de connexion %s", code, b.Name),
		Text:    text,
		HTML:    layout(b, "Code de connexion", fmt.Sprintf("Votre code : %s — valable %d minutes.", code, validMinutes), body),
	}
}

// ---------------------------------------------------------------- Alerte de sécurité (personnel)

// SecurityEmail : changement de sécurité sur un compte du personnel.
func SecurityEmail(b Brand, to, name, title, message string) Email {
	when := FrenchDate(time.Now().In(time.FixedZone("WAT", 3600))) + " à " + time.Now().In(time.FixedZone("WAT", 3600)).Format("15:04")
	body := h1(title) +
		para("Bonjour "+esc(name)+",") +
		para(esc(message)) +
		`<p style="margin:0;font-size:13px;line-height:20px;color:` + cSoft + `">Date : ` + esc(when) + `. Cet e-mail est envoyé automatiquement à chaque changement de sécurité sur votre compte.</p>`
	return Email{
		To:      to,
		Subject: "Sécurité de votre compte " + b.Name + " : " + strings.ToLower(title[:1]) + title[1:],
		Text:    fmt.Sprintf("Bonjour %s,\n\n%s\n\nDate : %s.\n\n%s", name, message, when, b.Name),
		HTML:    layout(b, title, message, body),
	}
}

// ---------------------------------------------------------------- Rappel la veille du voyage

type ReminderView struct {
	CustomerName  string
	Reference     string
	Origin        string
	Destination   string
	When          string // « demain » ou « aujourd'hui »
	DepartureDate string
	DepartureTime string
	Agency        string
	AgencyAddress string
	Seats         []string
	BaggageKg     int32
	Link          string
	SameDay       bool // rappel quelques heures avant le départ
}

// ReminderEmail : rappel envoyé la veille du départ.
func ReminderEmail(b Brand, to string, v ReminderView) Email {
	seats := strings.Join(v.Seats, ", ")
	row := func(label, value string) string {
		return `<tr><td style="padding:8px 0;border-top:1px solid ` + cLine + `;font-size:13px;color:` + cSoft + `;width:120px">` + esc(label) +
			`</td><td style="padding:8px 0;border-top:1px solid ` + cLine + `;font-size:14px;font-weight:bold;color:` + cInk + `">` + esc(value) + `</td></tr>`
	}
	place := v.Agency
	if v.AgencyAddress != "" {
		place += " — " + v.AgencyAddress
	}
	checklist := []string{
		"Arrivez à l’agence au moins 30 minutes avant le départ.",
		"Présentez le QR code de chaque passager, sur votre téléphone ou imprimé.",
		"Munissez-vous d’une pièce d’identité.",
	}
	if v.BaggageKg > 0 {
		checklist = append(checklist, fmt.Sprintf("Bagage en soute inclus : %d kg par passager.", v.BaggageKg))
	}
	var items strings.Builder
	for _, c := range checklist {
		items.WriteString(`<tr><td valign="top" style="padding:0 10px 8px 0;color:` + cGreen + `;font-weight:bold">✓</td><td style="padding:0 0 8px;font-size:14px;line-height:21px;color:` + cMuted + `">` + esc(c) + `</td></tr>`)
	}
	title := "Votre voyage, c’est " + v.When
	subject := fmt.Sprintf("Rappel : %s → %s %s à %s", v.Origin, v.Destination, v.When, v.DepartureTime)
	if v.SameDay {
		title = "Votre bus part à " + v.DepartureTime
		subject = fmt.Sprintf("Départ à %s : %s → %s", v.DepartureTime, v.Origin, v.Destination)
	}
	body := h1(title) +
		para("Bonjour "+esc(v.CustomerName)+", voici l’essentiel pour votre départ "+esc(v.Origin)+" → "+esc(v.Destination)+".") +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px">` +
		row("Départ", v.DepartureDate+" à "+v.DepartureTime) + row("Lieu", place) + row("Sièges", seats) + row("Référence", v.Reference) +
		`</table>` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 20px">` + items.String() + `</table>` +
		button("Afficher mes billets", v.Link)
	text := fmt.Sprintf("Bonjour %s,\n\nRappel : votre voyage %s → %s, c'est %s.\nDépart : %s à %s\nLieu : %s\nSièges : %s\nRéférence : %s\n\n- %s\n\nVos billets : %s\n\n%s",
		v.CustomerName, v.Origin, v.Destination, v.When, v.DepartureDate, v.DepartureTime, place, seats, v.Reference,
		strings.ReplaceAll(strings.Join(checklist, "\n- "), "’", "'"), v.Link, b.Name)
	return Email{
		To:      to,
		Subject: subject,
		Text:    text,
		HTML:    layout(b, title, fmt.Sprintf("Départ %s à %s, %s. Arrivez 30 minutes avant.", v.When, v.DepartureTime, v.Agency), body),
	}
}

// ---------------------------------------------------------------- Colis

// ParcelEmail : information sur un colis (dépôt, arrivée, remise).
func ParcelEmail(b Brand, to, subject, message, link string) Email {
	msg := strings.ToUpper(message[:1]) + message[1:]
	body := h1(subject) + para(esc(msg)) + button("Suivre le colis", link)
	return Email{
		To:      to,
		Subject: subject + " — " + b.Name,
		Text:    fmt.Sprintf("%s\n\nSuivi : %s\n\n%s", msg, link, b.Name),
		HTML:    layout(b, subject, msg, body),
	}
}

// ---------------------------------------------------------------- Bienvenue

// WelcomeEmail : envoyé une fois, après la création du compte client.
func WelcomeEmail(b Brand, to, name string) Email {
	first := strings.TrimSpace(name)
	if f := strings.Fields(first); len(f) > 0 {
		first = f[0]
	}
	hello, title, subject := "Bonjour,", "Bienvenue à bord", "Bienvenue chez "+b.Name
	if first != "" {
		hello = "Bonjour " + first + ","
		title = "Bienvenue à bord, " + first
		subject = fmt.Sprintf("Bienvenue chez %s, %s", b.Name, first)
	}
	web := strings.TrimRight(b.WebURL, "/")
	perks := []struct{ title, text string }{
		{"Réservez en quelques minutes", "Choisissez votre départ et votre siège, payez par Mobile Money ou par carte."},
		{"Vos billets toujours sur vous", "Chaque passager reçoit un billet avec QR code, à présenter à l’embarquement."},
		{"Voyagez et cumulez des avantages", "Chaque trajet vous rapporte des points de fidélité, convertibles en réductions."},
		{"Gagnez du temps avec vos proches", "Enregistrez les personnes avec qui vous voyagez pour réserver encore plus vite."},
	}
	var rows, lines strings.Builder
	for i, p := range perks {
		pad := "0 0 16px"
		if i == len(perks)-1 {
			pad = "0"
		}
		rows.WriteString(`<tr>
<td valign="top" style="padding:` + pad + `;width:38px"><table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td width="24" height="24" align="center" style="width:24px;height:24px;border-radius:4px;background:` + cAccent + `;color:` + cInk + `;font-size:12px;line-height:24px;font-weight:bold">` + fmt.Sprint(i+1) + `</td></tr></table></td>
<td valign="top" style="padding:` + pad + `;font-size:14px;line-height:21px;color:` + cMuted + `"><strong style="color:` + cInk + `">` + esc(p.title) + `</strong><br>` + esc(p.text) + `</td>
</tr>`)
		fmt.Fprintf(&lines, "- %s : %s\n", p.title, strings.ReplaceAll(p.text, "’", "'"))
	}

	body := h1(title) +
		para(esc(hello)+" votre compte "+esc(b.Name)+" est prêt. Merci de nous faire confiance pour vos prochains voyages.") +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px;padding:20px;background:` + cBg + `;border-radius:8px">` + rows.String() + `</table>` +
		button("Réserver mon premier voyage", web+"/") +
		`<p style="margin:0;font-size:13px;line-height:20px;color:` + cSoft + `">Retrouvez à tout moment vos réservations, billets et messages dans <a href="` + esc(web+"/compte") + `" style="color:` + cGreen + `;font-weight:bold">votre espace client</a>. Pour vous connecter, il suffit de saisir votre adresse : nous vous envoyons un code, sans mot de passe à retenir.</p>`

	text := fmt.Sprintf("%s\n\nVotre compte %s est prêt. Merci de nous faire confiance pour vos prochains voyages.\n\n%s\nRéserver un voyage : %s/\nVotre espace client : %s/compte\n\n%s",
		hello, b.Name, lines.String(), web, web, b.Name)
	return Email{
		To:      to,
		Subject: subject,
		Text:    text,
		HTML:    layout(b, "Bienvenue", "Votre compte est prêt : réservez, retrouvez vos billets et cumulez des avantages.", body),
	}
}

// ---------------------------------------------------------------- Réservation confirmée

type Passenger struct {
	Seat, Name, Ticket string
}

type BookingView struct {
	InvoiceAttached bool // facture PDF jointe
	CustomerName    string
	Reference       string
	Origin          string
	Destination     string
	DepartureDate   string // « vendredi 9 octobre 2026 »
	DepartureTime   string // « 07:00 »
	Agency          string
	Passengers      []Passenger
	Amount          int64
	Currency        string
	Link            string
	Changed         bool   // réservation issue d'une modification de billet
	OldReference    string // réservation remplacée
}

// BookingEmail : confirmation de réservation, présentée comme un billet.
func BookingEmail(b Brand, to string, v BookingView) Email {
	var seats []string
	var rows strings.Builder
	for _, p := range v.Passengers {
		seats = append(seats, p.Seat)
		rows.WriteString(`<tr>
<td style="padding:10px 0;border-top:1px solid ` + cLine + `;font-size:14px;font-weight:bold;color:` + cInk + `;width:56px">` + esc(p.Seat) + `</td>
<td style="padding:10px 0;border-top:1px solid ` + cLine + `;font-size:14px;color:` + cInk + `">` + esc(p.Name) + `</td>
<td style="padding:10px 0;border-top:1px solid ` + cLine + `;font-size:12px;color:` + cSoft + `;text-align:right">` + esc(p.Ticket) + `</td>
</tr>`)
	}
	amount := FormatMoney(v.Amount, v.Currency)

	text := fmt.Sprintf("Bonjour %s,\n\nVotre réservation %s est confirmée.\n\nTrajet : %s → %s\nDépart : %s à %s (%s)\nSièges : %s\nMontant payé : %s\n\nVos billets (QR code) : %s\n\nPrésentez-vous à l'agence au moins 30 minutes avant le départ.\n\n%s",
		v.CustomerName, v.Reference, v.Origin, v.Destination, v.DepartureDate, v.DepartureTime, v.Agency,
		strings.Join(seats, ", "), amount, v.Link, b.Name)
	if v.InvoiceAttached {
		text = strings.Replace(text, "Présentez-vous", "Votre facture est jointe à cet e-mail (PDF).\n\nPrésentez-vous", 1)
	}

	ticket := `<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px;border:1px solid ` + cLine + `;border-radius:8px">
<tr><td style="padding:18px 20px 6px">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td style="font-size:12px;font-weight:bold;letter-spacing:1.5px;color:` + cGreen + `">` + esc(strings.ToUpper(b.Name)) + `</td>
    <td style="text-align:right;font-size:12px;color:` + cSoft + `">Réf. <strong style="color:` + cInk + `">` + esc(v.Reference) + `</strong></td>
  </tr></table>
</td></tr>
<tr><td style="padding:10px 20px 18px">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td style="vertical-align:bottom">
      <div style="font-size:12px;color:` + cSoft + `">Départ</div>
      <div style="font-size:20px;line-height:26px;font-weight:800;color:` + cInk + `">` + esc(v.Origin) + `</div>
    </td>
    <td style="vertical-align:bottom;text-align:center;padding:0 8px;color:` + cAccent + `;font-size:20px;line-height:26px">&rarr;</td>
    <td style="vertical-align:bottom;text-align:right">
      <div style="font-size:12px;color:` + cSoft + `">Arrivée</div>
      <div style="font-size:20px;line-height:26px;font-weight:800;color:` + cInk + `">` + esc(v.Destination) + `</div>
    </td>
  </tr></table>
</td></tr>
<tr><td style="padding:14px 20px;border-top:2px dashed ` + cLine + `;background:` + cTint + `">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
    <td style="font-size:12px;color:` + cSoft + `">Date<br><strong style="font-size:14px;color:` + cInk + `">` + esc(v.DepartureDate) + `</strong></td>
    <td style="font-size:12px;color:` + cSoft + `">Heure<br><strong style="font-size:14px;color:` + cInk + `">` + esc(v.DepartureTime) + `</strong></td>
    <td style="font-size:12px;color:` + cSoft + `;text-align:right">Payé<br><strong style="font-size:14px;color:` + cGreen + `">` + esc(amount) + `</strong></td>
  </tr></table>
</td></tr>
</table>`

	title, intro := "Votre voyage est réservé", "Bonjour "+esc(v.CustomerName)+", votre paiement est bien reçu. Voici le récapitulatif de votre réservation."
	subject := fmt.Sprintf("Billet confirmé : %s → %s, %s à %s", v.Origin, v.Destination, v.DepartureDate, v.DepartureTime)
	if v.Changed {
		title = "Votre voyage est modifié"
		intro = "Bonjour " + esc(v.CustomerName) + ", votre changement de départ est confirmé. Les billets de la réservation " +
			esc(v.OldReference) + " ne sont plus valables : utilisez uniquement les nouveaux billets ci-dessous."
		subject = fmt.Sprintf("Billet modifié : %s → %s, %s à %s", v.Origin, v.Destination, v.DepartureDate, v.DepartureTime)
		text = strings.Replace(text, "Votre réservation "+v.Reference+" est confirmée.",
			"Votre réservation est modifiée : "+v.Reference+" remplace "+v.OldReference+". Les anciens billets ne sont plus valables.", 1)
	}
	body := h1(title) +
		para(intro) +
		ticket +
		`<p style="margin:0 0 4px;font-size:13px;font-weight:bold;color:` + cInk + `">Passagers</p>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px">` + rows.String() + `</table>` +
		button("Afficher mes billets", v.Link) +
		invoiceNote(v.InvoiceAttached) +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
<td style="padding:14px 16px;background:` + cBg + `;border-radius:6px;font-size:13px;line-height:20px;color:` + cMuted + `">
<strong style="color:` + cInk + `">Le jour du départ :</strong> présentez-vous à l’agence ` + esc(v.Agency) + ` au moins 30 minutes avant, avec le QR code de chaque passager (sur votre téléphone ou imprimé).
</td></tr></table>`

	return Email{
		To:      to,
		Subject: subject,
		Text:    text,
		HTML:    layout(b, title, fmt.Sprintf("Réservation %s · %s · %s", v.Reference, plural(len(v.Passengers), "passager", "passagers"), amount), body),
	}
}

func plural(n int, one, many string) string {
	if n > 1 {
		return fmt.Sprintf("%d %s", n, many)
	}
	return fmt.Sprintf("%d %s", n, one)
}

var (
	frDays   = []string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}
	frMonths = []string{"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"}
)

// FrenchDate : « vendredi 9 octobre 2026 ».
func FrenchDate(t time.Time) string {
	return fmt.Sprintf("%s %d %s %d", frDays[t.Weekday()], t.Day(), frMonths[t.Month()-1], t.Year())
}

// ---------------------------------------------------------------- Facture

func invoiceNote(attached bool) string {
	if !attached {
		return ""
	}
	return `<p style="margin:0 0 20px;font-size:13px;line-height:20px;color:` + cMuted + `">Votre facture est jointe à cet e-mail (PDF). Vous la retrouvez aussi dans votre espace client.</p>`
}

// InvoiceView : facture ou avoir envoyé avec son PDF en pièce jointe.
type InvoiceView struct {
	CustomerName string
	Number       string
	CreditNote   bool
	Reference    string
	Amount       int64
	Currency     string
	IssuedAt     time.Time
	Link         string // espace client (factures)
}

func InvoiceEmail(b Brand, to string, v InvoiceView) Email {
	kind, title := "facture", "Votre facture"
	intro := "Merci pour votre paiement. Vous trouverez votre facture en pièce jointe (PDF)."
	if v.CreditNote {
		kind, title = "avoir", "Votre avoir"
		intro = "Votre remboursement a été versé. Vous trouverez la facture d’avoir correspondante en pièce jointe (PDF)."
	}
	hello := "Bonjour,"
	if name := strings.TrimSpace(v.CustomerName); name != "" {
		hello = "Bonjour " + name + ","
	}
	amount := FormatMoney(v.Amount, v.Currency)
	subject := fmt.Sprintf("%s %s — %s", strings.ToUpper(kind[:1])+kind[1:], v.Number, b.Name)
	rows := [][2]string{{"Numéro", v.Number}, {"Date", formatDate(v.IssuedAt)}, {"Référence", v.Reference}, {"Montant", amount}}
	var table, lines strings.Builder
	for _, r := range rows {
		table.WriteString(`<tr><td style="padding:6px 0;font-size:14px;color:` + cMuted + `">` + esc(r[0]) + `</td><td align="right" style="padding:6px 0;font-size:14px;font-weight:bold;color:` + cInk + `">` + esc(r[1]) + `</td></tr>`)
		fmt.Fprintf(&lines, "%s : %s\n", r[0], r[1])
	}
	body := h1(title) +
		para(esc(hello)) + para(esc(intro)) +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px;padding:16px 20px;background:` + cBg + `;border-radius:8px">` + table.String() + `</table>`
	text := fmt.Sprintf("%s\n\n%s\n\n%s", hello, strings.ReplaceAll(intro, "’", "'"), lines.String())
	if v.Link != "" {
		body += button("Mes factures", v.Link)
		text += "\nToutes vos factures : " + v.Link + "\n"
	}
	return Email{
		To:      to,
		Subject: subject,
		Text:    text + "\n" + b.Name,
		HTML:    layout(b, title, fmt.Sprintf("%s %s · %s", strings.ToUpper(kind[:1])+kind[1:], v.Number, amount), body),
	}
}

// ---------------------------------------------------------------- Payer en agence

// PayLaterView : réservation à régler en agence avant une date limite.
type PayLaterView struct {
	Event         string // reserved | reminder | expired
	CustomerName  string
	Reference     string
	Origin        string
	Destination   string
	DepartureDate string
	DepartureTime string
	Amount        int64
	Currency      string
	PayBy         string // « samedi 10 octobre 2026 à 18h00 » (e-mail)
	PayByShort    string // « 10/10 18h00 » (SMS)
	DateShort     string // « 10/10 » (SMS)
	Agencies      []string
	Link          string
}

func PayLaterEmail(b Brand, to string, v PayLaterView) Email {
	amount := FormatMoney(v.Amount, v.Currency)
	trip := v.Origin + " → " + v.Destination
	var title, intro, subject, preheader string
	switch v.Event {
	case "reminder":
		title = "Pensez à régler votre réservation"
		intro = "Vos places sont encore bloquées, mais plus pour longtemps : réglez votre réservation avant le " + v.PayBy + ", en agence ou en ligne."
		subject = fmt.Sprintf("Rappel : réservation %s à payer avant le %s", v.Reference, v.PayBy)
		preheader = "Sans paiement, vos places seront libérées le " + v.PayBy + "."
	case "expired":
		title = "Vos places ont été libérées"
		intro = "Votre réservation " + v.Reference + " n'a pas été réglée avant la date limite : les places ont été remises en vente. Vous pouvez réserver à nouveau si des places sont encore disponibles."
		subject = fmt.Sprintf("Réservation %s expirée (non payée)", v.Reference)
		preheader = "La réservation " + v.Reference + " n'a pas été payée à temps."
	default:
		title = "Vos places sont réservées"
		intro = "Réglez votre réservation avant le " + v.PayBy + " dans l'une de nos agences, en donnant la référence ci-dessous. Vous pouvez aussi payer en ligne depuis votre espace."
		subject = fmt.Sprintf("Réservation %s : à payer avant le %s", v.Reference, v.PayBy)
		preheader = "Référence " + v.Reference + " · " + amount + " à régler avant le " + v.PayBy
	}
	row := func(label, value string) string {
		return `<tr><td style="padding:8px 0;border-top:1px solid ` + cLine + `;font-size:13px;color:` + cSoft + `;width:130px">` + esc(label) +
			`</td><td style="padding:8px 0;border-top:1px solid ` + cLine + `;font-size:14px;font-weight:bold;color:` + cInk + `">` + esc(value) + `</td></tr>`
	}
	body := h1(title) + para("Bonjour "+esc(v.CustomerName)+", "+esc(intro))
	text := fmt.Sprintf("Bonjour %s,\n\n%s\n\nRéférence : %s\nTrajet : %s, %s à %s\nMontant : %s\n", v.CustomerName, intro,
		v.Reference, trip, v.DepartureDate, v.DepartureTime, amount)
	if v.Event != "expired" {
		body += `<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 20px">` +
			row("Référence", v.Reference) + row("Trajet", trip) + row("Départ", v.DepartureDate+" à "+v.DepartureTime) +
			row("Montant", amount) + row("À payer avant", v.PayBy) + `</table>`
		text += "À payer avant : " + v.PayBy + "\n"
		if len(v.Agencies) > 0 {
			var list strings.Builder
			for _, a := range v.Agencies {
				list.WriteString(`<li style="margin:0 0 4px">` + esc(a) + `</li>`)
			}
			body += `<p style="margin:0 0 6px;font-size:13px;font-weight:bold;color:` + cInk + `">Où payer</p>` +
				`<ul style="margin:0 0 20px;padding-left:18px;font-size:14px;line-height:21px;color:` + cMuted + `">` + list.String() + `</ul>`
			text += "\nOù payer :\n- " + strings.Join(v.Agencies, "\n- ") + "\n"
		}
		body += button("Payer en ligne", v.Link)
		text += "\nPayer en ligne : " + v.Link + "\n"
	} else {
		body += button("Réserver à nouveau", strings.TrimSuffix(b.WebURL, "/")+"/")
	}
	return Email{To: to, Subject: subject, Text: text + "\n" + b.Name, HTML: layout(b, title, preheader, body)}
}

// PayLaterSMS : version courte (160 caractères visés après suppression des accents).
func PayLaterSMS(company string, v PayLaterView) string {
	amount := FormatMoney(v.Amount, v.Currency)
	switch v.Event {
	case "reminder":
		return fmt.Sprintf("%s : rappel, reservation %s (%s) a payer avant le %s en agence, sinon places liberees. %s", company, v.Reference, amount, v.PayByShort, v.Link)
	case "expired":
		return fmt.Sprintf("%s : reservation %s non payee a temps, places liberees. Reservez a nouveau sur le site ou en agence.", company, v.Reference)
	}
	return fmt.Sprintf("%s : places reservees, ref %s, %s %s %s. Payez %s en agence avant le %s. %s", company, v.Reference,
		v.Origin+"-"+v.Destination, v.DateShort, v.DepartureTime, amount, v.PayByShort, v.Link)
}

// ---------------------------------------------------------------- Avis

// ReviewInviteEmail : invitation à noter le voyage, envoyée après l'arrivée.
func ReviewInviteEmail(b Brand, to, name, trip, date, link string) Email {
	hello := "Bonjour,"
	if f := strings.Fields(name); len(f) > 0 {
		hello = "Bonjour " + f[0] + ","
	}
	title := "Comment s’est passé votre voyage ?"
	body := h1(title) +
		para(esc(hello)+" merci d’avoir voyagé avec nous ("+esc(trip)+", "+esc(date)+"). Votre avis nous aide à améliorer nos services : il ne prend qu’une minute.") +
		button("Donner mon avis", link) +
		`<p style="margin:0;font-size:13px;line-height:20px;color:` + cSoft + `">Un problème pendant le voyage ? Dites-le-nous dans votre avis : notre service client vous recontactera.</p>`
	text := fmt.Sprintf("%s\n\nMerci d'avoir voyagé avec nous (%s, %s). Votre avis nous aide à améliorer nos services : %s\n\n%s",
		hello, trip, date, link, b.Name)
	return Email{To: to, Subject: "Votre avis sur votre voyage " + trip, Text: text,
		HTML: layout(b, title, "Une minute pour noter votre voyage "+trip+".", body)}
}

// ReviewInviteSMS : version courte de l'invitation.
func ReviewInviteSMS(company, trip, link string) string {
	return fmt.Sprintf("%s : merci d'avoir voyage avec nous (%s). Votre avis en 1 minute : %s", company, trip, link)
}

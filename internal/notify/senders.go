// Package notify envoie les SMS, e-mails (et plus tard push / WhatsApp).
package notify

import (
	"busvoyage/internal/push"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"busvoyage/internal/config"
)

// SMSSender : envoi d'un SMS. Pilotes : log (aucun envoi), http (API REST du
// fournisseur décrite par SMS_HTTP_*), ou adaptateur dédié (voir sms.go).
type SMSSender interface {
	SendSMS(ctx context.Context, to, message string) error
}

// Email : message à envoyer. HTML est facultatif (version texte toujours fournie).
type Email struct {
	To          string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
}

// Attachment : pièce jointe (facture PDF…).
type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

type EmailSender interface {
	SendEmail(ctx context.Context, msg Email) error
}

// LogSMS écrit les SMS dans les journaux (développement).
type LogSMS struct{}

func (LogSMS) SendSMS(_ context.Context, to, message string) error {
	slog.Info("SMS (driver log)", "to", to, "message", message)
	return nil
}

type LogEmail struct{}

func (LogEmail) SendEmail(_ context.Context, msg Email) error {
	names := make([]string, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		names = append(names, fmt.Sprintf("%s (%d octets)", a.Name, len(a.Data)))
	}
	slog.Info("E-mail (driver log)", "to", msg.To, "subject", msg.Subject, "body", msg.Text, "pieces_jointes", names)
	return nil
}

// SMTPEmail envoie par SMTP authentifié.
//   - port 465 : TLS implicite (SSL), configuration habituelle de LWS ;
//   - port 587 ou 25 : STARTTLS si le serveur le propose.
type SMTPEmail struct {
	Host, User, Pass, From string
	Port                   int
	Timeout                time.Duration
}

func (s SMTPEmail) SendEmail(ctx context.Context, msg Email) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("EMAIL_FROM invalide: %w", err)
	}
	to, err := mail.ParseAddress(msg.To)
	if err != nil {
		return fmt.Errorf("destinataire invalide: %w", err)
	}
	data, err := buildMessage(from, to, msg)
	if err != nil {
		return err
	}

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	dialer := &net.Dialer{Timeout: timeout}
	tlsCfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	if s.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connexion SMTP %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if s.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("STARTTLS: %w", err)
			}
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Pass, s.Host)); err != nil {
			return fmt.Errorf("authentification SMTP: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMessage construit le message MIME : texte seul ou multipart/alternative
// (texte + HTML), enveloppé dans un multipart/mixed s'il y a des pièces jointes.
func buildMessage(from, to *mail.Address, msg Email) ([]byte, error) {
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return nil, fmt.Errorf("sujet d'e-mail invalide")
	}
	domain := from.Address[strings.LastIndex(from.Address, "@")+1:]
	var b strings.Builder
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", to.String())
	header("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+randomHex(16)+"@"+domain+">")
	header("MIME-Version", "1.0")

	if len(msg.Attachments) == 0 {
		writeBody(&b, msg)
		return []byte(b.String()), nil
	}
	mixed := randomHex(12)
	b.WriteString(`Content-Type: multipart/mixed; boundary="` + mixed + `"` + "\r\n\r\n")
	b.WriteString("--" + mixed + "\r\n")
	writeBody(&b, msg)
	for _, a := range msg.Attachments {
		if strings.ContainsAny(a.Name, "\r\n\"") {
			return nil, fmt.Errorf("nom de pièce jointe invalide")
		}
		typ := a.ContentType
		if typ == "" {
			typ = "application/octet-stream"
		}
		name := mime.QEncoding.Encode("utf-8", a.Name)
		b.WriteString("\r\n--" + mixed + "\r\n")
		b.WriteString("Content-Type: " + typ + `; name="` + name + `"` + "\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		b.WriteString(`Content-Disposition: attachment; filename="` + name + `"` + "\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	b.WriteString("--" + mixed + "--\r\n")
	return []byte(b.String()), nil
}

// writeBody écrit l'en-tête Content-Type et le corps (texte, ou texte + HTML).
func writeBody(b *strings.Builder, msg Email) {
	if msg.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + msg.Text + "\r\n")
		return
	}
	alt := randomHex(12)
	b.WriteString(`Content-Type: multipart/alternative; boundary="` + alt + `"` + "\r\n\r\n")
	for _, part := range []struct{ typ, body string }{{"text/plain", msg.Text}, {"text/html", msg.HTML}} {
		b.WriteString("--" + alt + "\r\n")
		b.WriteString("Content-Type: " + part.typ + "; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(part.body + "\r\n")
	}
	b.WriteString("--" + alt + "--\r\n")
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// BrandFromConfig : identité des e-mails à partir de la configuration.
func BrandFromConfig(cfg config.Config) Brand {
	name, web, phone, email, wa := cfg.Brand()
	return Brand{Name: name, WebURL: web, SupportPhone: phone, SupportEmail: email, WhatsApp: wa}
}

func NewSenders(cfg config.Config) (SMSSender, EmailSender, error) {
	driver, err := newSMSDriver(cfg.SMS)
	if err != nil {
		return nil, nil, err
	}
	sms := preparedSMS{inner: driver, countryCode: cfg.DefaultCountryCode, ascii: cfg.SMS.ASCII, maxSegments: cfg.SMS.MaxSegments}
	if cfg.SMS.Driver == "log" && !cfg.IsDev() {
		slog.Warn("SMS_DRIVER=log : aucun SMS n'est réellement envoyé (messages notés « simulés »)")
	}
	var email EmailSender
	switch cfg.EmailDriver {
	case "log":
		email = LogEmail{}
	case "smtp":
		if cfg.SMTPHost == "" {
			return nil, nil, fmt.Errorf("SMTP_HOST est obligatoire avec EMAIL_DRIVER=smtp")
		}
		email = SMTPEmail{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.EmailFrom}
	default:
		return nil, nil, fmt.Errorf("driver e-mail inconnu: %s", cfg.EmailDriver)
	}
	return sms, email, nil
}

// DeliveryStatus : statut enregistré dans l'historique des messages.
// « simulated » quand le pilote « log » est actif (mode test) : rien n'est
// réellement parti, l'historique ne doit pas laisser croire le contraire.
func DeliveryStatus(sender any, err error) (status, errMsg string) {
	switch {
	case err != nil:
		return "failed", err.Error()
	case isLogSender(sender):
		return "simulated", "mode test : pilote log, aucun envoi réel"
	default:
		return "sent", ""
	}
}

func isLogSender(sender any) bool {
	if w, ok := sender.(interface{ Unwrap() SMSSender }); ok {
		sender = w.Unwrap()
	}
	switch sender.(type) {
	case LogEmail, *LogEmail, LogSMS, *LogSMS, push.Log, *push.Log:
		return true
	}
	return false
}

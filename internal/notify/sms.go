package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"busvoyage/internal/config"
	"busvoyage/internal/platform/phone"
)

// ---------------------------------------------------------------- Registre de pilotes

// SMSDriverFactory construit un pilote SMS dédié à un fournisseur. À utiliser
// quand l'API du fournisseur ne se décrit pas avec le pilote « http » générique
// (signature particulière, échange de jeton, etc.) :
//
//	func init() { notify.RegisterSMSDriver("monfournisseur", newMonFournisseur) }
//
// puis SMS_DRIVER=monfournisseur.
type SMSDriverFactory func(cfg config.SMSConfig) (SMSSender, error)

var (
	smsDriversMu sync.RWMutex
	smsDrivers   = map[string]SMSDriverFactory{}
)

func RegisterSMSDriver(name string, factory SMSDriverFactory) {
	smsDriversMu.Lock()
	defer smsDriversMu.Unlock()
	smsDrivers[name] = factory
}

func newSMSDriver(cfg config.SMSConfig) (SMSSender, error) {
	switch cfg.Driver {
	case "log":
		return LogSMS{}, nil
	case "http":
		return NewHTTPSMS(cfg), nil
	}
	smsDriversMu.RLock()
	factory, ok := smsDrivers[cfg.Driver]
	smsDriversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("SMS_DRIVER : pilote SMS inconnu %q (log, http ou adaptateur enregistré)", cfg.Driver)
	}
	return factory(cfg)
}

// ---------------------------------------------------------------- Préparation des messages

// preparedSMS enveloppe le pilote : numéro au format international, texte
// nettoyé (accents retirés si SMS_ASCII) et limité à SMS_MAX_SEGMENTS SMS.
// Tous les envois de l'application passent par lui.
type preparedSMS struct {
	inner       SMSSender
	countryCode string
	ascii       bool
	maxSegments int
}

func (p preparedSMS) SendSMS(ctx context.Context, to, message string) error {
	num, err := phone.Normalize(to, p.countryCode)
	if err != nil {
		return fmt.Errorf("SMS non envoyé : numéro %q invalide", phone.Mask(to))
	}
	return p.inner.SendSMS(ctx, num, PrepareSMS(message, p.ascii, p.maxSegments))
}

// Unwrap donne le pilote réel (statut « simulé » du pilote log).
func (p preparedSMS) Unwrap() SMSSender { return p.inner }

var smsReplacer = strings.NewReplacer(
	"é", "e", "è", "e", "ê", "e", "ë", "e", "É", "E", "È", "E", "Ê", "E", "Ë", "E",
	"à", "a", "â", "a", "ä", "a", "À", "A", "Â", "A", "Ä", "A",
	"î", "i", "ï", "i", "Î", "I", "Ï", "I",
	"ô", "o", "ö", "o", "Ô", "O", "Ö", "O",
	"ù", "u", "û", "u", "ü", "u", "Ù", "U", "Û", "U", "Ü", "U",
	"ç", "c", "Ç", "C", "œ", "oe", "Œ", "OE", "æ", "ae", "Æ", "AE", "ÿ", "y",
	"’", "'", "‘", "'", "« ", "\"", " »", "\"", "« ", "\"", " »", "\"", "«\u202f", "\"", "\u202f»", "\"", "«", "\"", "»", "\"", "“", "\"", "”", "\"",
	"–", "-", "—", "-", "…", "...", "→", "->", "•", "-", "·", "-", "€", "EUR",
	" ", " ", " ", " ", " ", " ",
)

// PrepareSMS nettoie un texte pour l'envoi par SMS et le tronque proprement
// au nombre maximal de SMS autorisé.
func PrepareSMS(message string, ascii bool, maxSegments int) string {
	text := strings.TrimSpace(message)
	if ascii {
		text = smsReplacer.Replace(text)
		text = strings.Map(func(r rune) rune {
			if r == '\n' || (r >= 0x20 && r < 0x7f) {
				return r
			}
			return -1 // tout autre caractère coûterait un SMS « unicode » (70 caractères)
		}, text)
	}
	if maxSegments < 1 {
		maxSegments = 1
	}
	if n, _ := SMSSegments(text); n <= maxSegments {
		return text
	}
	ellipsis := "…"
	if ascii {
		ellipsis = "..."
	}
	runes := []rune(text)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		candidate := strings.TrimRight(string(runes), " ") + ellipsis
		if n, _ := SMSSegments(candidate); n <= maxSegments {
			return candidate
		}
	}
	return ""
}

// gsmBasic : alphabet GSM 03.38 de base (1 caractère = 7 bits) ; gsmExtended
// coûte 2 caractères. Tout autre caractère impose l'encodage UCS-2.
const (
	gsmBasic    = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsmExtended = "^{}\\[~]|€\f"
)

// SMSSegments : nombre de SMS facturés pour ce texte, et s'il passe en Unicode.
func SMSSegments(text string) (segments int, unicode bool) {
	units := 0
	for _, r := range text {
		switch {
		case strings.ContainsRune(gsmBasic, r):
			units++
		case strings.ContainsRune(gsmExtended, r):
			units += 2
		default:
			unicode = true
		}
	}
	if unicode {
		n := utf8.RuneCountInString(text)
		if n <= 70 {
			return 1, true
		}
		return (n + 66) / 67, true
	}
	if units <= 160 {
		return 1, false
	}
	return (units + 152) / 153, false
}

// ---------------------------------------------------------------- Pilote HTTP générique

// HTTPSMS envoie par l'API REST du fournisseur, décrite entièrement par la
// configuration (SMS_HTTP_*). Variables utilisables dans l'URL et le corps :
// {{to}}, {{message}}, {{sender}}, {{token}}, {{user}}, {{password}}.
type HTTPSMS struct {
	cfg    config.SMSConfig
	client *http.Client
}

func NewHTTPSMS(cfg config.SMSConfig) HTTPSMS {
	return HTTPSMS{cfg: cfg, client: &http.Client{Timeout: cfg.HTTPTimeout}}
}

func (h HTTPSMS) SendSMS(ctx context.Context, to, message string) error {
	vars := map[string]string{
		"to":       formatPhone(to, h.cfg.HTTPPhoneFormat),
		"message":  message,
		"sender":   h.cfg.SenderID,
		"token":    h.cfg.HTTPToken,
		"user":     h.cfg.HTTPUser,
		"password": h.cfg.HTTPPassword,
	}
	endpoint := fill(h.cfg.HTTPURL, vars, url.QueryEscape)

	var body io.Reader
	contentType := ""
	switch h.cfg.HTTPFormat {
	case "json":
		body = strings.NewReader(fill(h.cfg.HTTPBody, vars, jsonEscape))
		contentType = "application/json"
	case "form":
		body = strings.NewReader(fill(h.cfg.HTTPBody, vars, url.QueryEscape))
		contentType = "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, h.cfg.HTTPMethod, endpoint, body)
	if err != nil {
		return errors.New("SMS : requête invalide (vérifier SMS_HTTP_URL)")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	switch h.cfg.HTTPAuth {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+h.cfg.HTTPToken)
	case "basic":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(h.cfg.HTTPUser+":"+h.cfg.HTTPPassword)))
	case "header":
		req.Header.Set(h.cfg.HTTPHeaderName, h.cfg.HTTPToken)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		// L'URL peut contenir des secrets : on ne renvoie que l'hôte.
		return fmt.Errorf("SMS : fournisseur injoignable (%s)", req.URL.Host)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	excerpt := strings.TrimSpace(string(reply))
	if len(excerpt) > 300 {
		excerpt = excerpt[:300] + "…"
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("SMS refusé par le fournisseur (HTTP %d) : %s", resp.StatusCode, excerpt)
	}
	if h.cfg.HTTPSuccess != "" && !strings.Contains(string(reply), h.cfg.HTTPSuccess) {
		return fmt.Errorf("SMS : réponse inattendue du fournisseur : %s", excerpt)
	}
	slog.Debug("SMS envoyé", "to", phone.Mask(to), "fournisseur", req.URL.Host)
	return nil
}

func fill(template string, vars map[string]string, escape func(string) string) string {
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{{"+k+"}}", escape(v))
	}
	return strings.NewReplacer(pairs...).Replace(template)
}

// jsonEscape : valeur à insérer entre guillemets dans un modèle JSON.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// formatPhone : +22997000000 → e164 « +22997000000 », digits « 22997000000 », national « 97000000 ».
func formatPhone(e164, format string) string {
	switch format {
	case "digits":
		return strings.TrimPrefix(e164, "+")
	case "national":
		// Bénin : indicatif 229 suivi de 10 chiffres (01…) ; ailleurs, retire l'indicatif à 3 chiffres.
		d := strings.TrimPrefix(e164, "+")
		if len(d) > 3 {
			return d[3:]
		}
		return d
	}
	return e164
}

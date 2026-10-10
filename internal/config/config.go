// Package config charge la configuration depuis les variables d'environnement.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultBrandName : nom de la plateforme si BRAND_NAME n'est pas défini.
// Le nom affiché partout (e-mails, factures, double authentification,
// documentation de l'API) vient de BRAND_NAME : c'est la seule valeur à changer.
const DefaultBrandName = "BUS-VOYAGE"

type Config struct {
	Env                  string // development | staging | production
	HTTPAddr             string
	PublicAPIURL         string // URL publique de l'API (callbacks paiement)
	PublicWebURL         string // URL du site web (liens dans les SMS/e-mails)
	DatabaseURL          string // rôle applicatif (soumis au RLS)
	MigrationDatabaseURL string // rôle propriétaire (migrations uniquement)
	AutoMigrate          bool

	CORSOrigins      []string
	TrustProxy       bool
	CookieDomain     string
	CookiePathPrefix string // préfixe sous lequel l'API est publiée (ex. /api derrière Caddy)
	CookieSecure     bool
	EnableDocs       bool

	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	OTPSecret       []byte

	TicketSigningKey   []byte // seed Ed25519 (32 octets)
	TicketSigningKeyID string
	TicketVerifyKeys   map[string][]byte // anciennes clés publiques encore acceptées (rotation)
	MFAEncryptionKey   []byte            // chiffrement des secrets TOTP (32 octets)

	DefaultCountryCode string // indicatif par défaut, ex. 229 (Bénin)
	BrandName          string // nom affiché dans les SMS et e-mails
	SupportPhone       string // coordonnées du service client (pied des e-mails)
	SupportEmail       string
	SupportWhatsApp    string
	OTPChannels        []string // canaux de connexion client : sms, email

	PaymentProviders     []string // fournisseurs actifs, ex. mock,fedapay
	FedaPaySecretKey     string
	FedaPayWebhookSecret string
	FedaPayEnv           string // sandbox | live

	SMS SMSConfig

	EmailDriver string // log | smtp
	SMTPHost    string
	SMTPPort    int
	SMTPUser    string
	SMTPPass    string
	EmailFrom   string
}

// SMSConfig : envoi des SMS. Le fournisseur se branche par configuration seule
// (pilote « http » générique) ou par un adaptateur Go dédié (notify.RegisterSMSDriver).
type SMSConfig struct {
	Driver      string // log (aucun envoi réel) | http | nom d'un adaptateur enregistré
	SenderID    string // nom d'expéditeur affiché (11 caractères au plus, à faire valider par le fournisseur)
	ASCII       bool   // retirer les accents : 160 caractères par SMS au lieu de 70
	MaxSegments int    // nombre maximal de SMS concaténés par message (coût)

	// Pilote « http » : API REST du fournisseur.
	HTTPURL         string        // peut contenir {{to}}, {{message}}, {{sender}}, {{token}}…
	HTTPMethod      string        // POST (défaut) | GET
	HTTPFormat      string        // json (défaut) | form | none (paramètres dans l'URL)
	HTTPBody        string        // modèle du corps, ex. {"from":"{{sender}}","to":"{{to}}","text":"{{message}}"}
	HTTPAuth        string        // none | bearer | basic | header
	HTTPToken       string        // jeton (bearer, header) — secret
	HTTPUser        string        // identifiant (basic)
	HTTPPassword    string        // mot de passe (basic) — secret
	HTTPHeaderName  string        // nom de l'en-tête pour HTTPAuth=header, ex. X-API-Key
	HTTPPhoneFormat string        // e164 (+22997…, défaut) | digits (22997…) | national (97…)
	HTTPSuccess     string        // texte attendu dans la réponse (facultatif ; sinon tout code 2xx vaut succès)
	HTTPTimeout     time.Duration // délai maximal d'un appel
}

func (c Config) IsDev() bool { return c.Env == "development" }

// AllowsSimulation : paiement simulé et données de démonstration autorisés
// (développement, et recette « staging » hébergée avec la sécurité de production).
func (c Config) AllowsSimulation() bool { return c.Env == "development" || c.Env == "staging" }

func Load() (Config, error) {
	c := Config{
		Env:                  env("APP_ENV", "development"),
		HTTPAddr:             env("HTTP_ADDR", ":8080"),
		PublicAPIURL:         strings.TrimRight(env("PUBLIC_API_URL", "http://localhost:8080"), "/"),
		PublicWebURL:         strings.TrimRight(env("PUBLIC_WEB_URL", "http://localhost:4200"), "/"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		MigrationDatabaseURL: os.Getenv("MIGRATION_DATABASE_URL"),
		AutoMigrate:          envBool("AUTO_MIGRATE", false),
		CORSOrigins:          envList("CORS_ORIGINS", "http://localhost:4200"),
		TrustProxy:           envBool("TRUST_PROXY", false),
		CookieDomain:         os.Getenv("COOKIE_DOMAIN"),
		CookiePathPrefix:     strings.TrimRight(os.Getenv("COOKIE_PATH_PREFIX"), "/"),
		EnableDocs:           envBool("ENABLE_DOCS", true),
		AccessTokenTTL:       envDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:      envDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		TicketSigningKeyID:   env("TICKET_SIGNING_KEY_ID", "k1"),
		DefaultCountryCode:   env("DEFAULT_COUNTRY_CODE", "229"),
		BrandName:            env("BRAND_NAME", DefaultBrandName),
		SupportPhone:         os.Getenv("SUPPORT_PHONE"),
		SupportEmail:         os.Getenv("SUPPORT_EMAIL"),
		SupportWhatsApp:      os.Getenv("SUPPORT_WHATSAPP"),
		OTPChannels:          envList("OTP_CHANNELS", "email"),
		PaymentProviders:     envList("PAYMENT_PROVIDERS", "mock"),
		FedaPaySecretKey:     os.Getenv("FEDAPAY_SECRET_KEY"),
		FedaPayWebhookSecret: os.Getenv("FEDAPAY_WEBHOOK_SECRET"),
		FedaPayEnv:           env("FEDAPAY_ENV", "sandbox"),
		SMS: SMSConfig{
			Driver:          env("SMS_DRIVER", "log"),
			SenderID:        os.Getenv("SMS_SENDER_ID"),
			ASCII:           envBool("SMS_ASCII", true),
			MaxSegments:     envInt("SMS_MAX_SEGMENTS", 3),
			HTTPURL:         os.Getenv("SMS_HTTP_URL"),
			HTTPMethod:      strings.ToUpper(env("SMS_HTTP_METHOD", "POST")),
			HTTPFormat:      env("SMS_HTTP_FORMAT", "json"),
			HTTPBody:        os.Getenv("SMS_HTTP_BODY"),
			HTTPAuth:        env("SMS_HTTP_AUTH", "none"),
			HTTPToken:       os.Getenv("SMS_HTTP_TOKEN"),
			HTTPUser:        os.Getenv("SMS_HTTP_USER"),
			HTTPPassword:    os.Getenv("SMS_HTTP_PASSWORD"),
			HTTPHeaderName:  env("SMS_HTTP_HEADER_NAME", "X-API-Key"),
			HTTPPhoneFormat: env("SMS_HTTP_PHONE_FORMAT", "e164"),
			HTTPSuccess:     os.Getenv("SMS_HTTP_SUCCESS"),
			HTTPTimeout:     envDuration("SMS_HTTP_TIMEOUT", 15*time.Second),
		},
		EmailDriver: env("EMAIL_DRIVER", "log"),
		// Les noms MAIL_* (format Laravel) sont acceptés en repli.
		SMTPHost:  firstEnv("SMTP_HOST", "MAIL_HOST"),
		SMTPPort:  envInt("SMTP_PORT", envInt("MAIL_PORT", 587)),
		SMTPUser:  firstEnv("SMTP_USER", "MAIL_USERNAME"),
		SMTPPass:  firstEnv("SMTP_PASSWORD", "MAIL_PASSWORD"),
		EmailFrom: emailFrom(),
	}
	c.CookieSecure = envBool("COOKIE_SECURE", !c.IsDev())

	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL est obligatoire"))
	}
	c.JWTSecret = []byte(os.Getenv("JWT_SECRET"))
	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET doit contenir au moins 32 caractères"))
	}
	c.OTPSecret = []byte(os.Getenv("OTP_SECRET"))
	if len(c.OTPSecret) < 32 {
		errs = append(errs, errors.New("OTP_SECRET doit contenir au moins 32 caractères"))
	}
	if raw := os.Getenv("TICKET_SIGNING_KEY"); raw != "" {
		seed, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(seed) != 32 {
			errs = append(errs, errors.New("TICKET_SIGNING_KEY doit être une seed Ed25519 de 32 octets encodée en base64"))
		}
		c.TicketSigningKey = seed
	} else if !c.IsDev() {
		errs = append(errs, errors.New("TICKET_SIGNING_KEY est obligatoire hors développement"))
	}
	if raw := os.Getenv("MFA_ENCRYPTION_KEY"); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) != 32 {
			errs = append(errs, errors.New("MFA_ENCRYPTION_KEY doit être une clé de 32 octets encodée en base64"))
		}
		c.MFAEncryptionKey = key
	} else if !c.IsDev() {
		errs = append(errs, errors.New("MFA_ENCRYPTION_KEY est obligatoire hors développement"))
	} else {
		sum := sha256.Sum256(append([]byte("bv-mfa-dev:"), c.JWTSecret...)) // développement uniquement
		c.MFAEncryptionKey = sum[:]
	}
	c.TicketVerifyKeys = map[string][]byte{}
	for _, item := range envList("TICKET_VERIFY_KEYS", "") {
		kid, b64, ok := strings.Cut(item, ":")
		pub, err := base64.StdEncoding.DecodeString(b64)
		if !ok || kid == "" || err != nil || len(pub) != 32 {
			errs = append(errs, fmt.Errorf("TICKET_VERIFY_KEYS : entrée invalide %q (format kid:clé_publique_base64)", item))
			continue
		}
		c.TicketVerifyKeys[kid] = pub
	}
	for _, ch := range c.OTPChannels {
		if ch != "sms" && ch != "email" {
			errs = append(errs, fmt.Errorf("OTP_CHANNELS : canal inconnu %q (sms, email)", ch))
		}
		// Sans fournisseur, un code de connexion par SMS n'arriverait jamais.
		if ch == "sms" && c.SMS.Driver == "log" && !c.IsDev() {
			errs = append(errs, errors.New("OTP_CHANNELS contient sms alors que SMS_DRIVER=log (aucun envoi réel) : brancher un fournisseur ou retirer sms"))
		}
	}
	errs = append(errs, c.SMS.validate()...)
	switch c.Env {
	case "development", "staging", "production":
	default:
		errs = append(errs, fmt.Errorf("APP_ENV : %q inconnu (development, staging, production)", c.Env))
	}
	if !c.AllowsSimulation() {
		for _, p := range c.PaymentProviders {
			if p == "mock" {
				errs = append(errs, errors.New("le fournisseur de paiement 'mock' est interdit en production"))
			}
		}
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("configuration invalide: %w", errors.Join(errs...))
	}
	return c, nil
}

// firstEnv renvoie la première variable non vide.
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// emailFrom : EMAIL_FROM, sinon « BRAND_NAME <MAIL_FROM_ADDRESS> » : le nom de
// l'expéditeur suit le nom de la marque (MAIL_FROM_NAME seulement à défaut).
func emailFrom() string {
	if v := firstEnv("EMAIL_FROM"); v != "" {
		return v
	}
	name := firstEnv("BRAND_NAME", "MAIL_FROM_NAME")
	if name == "" {
		name = DefaultBrandName
	}
	addr := firstEnv("MAIL_FROM_ADDRESS", "MAIL_USERNAME")
	if addr == "" {
		addr = "no-reply@example.com"
	}
	return (&mail.Address{Name: name, Address: addr}).String()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

func envInt(key string, def int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

func envDuration(key string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return def
	}
	return v
}

func envList(key, def string) []string {
	var out []string
	for _, s := range strings.Split(env(key, def), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Brand renvoie l'identité utilisée dans les e-mails.
func (c Config) Brand() (name, web, phone, email, whatsapp string) {
	return c.BrandName, c.PublicWebURL, c.SupportPhone, c.SupportEmail, c.SupportWhatsApp
}

func (s SMSConfig) validate() []error {
	var errs []error
	if len([]rune(s.SenderID)) > 11 {
		errs = append(errs, errors.New("SMS_SENDER_ID : 11 caractères au plus"))
	}
	if s.MaxSegments < 1 || s.MaxSegments > 10 {
		errs = append(errs, errors.New("SMS_MAX_SEGMENTS doit être compris entre 1 et 10"))
	}
	if s.Driver != "http" {
		return errs
	}
	if !strings.HasPrefix(s.HTTPURL, "https://") && !strings.HasPrefix(s.HTTPURL, "http://") {
		errs = append(errs, errors.New("SMS_HTTP_URL est obligatoire avec SMS_DRIVER=http"))
	}
	if s.HTTPMethod != "POST" && s.HTTPMethod != "GET" {
		errs = append(errs, fmt.Errorf("SMS_HTTP_METHOD : %q (POST ou GET)", s.HTTPMethod))
	}
	switch s.HTTPFormat {
	case "json", "form":
		if s.HTTPBody == "" {
			errs = append(errs, errors.New("SMS_HTTP_BODY est obligatoire avec SMS_HTTP_FORMAT=json ou form"))
		}
	case "none":
	default:
		errs = append(errs, fmt.Errorf("SMS_HTTP_FORMAT : %q (json, form ou none)", s.HTTPFormat))
	}
	switch s.HTTPAuth {
	case "none":
	case "bearer", "header":
		if s.HTTPToken == "" {
			errs = append(errs, errors.New("SMS_HTTP_TOKEN est obligatoire avec SMS_HTTP_AUTH="+s.HTTPAuth))
		}
	case "basic":
		if s.HTTPUser == "" || s.HTTPPassword == "" {
			errs = append(errs, errors.New("SMS_HTTP_USER et SMS_HTTP_PASSWORD sont obligatoires avec SMS_HTTP_AUTH=basic"))
		}
	default:
		errs = append(errs, fmt.Errorf("SMS_HTTP_AUTH : %q (none, bearer, basic, header)", s.HTTPAuth))
	}
	switch s.HTTPPhoneFormat {
	case "e164", "digits", "national":
	default:
		errs = append(errs, fmt.Errorf("SMS_HTTP_PHONE_FORMAT : %q (e164, digits, national)", s.HTTPPhoneFormat))
	}
	return errs
}

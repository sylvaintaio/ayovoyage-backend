// Package server assemble le routeur HTTP, l'API Huma (OpenAPI) et les middlewares.
package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/httprate"

	"busvoyage/internal/auth"
	"busvoyage/internal/config"
	"busvoyage/internal/platform/apix"
)

const Version = "1.0.0"

const apiDescription = `API de la plateforme de transport de voyageurs et de colis.

## Authentification
- **Clients** : code à usage unique par SMS (` + "`/v1/auth/otp/request`" + ` puis ` + "`/v1/auth/otp/verify`" + `).
- **Personnel** : e-mail + mot de passe (` + "`/v1/auth/staff/login`" + `).
- **Applications mobiles** (en-tête ` + "`X-Client-Type: mobile`" + `) : jetons dans le corps de la réponse, envoyés ensuite en ` + "`Authorization: Bearer <jeton>`" + `.
- **Site web Angular** (` + "`X-Client-Type: web`" + `) : jetons en cookies HttpOnly. Pour toute requête POST/PUT/DELETE,
  renvoyer la valeur du cookie ` + "`XSRF-TOKEN`" + ` dans l'en-tête ` + "`X-XSRF-TOKEN`" + ` (comportement natif du HttpClient Angular).
- Jeton d'accès : 15 minutes. Rotation via ` + "`/v1/auth/refresh`" + `.

## Erreurs
Format **RFC 9457** (` + "`application/problem+json`" + `) avec un message en français dans ` + "`detail`" + `.
Codes usuels : 401 (non connecté), 403 (droits), 404, 409 (conflit métier : siège déjà pris…), 422 (validation), 429 (trop de requêtes).

## Conventions
- Montants en **francs CFA (XOF), entiers**, sans décimales.
- Dates en **RFC 3339** (UTC dans les réponses) ; les dates seules (` + "`AAAA-MM-JJ`" + `) sont interprétées à l'heure du Bénin (UTC+1).
- Pagination : ` + "`limit`" + ` (max 100) et ` + "`offset`" + `.
`

type Registrar interface {
	Register(api huma.API)
}

// New construit le handler HTTP complet.
func New(cfg config.Config, issuer *auth.TokenIssuer, healthCheck func() error, modules ...Registrar) (http.Handler, huma.API) {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if cfg.TrustProxy {
		r.Use(middleware.RealIP) // uniquement derrière un reverse proxy de confiance (Caddy)
	}
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ip, _, err := net.SplitHostPort(req.RemoteAddr)
			if err != nil {
				ip = req.RemoteAddr
			}
			ctx := apix.WithRequestMeta(req.Context(), apix.RequestMeta{IP: ip, UserAgent: req.UserAgent()})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Client-Type", "X-Device-ID", apix.HeaderXSRF},
		ExposedHeaders:   []string{apix.HeaderMFA},
		AllowCredentials: true,
		MaxAge:           600,
	}))
	// Limitation de débit globale par IP ; l'authentification a ses propres limites (OTP, verrouillage).
	r.Use(httprate.LimitByIP(300, time.Minute))
	authLimiter := httprate.LimitByIP(30, time.Minute)
	r.Use(func(next http.Handler) http.Handler {
		limited := authLimiter(next)
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasPrefix(req.URL.Path, "/v1/auth/") {
				limited.ServeHTTP(w, req)
				return
			}
			next.ServeHTTP(w, req)
		})
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := healthCheck(); err != nil {
			http.Error(w, "base de données indisponible", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Les listes sont toujours renvoyées vides ([]) et jamais null.
	huma.DefaultArrayNullable = false
	hc := huma.DefaultConfig(cfg.BrandName+" API", Version)
	hc.Info.Description = apiDescription
	hc.Servers = []*huma.Server{{URL: cfg.PublicAPIURL}}
	hc.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearerAuth": {Type: "http", Scheme: "bearer", BearerFormat: "JWT", Description: "Applications mobiles"},
		"cookieAuth": {Type: "apiKey", In: "cookie", Name: apix.CookieAccess, Description: "Site web (cookie HttpOnly + CSRF)"},
	}
	hc.CreateHooks = nil // pas de champ $schema ajouté aux réponses
	if !cfg.EnableDocs {
		hc.DocsPath = ""
	}
	api := humachi.New(r, hc)
	api.UseMiddleware(apix.AuthMiddleware(api, issuer))

	for _, m := range modules {
		m.Register(api)
	}
	return r, api
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		if !strings.HasPrefix(r.URL.Path, "/docs") {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		slog.Info("http",
			"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(), "request_id", middleware.GetReqID(r.Context()))
	})
}

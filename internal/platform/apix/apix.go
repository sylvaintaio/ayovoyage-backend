// Package apix regroupe les briques communes aux handlers HTTP :
// déclaration des opérations, contrôle d'accès, erreurs, pagination, audit.
package apix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// Access définit qui peut appeler une opération.
type Access int

const (
	Public        Access = iota // sans authentification
	Customer                    // client connecté
	Staff                       // personnel (rôles éventuellement restreints)
	Authenticated               // tout utilisateur connecté (client ou personnel)
)

const (
	metaAccess   = "access"
	metaRoles    = "roles"
	metaFreshMFA = "fresh_mfa"

	// FreshMFAMaxAge : durée de validité d'une vérification du code pour les actions sensibles.
	FreshMFAMaxAge = 15 * time.Minute
	HeaderMFA      = "X-MFA-Required"

	CookieAccess  = "bv_access"
	CookieRefresh = "bv_refresh"
	CookieXSRF    = "XSRF-TOKEN"
	HeaderXSRF    = "X-XSRF-TOKEN"
)

var securityRequirement = []map[string][]string{{"bearerAuth": {}}, {"cookieAuth": {}}}

// Op construit une opération documentée avec ses règles d'accès.
func Op(id, method, path, tag, summary string, access Access, roles ...string) huma.Operation {
	op := huma.Operation{
		OperationID: id,
		Method:      method,
		Path:        path,
		Tags:        []string{tag},
		Summary:     summary,
		Metadata:    map[string]any{metaAccess: access, metaRoles: roles},
	}
	switch access {
	case Authenticated:
		op.Security = securityRequirement
		op.Description = "Réservé aux utilisateurs connectés (clients et personnel)."
		op.Errors = []int{http.StatusUnauthorized}
	case Customer:
		op.Security = securityRequirement
		op.Description = "Réservé aux **clients** connectés."
		op.Errors = []int{http.StatusUnauthorized, http.StatusForbidden}
	case Staff:
		op.Security = securityRequirement
		if len(roles) > 0 {
			op.Description = "Réservé au **personnel** — rôles : `" + strings.Join(roles, "`, `") + "`."
		} else {
			op.Description = "Réservé au **personnel** (tous rôles)."
		}
		op.Errors = []int{http.StatusUnauthorized, http.StatusForbidden}
	}
	return op
}

// RequireFreshMFA : action sensible. Le personnel doit avoir saisi son code de
// double authentification depuis moins de 15 minutes (sinon 403 + en-tête
// X-MFA-Required: step-up, puis POST /v1/auth/staff/mfa/confirm).
func RequireFreshMFA(op huma.Operation) huma.Operation {
	op.Metadata[metaFreshMFA] = true
	op.Description += "\n\n**Action sensible** : code de double authentification exigé (vérifié depuis moins de 15 minutes)."
	return op
}

// WithDescription ajoute un paragraphe à la description de l'opération.
func WithDescription(op huma.Operation, text string) huma.Operation {
	if op.Description != "" {
		op.Description = text + "\n\n" + op.Description
	} else {
		op.Description = text
	}
	return op
}

// AuthMiddleware authentifie la requête (Bearer ou cookie HttpOnly),
// vérifie le CSRF pour les cookies et applique les règles d'accès de l'opération.
func AuthMiddleware(api huma.API, issuer *auth.TokenIssuer) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		access, _ := ctx.Operation().Metadata[metaAccess].(Access)
		roles, _ := ctx.Operation().Metadata[metaRoles].([]string)

		token, fromCookie := "", false
		if h := ctx.Header("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		} else if c, err := huma.ReadCookie(ctx, CookieAccess); err == nil {
			token, fromCookie = c.Value, true
		}

		if token == "" {
			if access == Public {
				next(ctx)
				return
			}
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "Authentification requise")
			return
		}

		p, err := issuer.Parse(token)
		if err != nil {
			if access == Public {
				next(ctx)
				return
			}
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "Session invalide ou expirée")
			return
		}

		// Protection CSRF (double-submit cookie) pour les sessions par cookie.
		if fromCookie && !isSafeMethod(ctx.Method()) {
			c, err := huma.ReadCookie(ctx, CookieXSRF)
			if err != nil || c.Value == "" || c.Value != ctx.Header(HeaderXSRF) {
				_ = huma.WriteErr(api, ctx, http.StatusForbidden, "Jeton CSRF manquant ou invalide")
				return
			}
		}

		switch access {
		case Customer:
			if !p.IsCustomer() {
				_ = huma.WriteErr(api, ctx, http.StatusForbidden, "Opération réservée aux clients")
				return
			}
		case Staff:
			if !p.IsStaff() || (len(roles) > 0 && !p.HasRole(roles...)) {
				_ = huma.WriteErr(api, ctx, http.StatusForbidden, "Droits insuffisants pour cette opération")
				return
			}
		}
		if fresh, _ := ctx.Operation().Metadata[metaFreshMFA].(bool); fresh && p.IsStaff() && !auth.MFAExempt(p.Role) &&
			!p.FreshMFA(FreshMFAMaxAge, time.Now()) {
			ctx.SetHeader(HeaderMFA, "step-up")
			_ = huma.WriteErr(api, ctx, http.StatusForbidden, "Confirmez votre identité avec le code de votre application d'authentification")
			return
		}
		next(huma.WithContext(ctx, auth.WithPrincipal(ctx.Context(), p)))
	}
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// ---------------------------------------------------------------- Erreurs

// Err convertit une erreur interne en réponse HTTP (problem+json).
// Les erreurs huma sont conservées ; les erreurs inattendues sont journalisées
// et masquées au client.
func Err(err error) error {
	if err == nil {
		return nil
	}
	var se huma.StatusError
	if errors.As(err, &se) {
		return se
	}
	switch {
	case database.IsNotFound(err):
		return huma.Error404NotFound("Ressource introuvable")
	case database.UniqueViolation(err) != "":
		return huma.Error409Conflict("Cette ressource existe déjà (" + database.UniqueViolation(err) + ")")
	case database.IsForeignKeyViolation(err):
		return huma.Error422UnprocessableEntity("Une référence fournie est invalide")
	case database.IsCheckViolation(err):
		return huma.Error422UnprocessableEntity("Une valeur fournie est hors des limites autorisées")
	case errors.Is(err, context.Canceled):
		return huma.Error408RequestTimeout("Requête annulée")
	}
	slog.Error("erreur interne", "err", err)
	return huma.Error500InternalServerError("Erreur interne, veuillez réessayer")
}

// NotFound est un raccourci pour les ressources absentes ou invisibles (RLS).
func NotFound(what string) error {
	return huma.Error404NotFound(fmt.Sprintf("%s introuvable", what))
}

// ---------------------------------------------------------------- Pagination

type Page struct {
	Limit  int32 `query:"limit" minimum:"1" maximum:"100" default:"50" doc:"Nombre d'éléments (max 100)"`
	Offset int32 `query:"offset" minimum:"0" default:"0" doc:"Décalage pour la pagination"`
}

// ---------------------------------------------------------------- Requête

type requestMetaKey struct{}

// RequestMeta transporte les informations de la requête utiles à l'audit.
type RequestMeta struct {
	IP        string
	UserAgent string
}

func WithRequestMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, m)
}

func Meta(ctx context.Context) RequestMeta {
	m, _ := ctx.Value(requestMetaKey{}).(RequestMeta)
	return m
}

// ---------------------------------------------------------------- Audit

// Audit enregistre une action sensible dans le journal (ajout seul).
func Audit(ctx context.Context, q *store.Queries, companyID *uuid.UUID, action, entityType, entityID string, before, after any) error {
	m := Meta(ctx)
	params := store.InsertAuditLogParams{
		CompanyID:  companyID,
		ActorType:  "system",
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		Ip:         m.IP,
		UserAgent:  truncate(m.UserAgent, 300),
	}
	if p, ok := auth.FromContext(ctx); ok {
		params.ActorType = string(p.Type)
		id := p.ID
		params.ActorID = &id
	}
	var err error
	if before != nil {
		if params.Before, err = json.Marshal(before); err != nil {
			return err
		}
	}
	if after != nil {
		if params.After, err = json.Marshal(after); err != nil {
			return err
		}
	}
	return q.InsertAuditLog(ctx, params)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Ptr renvoie un pointeur vers v (utile pour les champs optionnels).
func Ptr[T any](v T) *T { return &v }

// NilIfEmpty convertit une chaîne vide en nil.
func NilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

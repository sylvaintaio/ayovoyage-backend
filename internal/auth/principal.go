package auth

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"busvoyage/internal/platform/database"
)

type SubjectType string

const (
	SubjectCustomer SubjectType = "customer"
	SubjectStaff    SubjectType = "staff"
)

// Rôles du personnel.
const (
	RoleCompanyAdmin  = "company_admin"
	RoleAgencyManager = "agency_manager"
	RoleCashier       = "cashier"
	RoleBoardingAgent = "boarding_agent"
	RoleDriver        = "driver"
	RoleParcelAgent   = "parcel_agent"
	RoleFinance       = "finance"
)

var AllRoles = []string{RoleCompanyAdmin, RoleAgencyManager, RoleCashier, RoleBoardingAgent, RoleDriver, RoleParcelAgent, RoleFinance}

// Principal représente l'utilisateur authentifié de la requête.
type Principal struct {
	Type      SubjectType
	ID        uuid.UUID
	CompanyID uuid.UUID  // personnel uniquement
	AgencyID  *uuid.UUID // personnel rattaché à une agence
	Role      string     // personnel uniquement
	MFAAt     *time.Time // dernière vérification du code de double authentification
}

// MFAExempt : rôles qui n'utilisent que l'application Agent, sur un terminal
// enregistré et révocable (le terminal tient lieu de second facteur).
func MFAExempt(role string) bool {
	return role == RoleBoardingAgent || role == RoleDriver || role == RoleParcelAgent
}

// FreshMFA : le code a-t-il été vérifié il y a moins de maxAge ?
func (p Principal) FreshMFA(maxAge time.Duration, now time.Time) bool {
	return p.MFAAt != nil && now.Sub(*p.MFAAt) <= maxAge
}

func (p Principal) IsStaff() bool    { return p.Type == SubjectStaff }
func (p Principal) IsCustomer() bool { return p.Type == SubjectCustomer }
func (p Principal) HasRole(roles ...string) bool {
	return p.IsStaff() && slices.Contains(roles, p.Role)
}

// Tenant renvoie le contexte RLS correspondant au principal.
func (p Principal) Tenant() database.Tenant {
	if p.IsStaff() {
		return database.Staff(p.CompanyID)
	}
	return database.Customer()
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// MustFromContext est utilisé dans les handlers protégés : le middleware
// garantit la présence du principal.
func MustFromContext(ctx context.Context) Principal {
	p, ok := FromContext(ctx)
	if !ok {
		panic("auth: principal absent d'une opération protégée")
	}
	return p
}

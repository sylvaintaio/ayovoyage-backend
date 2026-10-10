// Package admin : personnel, terminaux, journal d'audit et tableau de bord.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/departure"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

const (
	tagStaff     = "Admin — Personnel et terminaux"
	tagAudit     = "Admin — Journal d'audit"
	tagDashboard = "Admin — Tableau de bord"
)

type Handler struct {
	DB *database.DB
}

// ---------------------------------------------------------------- DTO

type Staff struct {
	ID          uuid.UUID  `json:"id"`
	AgencyID    *uuid.UUID `json:"agency_id,omitempty"`
	Email       string     `json:"email"`
	FullName    string     `json:"full_name"`
	Phone       string     `json:"phone"`
	Role        string     `json:"role"`
	IsActive    bool       `json:"is_active"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	MFAEnabled  bool       `json:"mfa_enabled" doc:"Double authentification activée"`
	MFARequired bool       `json:"mfa_required" doc:"Faux pour les rôles de l'application Agent"`
}

func toStaff(s store.StaffUser) Staff {
	return Staff{ID: s.ID, AgencyID: s.AgencyID, Email: s.Email, FullName: s.FullName, Phone: s.Phone, Role: s.Role,
		IsActive: s.IsActive, LastLoginAt: s.LastLoginAt, LockedUntil: s.LockedUntil, CreatedAt: s.CreatedAt,
		MFAEnabled: s.TotpEnabledAt != nil, MFARequired: !auth.MFAExempt(s.Role)}
}

type Device struct {
	ID         uuid.UUID  `json:"id"`
	StaffID    uuid.UUID  `json:"staff_id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	IsActive   bool       `json:"is_active"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func toDevice(d store.Device) Device {
	return Device{ID: d.ID, StaffID: d.StaffID, Name: d.Name, Platform: d.Platform, IsActive: d.IsActive,
		LastSeenAt: d.LastSeenAt, CreatedAt: d.CreatedAt}
}

type AuditEntry struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorID    *uuid.UUID      `json:"actor_id,omitempty"`
	Action     string          `json:"action"`
	EntityType string          `json:"entity_type"`
	EntityID   string          `json:"entity_id"`
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	IP         string          `json:"ip"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Dashboard struct {
	From           time.Time       `json:"from"`
	To             time.Time       `json:"to"`
	Revenue        int64           `json:"revenue" doc:"Encaissements (XOF)"`
	TicketsSold    int32           `json:"tickets_sold"`
	Cancellations  int32           `json:"cancellations"`
	RefundsAmount  int64           `json:"refunds_amount"`
	PendingRefunds int32           `json:"pending_refunds"`
	AverageFill    float64         `json:"average_fill_rate" doc:"Taux de remplissage moyen (0 à 1)"`
	ByAgency       []AgencyRevenue `json:"by_agency"`
	ByCategory     []CategoryUsage `json:"by_category" doc:"Utilisation des sièges VIP / Premium / Standard"`
	Departures     []DepartureFill `json:"departures"`
}

type AgencyRevenue struct {
	AgencyID   uuid.UUID `json:"agency_id"`
	AgencyName string    `json:"agency_name"`
	Bookings   int32     `json:"bookings"`
	Revenue    int64     `json:"revenue"`
}

type CategoryUsage struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	SeatsSold int32  `json:"seats_sold"`
	Revenue   int64  `json:"revenue"`
}

type DepartureFill struct {
	DepartureID uuid.UUID `json:"departure_id"`
	DepartureAt time.Time `json:"departure_at"`
	RouteName   string    `json:"route_name"`
	SeatCount   int32     `json:"seat_count"`
	SoldSeats   int32     `json:"sold_seats"`
	FillRate    float64   `json:"fill_rate"`
}

type StaffInput struct {
	AgencyID *uuid.UUID `json:"agency_id,omitempty" doc:"Obligatoire pour chef d'agence, caissier, agent"`
	FullName string     `json:"full_name" minLength:"2" maxLength:"120"`
	Phone    string     `json:"phone,omitempty" maxLength:"20"`
	Role     string     `json:"role" enum:"company_admin,agency_manager,cashier,boarding_agent,driver,parcel_agent,finance"`
}

// ---------------------------------------------------------------- Routes

func (h *Handler) Register(api huma.API) {
	admin := []string{auth.RoleCompanyAdmin}

	needsAgency := map[string]bool{auth.RoleAgencyManager: true, auth.RoleCashier: true, auth.RoleParcelAgent: true}
	checkAgency := func(ctx context.Context, q *store.Queries, companyID uuid.UUID, role string, agencyID *uuid.UUID) error {
		if needsAgency[role] && agencyID == nil {
			return huma.Error422UnprocessableEntity("Ce rôle doit être rattaché à une agence")
		}
		if agencyID != nil {
			if _, err := q.GetAgency(ctx, store.GetAgencyParams{ID: *agencyID, CompanyID: companyID}); err != nil {
				return huma.Error422UnprocessableEntity("Agence inconnue")
			}
		}
		return nil
	}

	huma.Register(api, apix.Op("list-staff", http.MethodGet, "/v1/admin/staff", tagStaff, "Lister le personnel", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Staff }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Staff }{Body: []Staff{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListStaff(ctx, p.CompanyID)
				for _, s := range rows {
					// Un chef d'agence ne voit que son équipe.
					if p.Role == auth.RoleAgencyManager && (s.AgencyID == nil || p.AgencyID == nil || *s.AgencyID != *p.AgencyID) {
						continue
					}
					out.Body = append(out.Body, toStaff(s))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("create-staff", http.MethodPost, "/v1/admin/staff", tagStaff,
		"Créer un compte du personnel", apix.Staff, admin...)),
		"Mot de passe : 12 caractères minimum. Stocké avec Argon2id."),
		func(ctx context.Context, in *struct {
			Body struct {
				StaffInput
				Email    string `json:"email" format:"email" maxLength:"254"`
				Password string `json:"password" minLength:"12" maxLength:"128"`
			}
		}) (*struct {
			Status int
			Body   Staff
		}, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			hash, err := auth.HashPassword(b.Password)
			if err != nil {
				return nil, apix.Err(err)
			}
			out := &struct {
				Status int
				Body   Staff
			}{Status: http.StatusCreated}
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if err := checkAgency(ctx, q, p.CompanyID, b.Role, b.AgencyID); err != nil {
					return err
				}
				s, err := q.CreateStaff(ctx, store.CreateStaffParams{CompanyID: p.CompanyID, AgencyID: b.AgencyID, Email: b.Email,
					PasswordHash: hash, FullName: b.FullName, Phone: b.Phone, Role: b.Role})
				if database.UniqueViolation(err) != "" {
					return huma.Error409Conflict("Un compte existe déjà avec cet e-mail")
				}
				if err != nil {
					return err
				}
				out.Body = toStaff(s)
				return apix.Audit(ctx, q, &p.CompanyID, "staff.created", "staff", s.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("update-staff", http.MethodPut, "/v1/admin/staff/{id}", tagStaff,
		"Modifier un compte (rôle, agence, activation)", apix.Staff, admin...)),
		"La désactivation révoque immédiatement toutes les sessions de la personne."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				StaffInput
				IsActive bool `json:"is_active"`
			}
		}) (*struct{ Body Staff }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			if in.ID == p.ID && (!b.IsActive || b.Role != auth.RoleCompanyAdmin) {
				return nil, huma.Error422UnprocessableEntity("Vous ne pouvez pas retirer vos propres droits d'administration")
			}
			out := &struct{ Body Staff }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetStaff(ctx, in.ID)
				if err != nil {
					return apix.NotFound("Compte")
				}
				if err := checkAgency(ctx, q, p.CompanyID, b.Role, b.AgencyID); err != nil {
					return err
				}
				s, err := q.UpdateStaff(ctx, store.UpdateStaffParams{ID: in.ID, CompanyID: p.CompanyID, AgencyID: b.AgencyID,
					FullName: b.FullName, Phone: b.Phone, Role: b.Role, IsActive: b.IsActive})
				if err != nil {
					return err
				}
				if !s.IsActive || s.Role != before.Role {
					if err := q.RevokeSubjectRefreshTokens(ctx, store.RevokeSubjectRefreshTokensParams{SubjectType: "staff", SubjectID: s.ID}); err != nil {
						return err
					}
				}
				out.Body = toStaff(s)
				return apix.Audit(ctx, q, &p.CompanyID, "staff.updated", "staff", s.ID.String(), toStaff(before), out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.RequireFreshMFA(apix.Op("reset-staff-password", http.MethodPost, "/v1/admin/staff/{id}/password", tagStaff,
		"Réinitialiser le mot de passe d'un compte", apix.Staff, admin...)),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				Password string `json:"password" minLength:"12" maxLength:"128"`
			}
		}) (*struct{}, error) {
			p := auth.MustFromContext(ctx)
			hash, err := auth.HashPassword(in.Body.Password)
			if err != nil {
				return nil, apix.Err(err)
			}
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if _, err := q.GetStaff(ctx, in.ID); err != nil {
					return apix.NotFound("Compte")
				}
				if err := q.SetStaffPassword(ctx, store.SetStaffPasswordParams{ID: in.ID, PasswordHash: hash}); err != nil {
					return err
				}
				if err := q.RevokeSubjectRefreshTokens(ctx, store.RevokeSubjectRefreshTokensParams{SubjectType: "staff", SubjectID: in.ID}); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "staff.password_reset", "staff", in.ID.String(), nil, nil)
			})
			return nil, apix.Err(err)
		})

	// --- Terminaux
	managers := []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager}
	huma.Register(api, apix.Op("list-devices", http.MethodGet, "/v1/admin/devices", tagStaff, "Lister les terminaux des agents", apix.Staff, managers...),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Device }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Device }{Body: []Device{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListDevices(ctx, p.CompanyID)
				for _, d := range rows {
					out.Body = append(out.Body, toDevice(d))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("register-device", http.MethodPost, "/v1/admin/devices", tagStaff,
		"Enregistrer le terminal d'un agent", apix.Staff, managers...)),
		"L'identifiant renvoyé est saisi dans l'application Agent et envoyé dans l'en-tête `X-Device-ID`."),
		func(ctx context.Context, in *struct {
			Body struct {
				StaffID  uuid.UUID `json:"staff_id"`
				Name     string    `json:"name" minLength:"2" maxLength:"80" example:"Samsung A15 — Agence Jonquet"`
				Platform string    `json:"platform,omitempty" enum:"android,ios" default:"android"`
			}
		}) (*struct {
			Status int
			Body   Device
		}, error) {
			p := auth.MustFromContext(ctx)
			out := &struct {
				Status int
				Body   Device
			}{Status: http.StatusCreated}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				if _, err := q.GetStaff(ctx, in.Body.StaffID); err != nil {
					return huma.Error422UnprocessableEntity("Agent inconnu")
				}
				d, err := q.CreateDevice(ctx, store.CreateDeviceParams{CompanyID: p.CompanyID, StaffID: in.Body.StaffID,
					Name: in.Body.Name, Platform: in.Body.Platform})
				if err != nil {
					return err
				}
				out.Body = toDevice(d)
				return apix.Audit(ctx, q, &p.CompanyID, "device.registered", "device", d.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.RequireFreshMFA(apix.Op("set-device-active", http.MethodPut, "/v1/admin/devices/{id}", tagStaff,
		"Activer / désactiver un terminal (perte, vol)", apix.Staff, managers...)),
		"Un terminal désactivé ne peut plus scanner ni synchroniser."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body struct {
				IsActive bool `json:"is_active"`
			}
		}) (*struct{ Body Device }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body Device }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				d, err := q.SetDeviceActive(ctx, store.SetDeviceActiveParams{ID: in.ID, CompanyID: p.CompanyID, IsActive: in.Body.IsActive})
				if err != nil {
					return err
				}
				out.Body = toDevice(d)
				return apix.Audit(ctx, q, &p.CompanyID, "device.status_changed", "device", d.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	// --- Journal d'audit
	huma.Register(api, apix.Op("list-audit-logs", http.MethodGet, "/v1/admin/audit-logs", tagAudit, "Consulter le journal d'audit", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleFinance),
		func(ctx context.Context, in *struct {
			apix.Page
			EntityType string    `query:"entity_type" maxLength:"40"`
			EntityID   string    `query:"entity_id" maxLength:"64"`
			ActorID    uuid.UUID `query:"actor_id"`
		}) (*struct{ Body []AuditEntry }, error) {
			p := auth.MustFromContext(ctx)
			params := store.ListAuditLogsParams{CompanyID: &p.CompanyID, EntityType: apix.NilIfEmpty(in.EntityType),
				EntityID: apix.NilIfEmpty(in.EntityID), Lim: in.Limit, Off: in.Offset}
			if in.ActorID != uuid.Nil {
				params.ActorID = &in.ActorID
			}
			out := &struct{ Body []AuditEntry }{Body: []AuditEntry{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListAuditLogs(ctx, params)
				for _, r := range rows {
					out.Body = append(out.Body, AuditEntry{ID: r.ID, ActorType: r.ActorType, ActorID: r.ActorID, Action: r.Action,
						EntityType: r.EntityType, EntityID: r.EntityID, Before: r.Before, After: r.After, IP: r.Ip, CreatedAt: r.CreatedAt})
				}
				return err
			})
			return out, apix.Err(err)
		})

	// --- Tableau de bord
	huma.Register(api, apix.Op("get-dashboard", http.MethodGet, "/v1/admin/dashboard", tagDashboard,
		"Indicateurs : chiffre d'affaires, billets, remplissage, agences, catégories", apix.Staff,
		auth.RoleCompanyAdmin, auth.RoleAgencyManager, auth.RoleFinance),
		func(ctx context.Context, in *struct {
			From string `query:"from" format:"date" doc:"Par défaut : il y a 30 jours"`
			To   string `query:"to" format:"date" doc:"Inclus. Par défaut : aujourd'hui"`
		}) (*struct{ Body Dashboard }, error) {
			p := auth.MustFromContext(ctx)
			now := time.Now().In(departure.LocalTZ)
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, departure.LocalTZ)
			from, to := today.AddDate(0, 0, -30), today
			var err error
			if in.From != "" {
				if from, err = time.ParseInLocation(time.DateOnly, in.From, departure.LocalTZ); err != nil {
					return nil, huma.Error422UnprocessableEntity("Date de début invalide")
				}
			}
			if in.To != "" {
				if to, err = time.ParseInLocation(time.DateOnly, in.To, departure.LocalTZ); err != nil {
					return nil, huma.Error422UnprocessableEntity("Date de fin invalide")
				}
			}
			to = to.AddDate(0, 0, 1)
			if to.Before(from) || to.Sub(from) > 366*24*time.Hour {
				return nil, huma.Error422UnprocessableEntity("Période invalide (1 an maximum)")
			}
			out := &struct{ Body Dashboard }{Body: Dashboard{From: from, To: to,
				ByAgency: []AgencyRevenue{}, ByCategory: []CategoryUsage{}, Departures: []DepartureFill{}}}
			d := &out.Body
			err = h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				t, err := q.DashboardTotals(ctx, store.DashboardTotalsParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to})
				if err != nil {
					return err
				}
				d.Revenue, d.TicketsSold, d.Cancellations, d.RefundsAmount, d.PendingRefunds =
					t.Revenue, t.TicketsSold, t.Cancellations, t.RefundsAmount, t.PendingRefunds
				agencies, err := q.DashboardRevenueByAgency(ctx, store.DashboardRevenueByAgencyParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to})
				if err != nil {
					return err
				}
				for _, a := range agencies {
					d.ByAgency = append(d.ByAgency, AgencyRevenue(a))
				}
				cats, err := q.DashboardCategoryUsage(ctx, store.DashboardCategoryUsageParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to})
				if err != nil {
					return err
				}
				for _, c := range cats {
					d.ByCategory = append(d.ByCategory, CategoryUsage(c))
				}
				fills, err := q.DashboardFillRates(ctx, store.DashboardFillRatesParams{CompanyID: p.CompanyID, FromTs: from, ToTs: to})
				if err != nil {
					return err
				}
				var sum float64
				for _, f := range fills {
					rate := 0.0
					if f.SeatCount > 0 {
						rate = float64(f.SoldSeats) / float64(f.SeatCount)
					}
					sum += rate
					d.Departures = append(d.Departures, DepartureFill{f.DepartureID, f.DepartureAt, f.RouteName, f.SeatCount, f.SoldSeats, rate})
				}
				if len(fills) > 0 {
					d.AverageFill = sum / float64(len(fills))
				}
				return nil
			})
			return out, apix.Err(err)
		})
}

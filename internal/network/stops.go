package network

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"busvoyage/internal/auth"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// StopPoint : point d'arrêt du catalogue de la compagnie (agence ou simple lieu).
type StopPoint struct {
	ID         uuid.UUID  `json:"id"`
	CityID     uuid.UUID  `json:"city_id"`
	CityName   string     `json:"city_name"`
	AgencyID   *uuid.UUID `json:"agency_id,omitempty" doc:"Point d'arrêt situé à une agence : montée possible sur réservation"`
	AgencyName string     `json:"agency_name,omitempty"`
	Name       string     `json:"name"`
	Address    string     `json:"address"`
	Latitude   *float64   `json:"latitude,omitempty"`
	Longitude  *float64   `json:"longitude,omitempty"`
	IsActive   bool       `json:"is_active"`
	InUse      bool       `json:"in_use" doc:"Utilisé par au moins une ligne"`
}

type StopPointInput struct {
	CityID    uuid.UUID `json:"city_id" doc:"Ignoré en modification"`
	Name      string    `json:"name" minLength:"2" maxLength:"80" example:"Carrefour Dassa"`
	Address   string    `json:"address" maxLength:"200" example:"Carrefour de la RNIE 2, devant la station"`
	Latitude  *float64  `json:"latitude,omitempty" minimum:"-90" maximum:"90"`
	Longitude *float64  `json:"longitude,omitempty" minimum:"-180" maximum:"180"`
	IsActive  bool      `json:"is_active"`
}

func toStopPoint(sp store.StopPoint, city, agency string) StopPoint {
	return StopPoint{ID: sp.ID, CityID: sp.CityID, CityName: city, AgencyID: sp.AgencyID, AgencyName: agency, Name: sp.Name,
		Address: sp.Address, Latitude: sp.Latitude, Longitude: sp.Longitude, IsActive: sp.IsActive}
}

// EnsureAgencyStopPoint crée le point d'arrêt d'une agence (une agence est toujours un point d'arrêt
// où l'on peut monter). Homonyme dans la ville : suffixe « (agence) ».
func EnsureAgencyStopPoint(ctx context.Context, q *store.Queries, a store.Agency) error {
	if _, err := q.GetStopPointByAgency(ctx, &a.ID); err == nil || !database.IsNotFound(err) {
		return err
	}
	params := store.CreateStopPointParams{CompanyID: a.CompanyID, CityID: a.CityID, AgencyID: &a.ID, Name: a.Name,
		Address: a.Address, Latitude: a.Latitude, Longitude: a.Longitude, IsActive: a.IsActive}
	if _, err := q.CreateStopPoint(ctx, params); database.UniqueViolation(err) != "" {
		params.Name = a.Name + " (agence)"
		_, err = q.CreateStopPoint(ctx, params)
		return err
	} else {
		return err
	}
}

func (h *Handler) registerStopPoints(api huma.API, admin []string) {
	huma.Register(api, apix.Op("list-stop-points", http.MethodGet, "/v1/admin/stop-points", tagAdmin,
		"Lister les points d'arrêt", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []StopPoint }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []StopPoint }{Body: []StopPoint{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListStopPoints(ctx, p.CompanyID)
				if err != nil {
					return err
				}
				for _, r := range rows {
					sp := toStopPoint(store.StopPoint{ID: r.ID, CompanyID: r.CompanyID, CityID: r.CityID, AgencyID: r.AgencyID, Name: r.Name,
						Address: r.Address, Latitude: r.Latitude, Longitude: r.Longitude, IsActive: r.IsActive}, r.CityName, deref(r.AgencyName))
					if sp.InUse, err = q.IsStopPointUsed(ctx, r.ID); err != nil {
						return err
					}
					out.Body = append(out.Body, sp)
				}
				return nil
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-stop-point", http.MethodPost, "/v1/admin/stop-points", tagAdmin,
		"Créer un point d'arrêt (carrefour, station, gare routière…)", apix.Staff, admin...),
		"Les agences ont déjà leur point d'arrêt, créé automatiquement."),
		func(ctx context.Context, in *struct{ Body StopPointInput }) (*struct{ Body StopPoint }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body StopPoint }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				city, err := q.GetCity(ctx, b.CityID)
				if database.IsNotFound(err) {
					return huma.Error422UnprocessableEntity("Ville inconnue")
				}
				if err != nil {
					return err
				}
				sp, err := q.CreateStopPoint(ctx, store.CreateStopPointParams{CompanyID: p.CompanyID, CityID: b.CityID, Name: b.Name,
					Address: b.Address, Latitude: b.Latitude, Longitude: b.Longitude, IsActive: b.IsActive})
				if database.UniqueViolation(err) != "" {
					return huma.Error409Conflict("Un point d'arrêt porte déjà ce nom dans cette ville")
				}
				if err != nil {
					return err
				}
				out.Body = toStopPoint(sp, city.Name, "")
				return apix.Audit(ctx, q, &p.CompanyID, "stop_point.created", "stop_point", sp.ID.String(), nil, out.Body)
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("update-stop-point", http.MethodPut, "/v1/admin/stop-points/{id}", tagAdmin,
		"Modifier un point d'arrêt", apix.Staff, admin...),
		"La ville et l'agence liée ne changent pas (les billets déjà vendus y font référence). "+
			"Un point d'arrêt désactivé n'est plus proposé pour les nouvelles lignes."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body StopPointInput
		}) (*struct{ Body StopPoint }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			out := &struct{ Body StopPoint }{}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				before, err := q.GetStopPoint(ctx, store.GetStopPointParams{ID: in.ID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return apix.NotFound("Point d'arrêt")
				}
				if err != nil {
					return err
				}
				sp, err := q.UpdateStopPoint(ctx, store.UpdateStopPointParams{ID: in.ID, CompanyID: p.CompanyID, Name: b.Name,
					Address: b.Address, Latitude: b.Latitude, Longitude: b.Longitude, IsActive: b.IsActive})
				if database.UniqueViolation(err) != "" {
					return huma.Error409Conflict("Un point d'arrêt porte déjà ce nom dans cette ville")
				}
				if err != nil {
					return err
				}
				city, err := q.GetCity(ctx, sp.CityID)
				if err != nil {
					return err
				}
				out.Body = toStopPoint(sp, city.Name, "")
				prev := toStopPoint(before, city.Name, "")
				return apix.Audit(ctx, q, &p.CompanyID, "stop_point.updated", "stop_point", sp.ID.String(), prev, out.Body)
			})
			return out, apix.Err(err)
		})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

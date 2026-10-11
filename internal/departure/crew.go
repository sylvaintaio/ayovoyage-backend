package departure

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// CrewMemberInput : membre d'équipage en plus du contrôleur (agent_id) et du chauffeur (driver_id).
type CrewMemberInput struct {
	StaffID uuid.UUID `json:"staff_id"`
	Role    string    `json:"role" enum:"driver,assistant" doc:"driver : second chauffeur ; assistant : aide, convoyeur…"`
}

// CrewMember : membre de l'équipage d'un départ.
type CrewMember struct {
	StaffID  uuid.UUID `json:"staff_id"`
	Role     string    `json:"role" enum:"controller,driver,assistant"`
	FullName string    `json:"full_name"`
	Phone    string    `json:"phone,omitempty"`
}

// DepartureStop : arrêt d'un départ, avec son heure prévue (copie figée de la ligne).
type DepartureStop struct {
	Seq          int        `json:"seq"`
	StopPointID  uuid.UUID  `json:"stop_point_id"`
	StopName     string     `json:"stop_name"`
	CityID       uuid.UUID  `json:"city_id"`
	CityName     string     `json:"city_name"`
	AgencyID     *uuid.UUID `json:"agency_id,omitempty"`
	Address      string     `json:"address,omitempty"`
	Latitude     *float64   `json:"latitude,omitempty"`
	Longitude    *float64   `json:"longitude,omitempty"`
	ScheduledAt  time.Time  `json:"scheduled_at"`
	PauseMinutes int        `json:"pause_minutes"`
	CanBoard     bool       `json:"can_board"`
	CanAlight    bool       `json:"can_alight"`
}

// LoadStops : arrêts d'un départ, dans l'ordre de passage.
func LoadStops(ctx context.Context, q *store.Queries, departureID uuid.UUID) ([]DepartureStop, error) {
	rows, err := q.ListDepartureStops(ctx, departureID)
	if err != nil {
		return nil, err
	}
	out := make([]DepartureStop, 0, len(rows))
	for _, r := range rows {
		out = append(out, DepartureStop{Seq: int(r.Seq), StopPointID: r.StopPointID, StopName: r.StopName, CityID: r.CityID,
			CityName: r.CityName, AgencyID: r.AgencyID, Address: r.Address, Latitude: r.Latitude, Longitude: r.Longitude,
			ScheduledAt: r.ScheduledAt, PauseMinutes: int(r.PauseMinutes), CanBoard: r.CanBoard, CanAlight: r.CanAlight})
	}
	return out, nil
}

// LoadCrew : contrôleur, chauffeur et autres membres d'un départ.
func LoadCrew(ctx context.Context, q *store.Queries, d store.Departure) ([]CrewMember, error) {
	out := []CrewMember{}
	for _, m := range []struct {
		id   *uuid.UUID
		role string
	}{{d.AgentID, "controller"}, {d.DriverID, "driver"}} {
		if m.id == nil {
			continue
		}
		st, err := q.GetStaff(ctx, *m.id)
		if err != nil && !database.IsNotFound(err) {
			return nil, err
		}
		out = append(out, CrewMember{StaffID: *m.id, Role: m.role, FullName: st.FullName, Phone: st.Phone})
	}
	rows, err := q.ListDepartureCrew(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, CrewMember{StaffID: r.StaffID, Role: r.Role, FullName: r.FullName, Phone: r.Phone})
	}
	return out, nil
}

// crewConflict : premier membre déjà affecté à un départ qui chevauche [from, to), ou nil.
// Contrôle aussi les doublons (une même personne deux fois dans l'équipage).
func crewConflict(ctx context.Context, q *store.Queries, companyID uuid.UUID, exclude *uuid.UUID, from, to time.Time,
	agent, driver *uuid.UUID, extra []CrewMemberInput) error {
	ids := []*uuid.UUID{agent, driver}
	for i := range extra {
		ids = append(ids, &extra[i].StaffID)
	}
	if err := ensureStaff(ctx, q, companyID, ids...); err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if id == nil {
			continue
		}
		if seen[*id] {
			return huma.Error422UnprocessableEntity("Une même personne figure deux fois dans l'équipage")
		}
		seen[*id] = true
		busy, err := q.StaffBusyOnDeparture(ctx, store.StaffBusyOnDepartureParams{CompanyID: companyID, ExcludeID: exclude,
			FromTs: from, ToTs: to, StaffID: id})
		if database.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		st, _ := q.GetStaff(ctx, *id)
		return huma.Error409Conflict(st.FullName + " est déjà affecté au départ " + busy.RouteName + " du " +
			busy.DepartureAt.In(LocalTZ).Format("02/01 à 15h04"))
	}
	return nil
}

// saveCrew remplace les autres membres de l'équipage (contrôleur et chauffeur sont sur le départ).
func saveCrew(ctx context.Context, q *store.Queries, companyID, departureID uuid.UUID, extra []CrewMemberInput) error {
	if err := q.DeleteDepartureCrew(ctx, departureID); err != nil {
		return err
	}
	for _, m := range extra {
		if err := q.InsertDepartureCrew(ctx, store.InsertDepartureCrewParams{DepartureID: departureID, CompanyID: companyID,
			StaffID: m.StaffID, Role: m.Role}); err != nil {
			return err
		}
	}
	return nil
}

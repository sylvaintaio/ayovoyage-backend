package departure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"busvoyage/internal/auth"
	"busvoyage/internal/jobs"
	"busvoyage/internal/platform/apix"
	"busvoyage/internal/platform/database"
	"busvoyage/internal/store"
)

// ScheduleHorizonDays : les départs d'un horaire sont créés jusqu'à ce nombre de jours à l'avance,
// au fil des jours (le serveur complète chaque heure).
const ScheduleHorizonDays = 60

// maxScheduleIssues : dates non créées gardées pour l'affichage.
const maxScheduleIssues = 30

// Schedule : horaire permanent d'une ligne (« tous les jours à 07:00 avec tel bus »).
type Schedule struct {
	ID              uuid.UUID         `json:"id"`
	RouteID         uuid.UUID         `json:"route_id"`
	RouteName       string            `json:"route_name"`
	BusID           uuid.UUID         `json:"bus_id"`
	PlateNumber     string            `json:"plate_number"`
	Time            string            `json:"time" doc:"Heure de départ (HH:MM, heure du Bénin)"`
	Weekdays        []int             `json:"weekdays" doc:"1 = lundi … 7 = dimanche ; vide = tous les jours"`
	BasePrice       int64             `json:"base_price" doc:"0 : tarif de la ligne"`
	DriverID        *uuid.UUID        `json:"driver_id,omitempty"`
	AgentID         *uuid.UUID        `json:"agent_id,omitempty"`
	Crew            []CrewMemberInput `json:"crew"`
	Notes           string            `json:"notes"`
	StartsOn        string            `json:"starts_on" format:"date"`
	EndsOn          *string           `json:"ends_on,omitempty" format:"date" doc:"Absent : sans fin"`
	Active          bool              `json:"active" doc:"Faux : suspendu (plus aucun départ créé)"`
	Issues          []ScheduleIssue   `json:"issues" doc:"Dates des semaines à venir non créées, et pourquoi"`
	UpcomingCount   int32             `json:"upcoming_count" doc:"Départs à venir déjà créés"`
	NextDepartureAt *time.Time        `json:"next_departure_at,omitempty"`
}

type ScheduleIssue struct {
	Date   string `json:"date" format:"date"`
	Reason string `json:"reason"`
}

// KeptDeparture : départ à venir laissé tel quel lors d'un changement de l'horaire.
type KeptDeparture struct {
	ID          uuid.UUID `json:"id"`
	DepartureAt time.Time `json:"departure_at"`
	Reason      string    `json:"reason"`
}

// ScheduleChange : effet d'une création, d'une modification ou d'une suppression d'horaire.
type ScheduleChange struct {
	DryRun   bool            `json:"dry_run"`
	Schedule *Schedule       `json:"schedule,omitempty"`
	Created  int             `json:"created" doc:"Départs créés"`
	Removed  int             `json:"removed" doc:"Départs à venir retirés (sans réservation)"`
	Kept     []KeptDeparture `json:"kept" doc:"Départs à venir gardés tels quels (réservés ou modifiés à la main)"`
	Upcoming []time.Time     `json:"upcoming" doc:"Prochains départs de l'horaire (10 au plus)"`
	Issues   []ScheduleIssue `json:"issues"`
}

type ScheduleInput struct {
	BusID     uuid.UUID         `json:"bus_id,omitempty" doc:"Obligatoire, sauf à partir d'un départ (son bus)"`
	Time      string            `json:"time,omitempty" pattern:"^(([01][0-9]|2[0-3]):[0-5][0-9])?$" example:"07:00" doc:"HH:MM, heure du Bénin"`
	Weekdays  []int             `json:"weekdays,omitempty" maxItems:"7" doc:"1 = lundi … 7 = dimanche ; vide = tous les jours"`
	BasePrice int64             `json:"base_price,omitempty" minimum:"0" doc:"0 ou absent : tarif de la ligne"`
	DriverID  *uuid.UUID        `json:"driver_id,omitempty"`
	AgentID   *uuid.UUID        `json:"agent_id,omitempty" doc:"Contrôleur à bord"`
	Crew      []CrewMemberInput `json:"crew,omitempty" maxItems:"6"`
	Notes     string            `json:"notes,omitempty" maxLength:"1000"`
	StartsOn  string            `json:"starts_on,omitempty" format:"date" doc:"Par défaut : aujourd'hui"`
	EndsOn    string            `json:"ends_on,omitempty" format:"date" doc:"Absent : sans fin"`
	Active    *bool             `json:"active,omitempty" doc:"Faux : suspendre l'horaire (par défaut : actif)"`
	DryRun    bool              `json:"dry_run,omitempty" doc:"Aperçu sans rien enregistrer"`
}

// errDryRun annule la transaction d'un aperçu.
var errDryRun = errors.New("aperçu")

func isoWeekday(t time.Time) int { return (int(t.Weekday())+6)%7 + 1 }

// dateOnly : jour civil (sans fuseau) d'une date PostgreSQL.
func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func todayBenin(now time.Time) time.Time { return dateOnly(now.In(beninTZ)) }

func atBenin(day time.Time, hhmm string) time.Time {
	var h, m int
	_, _ = fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, beninTZ)
}

func toSchedule(s store.DepartureSchedule, routeName, plate string, upcoming int32, next time.Time) Schedule {
	out := Schedule{ID: s.ID, RouteID: s.RouteID, RouteName: routeName, BusID: s.BusID, PlateNumber: plate, Time: s.DepartureTime,
		Weekdays: []int{}, BasePrice: s.BasePrice, DriverID: s.DriverID, AgentID: s.AgentID, Crew: []CrewMemberInput{},
		Notes: s.Notes, StartsOn: s.StartsOn.Format(time.DateOnly), Active: s.Active, Issues: []ScheduleIssue{}, UpcomingCount: upcoming}
	for _, d := range s.Weekdays {
		out.Weekdays = append(out.Weekdays, int(d))
	}
	if s.EndsOn != nil {
		e := s.EndsOn.Format(time.DateOnly)
		out.EndsOn = &e
	}
	_ = json.Unmarshal(s.Crew, &out.Crew)
	_ = json.Unmarshal(s.Issues, &out.Issues)
	if next.Year() > 1970 {
		out.NextDepartureAt = &next
	}
	return out
}

// generate crée les départs manquants d'un horaire, d'aujourd'hui à l'horizon. Une date déjà servie
// (même annulée à la main : jour sauté) n'est jamais recréée. Les dates impossibles (bus ou équipage
// déjà pris, bus indisponible) sont notées dans l'horaire et retentées à la génération suivante.
func generate(ctx context.Context, q *store.Queries, s store.DepartureSchedule, now time.Time) (int, []ScheduleIssue, error) {
	issues := []ScheduleIssue{}
	created := 0
	today := todayBenin(now)
	until := today.AddDate(0, 0, ScheduleHorizonDays)
	if s.EndsOn != nil && dateOnly(*s.EndsOn).Before(until) {
		until = dateOnly(*s.EndsOn)
	}
	start := dateOnly(s.StartsOn)
	if start.Before(today) {
		start = today
	}
	save := func() (int, []ScheduleIssue, error) {
		raw, _ := json.Marshal(issues)
		return created, issues, q.SetScheduleGenerated(ctx, store.SetScheduleGeneratedParams{ID: s.ID, Issues: raw, GeneratedUntil: &until})
	}
	if !s.Active || start.After(until) {
		return save()
	}
	route, err := q.GetRoute(ctx, store.GetRouteParams{ID: s.RouteID, CompanyID: s.CompanyID})
	if err != nil {
		return 0, nil, err
	}
	bus, err := q.GetBus(ctx, store.GetBusParams{ID: s.BusID, CompanyID: s.CompanyID})
	if err != nil {
		return 0, nil, err
	}
	blocker := ""
	switch {
	case !route.IsActive:
		blocker = "Ligne désactivée"
	case bus.Status != "active":
		blocker = "Bus " + bus.PlateNumber + " indisponible (" + bus.Status + ")"
	}
	served, err := q.ListScheduleServiceDates(ctx, store.ListScheduleServiceDatesParams{ScheduleID: &s.ID, FromDate: start})
	if err != nil {
		return 0, nil, err
	}
	done := map[time.Time]bool{}
	for _, d := range served {
		done[dateOnly(d)] = true
	}
	var crew []CrewMemberInput
	_ = json.Unmarshal(s.Crew, &crew)
	price := s.BasePrice
	if price == 0 {
		price = route.BasePrice
	}
	note := func(day time.Time, reason string) {
		if len(issues) < maxScheduleIssues {
			issues = append(issues, ScheduleIssue{Date: day.Format(time.DateOnly), Reason: reason})
		}
	}
	for day := start; !day.After(until); day = day.AddDate(0, 0, 1) {
		if done[day] || (len(s.Weekdays) > 0 && !slices.Contains(s.Weekdays, int16(isoWeekday(day)))) {
			continue
		}
		at := atBenin(day, s.DepartureTime)
		if !at.After(now) {
			continue
		}
		if blocker != "" {
			note(day, blocker)
			continue
		}
		arrival := at.Add(time.Duration(route.DurationMinutes) * time.Minute)
		overlap, err := q.BusHasOverlappingDeparture(ctx, store.BusHasOverlappingDepartureParams{BusID: bus.ID, DepartureAt: at, ArrivalEta: arrival})
		if err != nil {
			return 0, nil, err
		}
		if overlap {
			note(day, "Bus "+bus.PlateNumber+" déjà affecté sur ce créneau")
			continue
		}
		if err := crewConflict(ctx, q, s.CompanyID, nil, at, arrival, s.AgentID, s.DriverID, crew); err != nil {
			var se huma.StatusError
			if errors.As(err, &se) && se.GetStatus() == http.StatusConflict {
				note(day, se.Error())
				continue
			}
			return 0, nil, err
		}
		serviceDate := day
		d, err := q.CreateDeparture(ctx, store.CreateDepartureParams{
			CompanyID: s.CompanyID, RouteID: route.ID, BusID: bus.ID, LayoutID: bus.LayoutID,
			DepartureAt: at, ArrivalEta: arrival, BasePrice: price, DriverID: s.DriverID, AgentID: s.AgentID, Notes: s.Notes,
			ScheduleID: &s.ID, ServiceDate: &serviceDate,
		})
		if err != nil {
			return 0, nil, err
		}
		if err := q.CopyRouteStopsToDeparture(ctx, store.CopyRouteStopsToDepartureParams{
			DepartureID: d.ID, DepartureAt: d.DepartureAt, RouteID: route.ID}); err != nil {
			return 0, nil, err
		}
		if err := saveCrew(ctx, q, s.CompanyID, d.ID, crew); err != nil {
			return 0, nil, err
		}
		if _, err := applyDefaultQuota(ctx, q, d, s.CreatedBy); err != nil {
			return 0, nil, err
		}
		created++
	}
	return save()
}

// withdraw retire les départs à venir d'un horaire qui n'ont ni voyageur ni colis, et que personne
// n'a modifiés à la main : supprimés, ou annulés et détachés quand l'historique les retient
// (réservation expirée…). Les autres sont gardés tels quels et listés.
func withdraw(ctx context.Context, tx pgx.Tx, q *store.Queries, scheduleID uuid.UUID, companyID uuid.UUID, now time.Time) (int, []KeptDeparture, error) {
	rows, err := q.ListScheduleFutureDepartures(ctx, store.ListScheduleFutureDeparturesParams{ScheduleID: &scheduleID, After: now})
	if err != nil {
		return 0, nil, err
	}
	removed, kept := 0, []KeptDeparture{}
	for _, r := range rows {
		reason := ""
		switch {
		case r.Status == "cancelled":
			continue // jour sauté : reste noté pour ne pas être recréé
		case r.Status != "scheduled":
			reason = "Embarquement en cours"
		case r.Busy:
			reason = "Déjà réservé (voyageurs ou colis)"
		case r.ScheduleOverride:
			reason = "Modifié à la main"
		}
		if reason != "" {
			kept = append(kept, KeptDeparture{ID: r.ID, DepartureAt: r.DepartureAt, Reason: reason})
			continue
		}
		sp, err := tx.Begin(ctx) // point de sauvegarde : la suppression peut être refusée par l'historique
		if err != nil {
			return 0, nil, err
		}
		if err := q.WithTx(sp).DeleteDeparture(ctx, r.ID); err != nil {
			_ = sp.Rollback(ctx)
			if !database.IsForeignKeyViolation(err) {
				return 0, nil, err
			}
			if _, err := q.SetDepartureStatus(ctx, store.SetDepartureStatusParams{ID: r.ID, CompanyID: companyID, Status: "cancelled"}); err != nil {
				return 0, nil, err
			}
			if err := q.DetachDepartureFromSchedule(ctx, r.ID); err != nil {
				return 0, nil, err
			}
		} else if err := sp.Commit(ctx); err != nil {
			return 0, nil, err
		}
		removed++
	}
	return removed, kept, nil
}

// upcoming : prochains départs de l'horaire (aperçu).
func upcoming(ctx context.Context, q *store.Queries, scheduleID uuid.UUID, now time.Time) ([]time.Time, error) {
	rows, err := q.ListScheduleFutureDepartures(ctx, store.ListScheduleFutureDeparturesParams{ScheduleID: &scheduleID, After: now})
	out := []time.Time{}
	for _, r := range rows {
		if r.Status != "cancelled" && len(out) < 10 {
			out = append(out, r.DepartureAt)
		}
	}
	return out, err
}

// scheduleParams vérifie et complète la saisie (bus, jours, dates, personnel).
type scheduleParams struct {
	route    store.Route
	bus      store.Bus
	time     string
	weekdays []int16
	price    int64
	driver   *uuid.UUID
	agent    *uuid.UUID
	crew     []CrewMemberInput
	notes    string
	startsOn time.Time
	endsOn   *time.Time
	active   bool
	crewJSON []byte
}

func checkSchedule(ctx context.Context, q *store.Queries, companyID, routeID uuid.UUID, in ScheduleInput, now time.Time) (scheduleParams, error) {
	p := scheduleParams{time: in.Time, price: in.BasePrice, driver: in.DriverID, agent: in.AgentID, crew: in.Crew, notes: in.Notes, active: true}
	if in.Active != nil {
		p.active = *in.Active
	}
	var err error
	if p.route, err = q.GetRoute(ctx, store.GetRouteParams{ID: routeID, CompanyID: companyID}); database.IsNotFound(err) {
		return p, huma.Error422UnprocessableEntity("Ligne inconnue")
	} else if err != nil {
		return p, err
	}
	if !p.route.IsActive {
		return p, huma.Error422UnprocessableEntity("Cette ligne est désactivée")
	}
	if p.bus, err = q.GetBus(ctx, store.GetBusParams{ID: in.BusID, CompanyID: companyID}); database.IsNotFound(err) {
		return p, huma.Error422UnprocessableEntity("Bus inconnu")
	} else if err != nil {
		return p, err
	}
	if p.bus.Status != "active" {
		return p, huma.Error409Conflict("Ce bus n'est pas disponible (statut : " + p.bus.Status + ")")
	}
	if p.time == "" {
		return p, huma.Error422UnprocessableEntity("Indiquez l'heure de départ (HH:MM)")
	}
	for _, d := range in.Weekdays {
		if d < 1 || d > 7 {
			return p, huma.Error422UnprocessableEntity("Jours de la semaine : 1 (lundi) à 7 (dimanche)")
		}
		if !slices.Contains(p.weekdays, int16(d)) {
			p.weekdays = append(p.weekdays, int16(d))
		}
	}
	slices.Sort(p.weekdays)
	if len(p.weekdays) == 7 {
		p.weekdays = []int16{}
	}
	if p.weekdays == nil {
		p.weekdays = []int16{}
	}
	p.startsOn = todayBenin(now)
	if in.StartsOn != "" {
		if p.startsOn, err = time.Parse(time.DateOnly, in.StartsOn); err != nil {
			return p, huma.Error422UnprocessableEntity("Date de début invalide")
		}
	}
	if in.EndsOn != "" {
		end, err := time.Parse(time.DateOnly, in.EndsOn)
		if err != nil {
			return p, huma.Error422UnprocessableEntity("Date de fin invalide")
		}
		if end.Before(p.startsOn) {
			return p, huma.Error422UnprocessableEntity("La date de fin doit suivre la date de début")
		}
		p.endsOn = &end
	}
	ids := []*uuid.UUID{p.driver, p.agent}
	if p.crew == nil {
		p.crew = []CrewMemberInput{}
	}
	for i := range p.crew {
		ids = append(ids, &p.crew[i].StaffID)
	}
	if err := ensureStaff(ctx, q, companyID, ids...); err != nil {
		return p, err
	}
	p.crewJSON, _ = json.Marshal(p.crew)
	return p, nil
}

func (h *Handler) loadSchedule(ctx context.Context, q *store.Queries, companyID, id uuid.UUID) (*Schedule, error) {
	rows, err := q.ListSchedules(ctx, companyID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ID == id {
			s := toSchedule(store.DepartureSchedule{ID: r.ID, CompanyID: r.CompanyID, RouteID: r.RouteID, BusID: r.BusID,
				DepartureTime: r.DepartureTime, Weekdays: r.Weekdays, BasePrice: r.BasePrice, DriverID: r.DriverID, AgentID: r.AgentID,
				Crew: r.Crew, Notes: r.Notes, StartsOn: r.StartsOn, EndsOn: r.EndsOn, Active: r.Active, Issues: r.Issues},
				r.RouteName, r.PlateNumber, r.UpcomingCount, r.NextDepartureAt)
			return &s, nil
		}
	}
	return nil, apix.NotFound("Horaire")
}

// tx exécute fn ; en aperçu, tout est annulé à la fin (le résultat calculé est gardé).
func (h *Handler) scheduleTx(ctx context.Context, p auth.Principal, dryRun bool, fn func(tx pgx.Tx, q *store.Queries) error) error {
	err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
		if err := fn(tx, q); err != nil {
			return err
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		return nil
	}
	return err
}

func (h *Handler) registerSchedules(api huma.API) {
	planners := []string{auth.RoleCompanyAdmin, auth.RoleAgencyManager}
	const tag = "Horaires"

	huma.Register(api, apix.Op("list-departure-schedules", http.MethodGet, "/v1/admin/departure-schedules", tag,
		"Horaires permanents de la compagnie", apix.Staff),
		func(ctx context.Context, _ *struct{}) (*struct{ Body []Schedule }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body []Schedule }{Body: []Schedule{}}
			err := h.DB.Tx(ctx, p.Tenant(), func(_ pgx.Tx, q *store.Queries) error {
				rows, err := q.ListSchedules(ctx, p.CompanyID)
				for _, r := range rows {
					out.Body = append(out.Body, toSchedule(store.DepartureSchedule{ID: r.ID, CompanyID: r.CompanyID, RouteID: r.RouteID,
						BusID: r.BusID, DepartureTime: r.DepartureTime, Weekdays: r.Weekdays, BasePrice: r.BasePrice, DriverID: r.DriverID,
						AgentID: r.AgentID, Crew: r.Crew, Notes: r.Notes, StartsOn: r.StartsOn, EndsOn: r.EndsOn, Active: r.Active,
						Issues: r.Issues}, r.RouteName, r.PlateNumber, r.UpcomingCount, r.NextDepartureAt))
				}
				return err
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("create-departure-schedule", http.MethodPost, "/v1/admin/departure-schedules", tag,
		"Créer un horaire permanent", apix.Staff, planners...),
		fmt.Sprintf("Les départs sont créés automatiquement jusqu'à %d jours à l'avance, au fil des jours. ", ScheduleHorizonDays)+
			"Avec `from_departure_id`, transforme un départ existant en départ régulier (ligne, bus, heure, tarif et équipage repris, "+
			"modifiables dans la demande). `dry_run: true` renvoie l'aperçu sans rien enregistrer."),
		func(ctx context.Context, in *struct {
			Body struct {
				RouteID         uuid.UUID `json:"route_id,omitempty" doc:"Obligatoire, sauf à partir d'un départ"`
				FromDepartureID uuid.UUID `json:"from_departure_id,omitempty" doc:"Départ existant à rendre régulier"`
				ScheduleInput
			}
		}) (*struct {
			Status int
			Body   ScheduleChange
		}, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			now := time.Now()
			out := &struct {
				Status int
				Body   ScheduleChange
			}{Status: http.StatusCreated, Body: ScheduleChange{DryRun: b.DryRun, Kept: []KeptDeparture{}, Issues: []ScheduleIssue{}, Upcoming: []time.Time{}}}
			err := h.scheduleTx(ctx, p, b.DryRun, func(_ pgx.Tx, q *store.Queries) error {
				routeID := b.RouteID
				var from *store.Departure
				if b.FromDepartureID != uuid.Nil {
					d, err := q.GetDeparture(ctx, b.FromDepartureID)
					if err != nil || d.CompanyID != p.CompanyID {
						return apix.NotFound("Départ")
					}
					if d.ScheduleID != nil {
						return huma.Error409Conflict("Ce départ fait déjà partie d'un horaire")
					}
					if d.Status != "scheduled" {
						return huma.Error409Conflict("Seul un départ programmé (pas encore en embarquement) peut devenir régulier")
					}
					from, routeID = &d, d.RouteID
					local := d.DepartureAt.In(beninTZ)
					if b.BusID == uuid.Nil {
						b.BusID = d.BusID
					}
					if b.Time == "" {
						b.Time = local.Format("15:04")
					}
					if b.StartsOn == "" {
						b.StartsOn = local.Format(time.DateOnly)
					}
					if b.BasePrice == 0 {
						b.BasePrice = d.BasePrice
					}
					if b.DriverID == nil && b.AgentID == nil && b.Crew == nil {
						b.DriverID, b.AgentID = d.DriverID, d.AgentID
						rows, err := q.ListDepartureCrew(ctx, d.ID)
						if err != nil {
							return err
						}
						for _, m := range rows {
							if (d.DriverID == nil || m.StaffID != *d.DriverID) && (d.AgentID == nil || m.StaffID != *d.AgentID) {
								b.Crew = append(b.Crew, CrewMemberInput{StaffID: m.StaffID, Role: m.Role})
							}
						}
					}
					if b.Notes == "" {
						b.Notes = d.Notes
					}
				}
				if routeID == uuid.Nil {
					return huma.Error422UnprocessableEntity("Choisissez la ligne")
				}
				sp, err := checkSchedule(ctx, q, p.CompanyID, routeID, b.ScheduleInput, now)
				if err != nil {
					return err
				}
				// Tarif égal à celui de la ligne : on suit la ligne (un changement de tarif s'appliquera aux nouveaux départs).
				if sp.price == sp.route.BasePrice {
					sp.price = 0
				}
				staffID := p.ID
				s, err := q.CreateSchedule(ctx, store.CreateScheduleParams{CompanyID: p.CompanyID, RouteID: routeID, BusID: sp.bus.ID,
					DepartureTime: sp.time, Weekdays: sp.weekdays, BasePrice: sp.price, DriverID: sp.driver, AgentID: sp.agent,
					Crew: sp.crewJSON, Notes: sp.notes, StartsOn: sp.startsOn, EndsOn: sp.endsOn, Active: sp.active, CreatedBy: &staffID})
				if err != nil {
					return err
				}
				if from != nil {
					day := dateOnly(from.DepartureAt.In(beninTZ))
					if err := q.AttachDepartureToSchedule(ctx, store.AttachDepartureToScheduleParams{ID: from.ID, ScheduleID: &s.ID, ServiceDate: &day}); err != nil {
						return err
					}
				}
				if out.Body.Created, out.Body.Issues, err = generate(ctx, q, s, now); err != nil {
					return err
				}
				if out.Body.Upcoming, err = upcoming(ctx, q, s.ID, now); err != nil {
					return err
				}
				if out.Body.Schedule, err = h.loadSchedule(ctx, q, p.CompanyID, s.ID); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "schedule.created", "departure_schedule", s.ID.String(), nil,
					map[string]any{"route_id": routeID, "bus_id": sp.bus.ID, "time": sp.time, "weekdays": sp.weekdays,
						"from_departure_id": b.FromDepartureID, "created": out.Body.Created})
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("update-departure-schedule", http.MethodPut, "/v1/admin/departure-schedules/{id}", tag,
		"Modifier, suspendre ou reprendre un horaire", apix.Staff, planners...),
		"Les départs à venir sans voyageur ni colis sont recréés avec les nouveaux réglages. Ceux déjà réservés, ou modifiés "+
			"à la main, restent tels quels et sont listés dans `kept` (à modifier un par un si besoin). "+
			"`active: false` suspend l'horaire. `dry_run: true` renvoie l'aperçu sans rien enregistrer."),
		func(ctx context.Context, in *struct {
			ID   uuid.UUID `path:"id"`
			Body ScheduleInput
		}) (*struct{ Body ScheduleChange }, error) {
			p := auth.MustFromContext(ctx)
			b := in.Body
			now := time.Now()
			out := &struct{ Body ScheduleChange }{Body: ScheduleChange{DryRun: b.DryRun, Kept: []KeptDeparture{}, Issues: []ScheduleIssue{}, Upcoming: []time.Time{}}}
			err := h.scheduleTx(ctx, p, b.DryRun, func(tx pgx.Tx, q *store.Queries) error {
				before, err := q.GetScheduleForUpdate(ctx, store.GetScheduleForUpdateParams{ID: in.ID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return apix.NotFound("Horaire")
				}
				if err != nil {
					return err
				}
				sp, err := checkSchedule(ctx, q, p.CompanyID, before.RouteID, b, now)
				if err != nil {
					return err
				}
				if sp.price == sp.route.BasePrice {
					sp.price = 0
				}
				s, err := q.UpdateSchedule(ctx, store.UpdateScheduleParams{ID: in.ID, CompanyID: p.CompanyID, BusID: sp.bus.ID,
					DepartureTime: sp.time, Weekdays: sp.weekdays, BasePrice: sp.price, DriverID: sp.driver, AgentID: sp.agent,
					Crew: sp.crewJSON, Notes: sp.notes, StartsOn: sp.startsOn, EndsOn: sp.endsOn, Active: sp.active})
				if err != nil {
					return err
				}
				if out.Body.Removed, out.Body.Kept, err = withdraw(ctx, tx, q, s.ID, p.CompanyID, now); err != nil {
					return err
				}
				if out.Body.Created, out.Body.Issues, err = generate(ctx, q, s, now); err != nil {
					return err
				}
				if out.Body.Upcoming, err = upcoming(ctx, q, s.ID, now); err != nil {
					return err
				}
				if out.Body.Schedule, err = h.loadSchedule(ctx, q, p.CompanyID, s.ID); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "schedule.updated", "departure_schedule", s.ID.String(),
					map[string]any{"bus_id": before.BusID, "time": before.DepartureTime, "weekdays": before.Weekdays, "active": before.Active},
					map[string]any{"bus_id": s.BusID, "time": s.DepartureTime, "weekdays": s.Weekdays, "active": s.Active,
						"removed": out.Body.Removed, "kept": len(out.Body.Kept), "created": out.Body.Created})
			})
			return out, apix.Err(err)
		})

	huma.Register(api, apix.WithDescription(apix.Op("delete-departure-schedule", http.MethodDelete, "/v1/admin/departure-schedules/{id}", tag,
		"Supprimer un horaire", apix.Staff, planners...),
		"Les départs à venir sans voyageur ni colis sont retirés ; les autres restent, comme des départs simples."),
		func(ctx context.Context, in *struct {
			ID uuid.UUID `path:"id"`
		}) (*struct{ Body ScheduleChange }, error) {
			p := auth.MustFromContext(ctx)
			out := &struct{ Body ScheduleChange }{Body: ScheduleChange{Kept: []KeptDeparture{}, Issues: []ScheduleIssue{}, Upcoming: []time.Time{}}}
			err := h.DB.Tx(ctx, p.Tenant(), func(tx pgx.Tx, q *store.Queries) error {
				s, err := q.GetScheduleForUpdate(ctx, store.GetScheduleForUpdateParams{ID: in.ID, CompanyID: p.CompanyID})
				if database.IsNotFound(err) {
					return apix.NotFound("Horaire")
				}
				if err != nil {
					return err
				}
				if out.Body.Removed, out.Body.Kept, err = withdraw(ctx, tx, q, s.ID, p.CompanyID, time.Now()); err != nil {
					return err
				}
				if err := q.DeleteSchedule(ctx, store.DeleteScheduleParams{ID: s.ID, CompanyID: p.CompanyID}); err != nil {
					return err
				}
				return apix.Audit(ctx, q, &p.CompanyID, "schedule.deleted", "departure_schedule", s.ID.String(),
					map[string]any{"route_id": s.RouteID, "time": s.DepartureTime}, map[string]any{"removed": out.Body.Removed, "kept": len(out.Body.Kept)})
			})
			return out, apix.Err(err)
		})
}

// GenerateSchedulesWorker complète régulièrement les départs de tous les horaires actifs (un horaire
// en échec n'empêche pas les autres).
type GenerateSchedulesWorker struct {
	river.WorkerDefaults[jobs.GenerateSchedulesArgs]
	DB *database.DB
}

func (w *GenerateSchedulesWorker) Work(ctx context.Context, _ *river.Job[jobs.GenerateSchedulesArgs]) error {
	now := time.Now()
	var list []store.ListActiveScheduleIDsRow
	if err := w.DB.Tx(ctx, database.System(), func(_ pgx.Tx, q *store.Queries) error {
		var err error
		list, err = q.ListActiveScheduleIDs(ctx, todayBenin(now))
		return err
	}); err != nil {
		return err
	}
	var failed error
	for _, row := range list {
		err := w.DB.Tx(ctx, database.Staff(row.CompanyID), func(_ pgx.Tx, q *store.Queries) error {
			s, err := q.GetScheduleForUpdate(ctx, store.GetScheduleForUpdateParams{ID: row.ID, CompanyID: row.CompanyID})
			if err != nil {
				return err
			}
			_, _, err = generate(ctx, q, s, now)
			return err
		})
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("horaire %s : %w", row.ID, err))
		}
	}
	return failed
}

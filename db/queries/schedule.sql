-- name: CreateSchedule :one
INSERT INTO departure_schedules (company_id, route_id, bus_id, departure_time, weekdays, base_price, driver_id, agent_id,
                                 crew, notes, starts_on, ends_on, active, created_by)
VALUES (@company_id, @route_id, @bus_id, @departure_time, @weekdays, @base_price, @driver_id, @agent_id,
        @crew, @notes, @starts_on, sqlc.narg('ends_on'), @active, @created_by)
RETURNING *;

-- name: UpdateSchedule :one
UPDATE departure_schedules
SET bus_id = @bus_id, departure_time = @departure_time, weekdays = @weekdays, base_price = @base_price,
    driver_id = @driver_id, agent_id = @agent_id, crew = @crew, notes = @notes,
    starts_on = @starts_on, ends_on = sqlc.narg('ends_on'), active = @active
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: GetScheduleForUpdate :one
SELECT * FROM departure_schedules WHERE id = @id AND company_id = @company_id FOR UPDATE;

-- name: ListSchedules :many
-- Horaires de la compagnie, avec la ligne, le bus et le nombre de départs à venir déjà créés.
SELECT s.*, r.name AS route_name, b.plate_number,
       (SELECT count(*) FROM departures d
        WHERE d.schedule_id = s.id AND d.departure_at > now() AND d.status <> 'cancelled')::int AS upcoming_count,
       -- Aucun départ à venir : 1970-01-01 (epoch).
       COALESCE((SELECT min(d.departure_at) FROM departures d
        WHERE d.schedule_id = s.id AND d.departure_at > now() AND d.status <> 'cancelled'), 'epoch'::timestamptz)::timestamptz AS next_departure_at
FROM departure_schedules s
JOIN routes r ON r.id = s.route_id
JOIN buses b ON b.id = s.bus_id
WHERE s.company_id = @company_id
ORDER BY r.name, s.departure_time;

-- name: ListActiveScheduleIDs :many
-- Tous les horaires actifs, toutes compagnies (création automatique des départs).
SELECT id, company_id FROM departure_schedules WHERE active AND (ends_on IS NULL OR ends_on >= @today::date);

-- name: SetScheduleGenerated :exec
UPDATE departure_schedules SET issues = @issues, generated_until = @generated_until WHERE id = @id;

-- name: DeleteSchedule :exec
DELETE FROM departure_schedules WHERE id = @id AND company_id = @company_id;

-- name: ListScheduleServiceDates :many
SELECT service_date::date FROM departures WHERE schedule_id = @schedule_id AND service_date >= @from_date::date;

-- name: ListScheduleFutureDepartures :many
-- Départs à venir d'un horaire. busy : des voyageurs ou des colis en dépendent (ne pas y toucher).
SELECT d.id, d.departure_at, d.status, d.schedule_override,
       (EXISTS (SELECT 1 FROM bookings b WHERE b.departure_id = d.id AND b.status IN ('pending', 'confirmed'))
        OR EXISTS (SELECT 1 FROM parcels p WHERE p.departure_id = d.id))::boolean AS busy
FROM departures d
WHERE d.schedule_id = @schedule_id AND d.departure_at > @after::timestamptz
ORDER BY d.departure_at;

-- name: AttachDepartureToSchedule :exec
UPDATE departures SET schedule_id = @schedule_id, service_date = @service_date WHERE id = @id;

-- name: DetachDepartureFromSchedule :exec
UPDATE departures SET schedule_id = NULL, service_date = NULL WHERE id = @id;

-- name: DeleteDeparture :exec
DELETE FROM departures WHERE id = @id;

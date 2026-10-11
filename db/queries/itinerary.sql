-- ============================ Points d’arrêt ============================

-- name: ListStopPoints :many
SELECT sp.id, sp.company_id, sp.city_id, sp.agency_id, sp.name, sp.address, sp.latitude, sp.longitude, sp.is_active, sp.created_at, sp.updated_at,
       c.name AS city_name, a.name AS agency_name
FROM stop_points sp
JOIN cities c ON c.id = sp.city_id
LEFT JOIN agencies a ON a.id = sp.agency_id
WHERE sp.company_id = $1
ORDER BY c.name, sp.name;

-- name: GetStopPoint :one
SELECT sp.id, sp.company_id, sp.city_id, sp.agency_id, sp.name, sp.address, sp.latitude, sp.longitude, sp.is_active, sp.created_at, sp.updated_at FROM stop_points sp WHERE sp.id = @id AND sp.company_id = @company_id;

-- name: GetStopPointByAgency :one
SELECT sp.id, sp.company_id, sp.city_id, sp.agency_id, sp.name, sp.address, sp.latitude, sp.longitude, sp.is_active, sp.created_at, sp.updated_at FROM stop_points sp WHERE sp.agency_id = $1;

-- name: CreateStopPoint :one
INSERT INTO stop_points (company_id, city_id, agency_id, name, address, latitude, longitude, is_active)
VALUES (@company_id, @city_id, @agency_id, @name, @address, @latitude, @longitude, @is_active)
RETURNING *;

-- name: UpdateStopPoint :one
UPDATE stop_points SET name = @name, address = @address, latitude = @latitude, longitude = @longitude, is_active = @is_active
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: IsStopPointUsed :one
SELECT EXISTS (SELECT 1 FROM route_stops WHERE stop_point_id = $1)::bool;

-- ============================ Arrêts et grille d’une ligne ============================

-- name: ListRouteStops :many
SELECT rs.*, sp.name AS stop_name, sp.city_id, sp.agency_id, c.name AS city_name
FROM route_stops rs
JOIN stop_points sp ON sp.id = rs.stop_point_id
JOIN cities c ON c.id = sp.city_id
WHERE rs.route_id = $1
ORDER BY rs.seq;

-- name: DeleteRouteStops :exec
DELETE FROM route_stops WHERE route_id = $1;

-- name: InsertRouteStop :exec
INSERT INTO route_stops (route_id, company_id, seq, stop_point_id, offset_minutes, pause_minutes, can_board, can_alight)
VALUES (@route_id, @company_id, @seq, @stop_point_id, @offset_minutes, @pause_minutes, @can_board, @can_alight);

-- name: ListRouteFares :many
SELECT * FROM route_fares WHERE route_id = $1;

-- name: DeleteRouteFares :exec
DELETE FROM route_fares WHERE route_id = $1;

-- name: InsertRouteFare :exec
INSERT INTO route_fares (route_id, company_id, from_city_id, to_city_id, price)
VALUES (@route_id, @company_id, @from_city_id, @to_city_id, @price);

-- name: SetRouteItinerary :one
-- Extrémités et durée recalculées à partir des arrêts (compatibilité des écrans existants).
UPDATE routes SET origin_agency_id = @origin_agency_id, destination_agency_id = @destination_agency_id,
                  duration_minutes = @duration_minutes, pricing_mode = @pricing_mode
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: ListRouteTerminals :many
SELECT rt.*, oc.name AS origin_city_name, dc.name AS destination_city_name
FROM route_terminals rt
JOIN routes r ON r.id = rt.route_id
JOIN cities oc ON oc.id = rt.origin_city_id
JOIN cities dc ON dc.id = rt.destination_city_id
WHERE r.company_id = $1;

-- ============================ Arrêts d’un départ (copie figée) ============================

-- name: CopyRouteStopsToDeparture :exec
INSERT INTO departure_stops (departure_id, company_id, seq, stop_point_id, scheduled_at, pause_minutes, can_board, can_alight)
SELECT @departure_id, rs.company_id, rs.seq, rs.stop_point_id,
       sqlc.arg(departure_at)::timestamptz + make_interval(mins => rs.offset_minutes), rs.pause_minutes, rs.can_board, rs.can_alight
FROM route_stops rs
WHERE rs.route_id = @route_id;

-- name: ListDepartureStops :many
SELECT ds.*, sp.name AS stop_name, sp.city_id, sp.agency_id, c.name AS city_name, sp.address,
       sp.latitude, sp.longitude
FROM departure_stops ds
JOIN stop_points sp ON sp.id = ds.stop_point_id
JOIN cities c ON c.id = sp.city_id
WHERE ds.departure_id = $1
ORDER BY ds.seq;

-- name: RescheduleDepartureStops :exec
-- Nouvel horaire : tous les arrêts sont décalés d'autant.
UPDATE departure_stops SET scheduled_at = scheduled_at + (sqlc.arg(new_departure_at)::timestamptz - sqlc.arg(old_departure_at)::timestamptz)
WHERE departure_id = @departure_id;

-- ============================ Équipage ============================

-- name: ListDepartureCrew :many
SELECT dc.staff_id, dc.role, s.full_name, s.phone
FROM departure_crew dc
JOIN staff_users s ON s.id = dc.staff_id
WHERE dc.departure_id = $1
ORDER BY dc.role, s.full_name;

-- name: DeleteDepartureCrew :exec
DELETE FROM departure_crew WHERE departure_id = $1;

-- name: InsertDepartureCrew :exec
INSERT INTO departure_crew (departure_id, company_id, staff_id, role) VALUES (@departure_id, @company_id, @staff_id, @role);

-- name: StaffBusyOnDeparture :one
-- Membre d'équipage déjà affecté à un autre départ qui chevauche ce créneau.
SELECT d.id, d.departure_at, r.name AS route_name
FROM departures d
JOIN routes r ON r.id = d.route_id
WHERE d.company_id = @company_id
  AND d.status NOT IN ('cancelled', 'arrived')
  AND (sqlc.narg('exclude_id')::uuid IS NULL OR d.id <> sqlc.narg('exclude_id')::uuid)
  AND d.departure_at < @to_ts AND d.arrival_eta > @from_ts
  AND (d.agent_id = @staff_id OR d.driver_id = @staff_id
       OR EXISTS (SELECT 1 FROM departure_crew c WHERE c.departure_id = d.id AND c.staff_id = @staff_id))
LIMIT 1;

-- name: IsCrewMember :one
SELECT EXISTS (SELECT 1 FROM departure_crew WHERE departure_id = @departure_id AND staff_id = @staff_id)::bool;

-- name: StopDeparted :one
SELECT EXISTS (SELECT 1 FROM departure_stop_visits
               WHERE departure_id = @departure_id AND seq = @seq AND departed_at IS NOT NULL)::bool;

-- name: DepartureLastSeq :one
-- Indice du dernier arrêt d'un départ (tout le trajet = [0, dernier]).
SELECT COALESCE(max(seq), 1)::int FROM departure_stops WHERE departure_id = $1;

-- name: BookingBoardingAt :one
-- Heure prévue à l'arrêt de montée d'une réservation : base des délais d'annulation,
-- de modification et de paiement en agence.
SELECT ds.scheduled_at FROM bookings b
JOIN departure_stops ds ON ds.departure_id = b.departure_id AND ds.seq = b.boarding_seq
WHERE b.id = $1;

-- ============================ Journal des arrets ============================

-- name: ListStopVisits :many
SELECT * FROM departure_stop_visits WHERE departure_id = $1 ORDER BY arrived_at;

-- name: GetStopVisitByClientID :one
SELECT * FROM departure_stop_visits WHERE device_id = @device_id AND client_id = @client_id;

-- name: InsertStopVisit :one
INSERT INTO departure_stop_visits (company_id, departure_id, seq, reason, note, arrived_at, arrival_lat, arrival_lng,
                                   arrival_accuracy, location_mocked, recorded_by, device_id, client_id)
VALUES (@company_id, @departure_id, sqlc.narg(seq), @reason, @note, @arrived_at, sqlc.narg(arrival_lat), sqlc.narg(arrival_lng),
        sqlc.narg(arrival_accuracy), @location_mocked, @recorded_by, sqlc.narg(device_id), sqlc.narg(client_id))
RETURNING *;

-- name: CloseOpenStopVisits :exec
-- Arrêt précédent jamais fermé (oubli, perte de réseau) : clos à l'arrivée suivante.
UPDATE departure_stop_visits SET departed_at = @at
WHERE departure_id = @departure_id AND departed_at IS NULL AND arrived_at <= @at;

-- name: DepartStopVisit :one
UPDATE departure_stop_visits SET departed_at = GREATEST(@departed_at, arrived_at),
                                 departure_lat = sqlc.narg(departure_lat), departure_lng = sqlc.narg(departure_lng),
                                 departure_client_id = sqlc.narg(departure_client_id)
WHERE id = @id AND departed_at IS NULL
RETURNING *;

-- name: GetStopVisitByDepartureClientID :one
SELECT * FROM departure_stop_visits WHERE device_id = @device_id AND departure_client_id = @departure_client_id;

-- name: GetBookingTripView :one
-- Trajet d une reservation pour l affichage (billet, e-mails, facture) : arrets, villes, heures prevues.
SELECT bst.scheduled_at AS boarding_at, bsp.name AS boarding_stop, bsp.address AS boarding_address,
       bsp.agency_id AS boarding_agency_id, bc.name AS boarding_city,
       alt.scheduled_at AS alighting_at, asp.name AS alighting_stop, asp.agency_id AS alighting_agency_id, ac.name AS alighting_city
FROM bookings b
JOIN departure_stops bst ON bst.departure_id = b.departure_id AND bst.seq = b.boarding_seq
JOIN stop_points bsp ON bsp.id = bst.stop_point_id
JOIN cities bc ON bc.id = bsp.city_id
JOIN departure_stops alt ON alt.departure_id = b.departure_id AND alt.seq = b.alighting_seq
JOIN stop_points asp ON asp.id = alt.stop_point_id
JOIN cities ac ON ac.id = asp.city_id
WHERE b.id = $1;

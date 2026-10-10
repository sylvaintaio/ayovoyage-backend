-- name: CreateDeparture :one
INSERT INTO departures (company_id, route_id, bus_id, layout_id, departure_at, arrival_eta, base_price,
                        driver_id, agent_id, notes)
VALUES (@company_id, @route_id, @bus_id, @layout_id, @departure_at, @arrival_eta, @base_price,
        @driver_id, @agent_id, @notes)
RETURNING *;

-- name: UpdateDeparture :one
UPDATE departures SET departure_at = @departure_at, arrival_eta = @arrival_eta, base_price = @base_price,
                      driver_id = @driver_id, agent_id = @agent_id, notes = @notes
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: SetDepartureStatus :one
UPDATE departures SET status = $3 WHERE id = $1 AND company_id = $2 RETURNING *;

-- name: GetDeparture :one
SELECT * FROM departures WHERE id = $1;

-- name: BusHasOverlappingDeparture :one
SELECT EXISTS (
    SELECT 1 FROM departures
    WHERE bus_id = @bus_id AND status <> 'cancelled'
      AND (sqlc.narg('exclude_id')::uuid IS NULL OR id <> sqlc.narg('exclude_id')::uuid)
      AND departure_at < @arrival_eta::timestamptz AND arrival_eta > @departure_at::timestamptz
) AS overlaps;

-- name: ListDepartures :many
SELECT d.*, r.name AS route_name, b.plate_number, l.seat_count,
       (SELECT count(*) FROM booking_seats bs WHERE bs.departure_id = d.id AND bs.status = 'sold')::int AS sold_seats,
       (SELECT count(*) FROM tickets t WHERE t.departure_id = d.id AND t.status = 'used')::int AS boarded_seats
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN buses b ON b.id = d.bus_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE d.company_id = @company_id
  AND d.departure_at >= @from_ts AND d.departure_at < @to_ts
  AND (sqlc.narg('route_id')::uuid IS NULL OR d.route_id = sqlc.narg('route_id')::uuid)
  AND (sqlc.narg('origin_agency_id')::uuid IS NULL OR r.origin_agency_id = sqlc.narg('origin_agency_id')::uuid)
  AND (sqlc.narg('status')::text IS NULL OR d.status = sqlc.narg('status')::text)
  AND (sqlc.narg('staff_id')::uuid IS NULL OR d.agent_id = sqlc.narg('staff_id')::uuid OR d.driver_id = sqlc.narg('staff_id')::uuid)
ORDER BY d.departure_at;

-- name: GetDepartureDetail :one
-- Détail public d'un départ (toutes compagnies).
SELECT d.id, d.company_id, d.route_id, d.layout_id, d.departure_at, d.arrival_eta, d.base_price, d.status,
       r.name AS route_name, r.duration_minutes, r.included_baggage_kg,
       oa.id AS origin_agency_id, oa.name AS origin_agency_name, oa.address AS origin_agency_address, oc.name AS origin_city,
       da.id AS destination_agency_id, da.name AS destination_agency_name, dc.name AS destination_city,
       co.name AS company_name, co.slug AS company_slug, co.sales_cutoff_minutes,
       b.model AS bus_model, b.amenities AS bus_amenities, l.seat_count
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = d.company_id
JOIN buses b ON b.id = d.bus_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE d.id = $1;

-- name: SearchDepartures :many
SELECT d.id, d.company_id, d.route_id, d.departure_at, d.arrival_eta, d.base_price, d.status,
       r.name AS route_name, r.duration_minutes, r.included_baggage_kg,
       oa.id AS origin_agency_id, oa.name AS origin_agency_name, oc.name AS origin_city,
       da.id AS destination_agency_id, da.name AS destination_agency_name, dc.name AS destination_city,
       co.name AS company_name, co.slug AS company_slug,
       b.model AS bus_model, b.amenities AS bus_amenities, l.seat_count,
       (l.seat_count
          - (SELECT count(*) FROM booking_seats bs
             WHERE bs.departure_id = d.id
               AND (bs.status = 'sold' OR (bs.status = 'held' AND bs.hold_expires_at > now())))
          - (SELECT count(*) FROM departure_blocked_seats x WHERE x.departure_id = d.id))::int AS available_seats
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = d.company_id
JOIN buses b ON b.id = d.bus_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE oc.slug = @from_city AND dc.slug = @to_city
  AND d.departure_at >= @from_ts AND d.departure_at < @to_ts
  AND d.status IN ('scheduled', 'boarding')
  AND d.departure_at > now() + make_interval(mins => co.sales_cutoff_minutes)
  AND co.status = 'active'
  AND (sqlc.narg('company_id')::uuid IS NULL OR d.company_id = sqlc.narg('company_id')::uuid)
ORDER BY d.departure_at;

-- name: ListUpcomingDeparturesFromAgency :many
SELECT d.id, d.departure_at, d.arrival_eta, d.base_price, d.status, dc.name AS destination_city,
       da.name AS destination_agency_name,
       (l.seat_count
          - (SELECT count(*) FROM booking_seats bs
             WHERE bs.departure_id = d.id
               AND (bs.status = 'sold' OR (bs.status = 'held' AND bs.hold_expires_at > now())))
          - (SELECT count(*) FROM departure_blocked_seats x WHERE x.departure_id = d.id))::int AS available_seats
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE r.origin_agency_id = $1
  AND d.status IN ('scheduled', 'boarding')
  AND d.departure_at > now()
  AND d.departure_at < now() + interval '3 days'
ORDER BY d.departure_at
LIMIT 30;

-- name: GetDepartureSeatMap :many
-- État de chaque case du plan pour un départ. L'index unique partiel garantit
-- au plus une ligne booking_seats active par siège : pas de doublon possible.
SELECT c.id, c.row_index, c.col_index, c.kind, c.seat_label, c.category_id, c.extra_surcharge, c.attributes,
       (CASE
            WHEN c.kind <> 'seat' THEN 'none'
            WHEN blk.seat_cell_id IS NOT NULL THEN 'blocked'
            WHEN t.status = 'used' THEN 'boarded'
            WHEN bs.status = 'sold' THEN 'sold'
            WHEN bs.status = 'held' AND bs.hold_expires_at > now() THEN 'held'
            ELSE 'available'
        END)::text AS state,
       bs.booking_id, bs.passenger_name
FROM seat_layout_cells c
LEFT JOIN booking_seats bs
       ON bs.seat_cell_id = c.id AND bs.departure_id = @departure_id AND bs.status IN ('held', 'sold')
LEFT JOIN tickets t ON t.booking_seat_id = bs.id
LEFT JOIN departure_blocked_seats blk ON blk.seat_cell_id = c.id AND blk.departure_id = @departure_id
WHERE c.layout_id = @layout_id
ORDER BY c.row_index, c.col_index;

-- name: BlockDepartureSeat :exec
INSERT INTO departure_blocked_seats (departure_id, seat_cell_id, company_id, reason, blocked_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (departure_id, seat_cell_id) DO UPDATE SET reason = EXCLUDED.reason;

-- name: UnblockDepartureSeat :exec
DELETE FROM departure_blocked_seats WHERE departure_id = $1 AND seat_cell_id = $2 AND company_id = $3;

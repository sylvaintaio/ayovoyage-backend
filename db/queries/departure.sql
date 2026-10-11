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
-- sold_seats : passagers vendus (un siège revendu après une descente compte deux fois) ;
-- peak_seats : charge maximale du car sur un tronçon (places réellement occupées au plus fort).
-- Filtre agence : départs qui s'arrêtent à cette agence pour embarquer (pas seulement ceux qui en partent).
-- Filtre personnel : contrôleur, chauffeur ou autre membre de l'équipage.
SELECT d.*, r.name AS route_name, b.plate_number, l.seat_count,
       (SELECT count(*) FROM booking_seats bs WHERE bs.departure_id = d.id AND bs.status = 'sold')::int AS sold_seats,
       (SELECT count(*) FROM tickets t WHERE t.departure_id = d.id AND t.status = 'used')::int AS boarded_seats,
       COALESCE((SELECT max(n) FROM (
           SELECT count(DISTINCT g.seat_cell_id) AS n FROM booking_seat_segments g
           JOIN booking_seats x ON x.id = g.booking_seat_id
           WHERE g.departure_id = d.id AND x.status = 'sold'
           GROUP BY g.segment) loads), 0)::int AS peak_seats
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN buses b ON b.id = d.bus_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE d.company_id = @company_id
  AND d.departure_at >= @from_ts AND d.departure_at < @to_ts
  AND (sqlc.narg('route_id')::uuid IS NULL OR d.route_id = sqlc.narg('route_id')::uuid)
  AND (sqlc.narg('origin_agency_id')::uuid IS NULL OR EXISTS (
        SELECT 1 FROM departure_stops ds JOIN stop_points sp ON sp.id = ds.stop_point_id
        WHERE ds.departure_id = d.id AND ds.can_board AND sp.agency_id = sqlc.narg('origin_agency_id')::uuid))
  AND (sqlc.narg('status')::text IS NULL OR d.status = sqlc.narg('status')::text)
  AND (sqlc.narg('staff_id')::uuid IS NULL OR d.agent_id = sqlc.narg('staff_id')::uuid OR d.driver_id = sqlc.narg('staff_id')::uuid
       OR EXISTS (SELECT 1 FROM departure_crew c WHERE c.departure_id = d.id AND c.staff_id = sqlc.narg('staff_id')::uuid))
ORDER BY d.departure_at;

-- name: GetDepartureDetail :one
-- Détail public d'un départ (toutes compagnies).
SELECT d.id, d.company_id, d.route_id, d.layout_id, d.departure_at, d.arrival_eta, d.base_price, d.status,
       r.name AS route_name, r.duration_minutes, r.included_baggage_kg,
       oa.id AS origin_agency_id, oa.name AS origin_agency_name, oa.address AS origin_agency_address, oc.name AS origin_city,
       r.destination_agency_id, rt.destination_stop_name AS destination_agency_name, dc.name AS destination_city,
       co.name AS company_name, co.slug AS company_slug, co.sales_cutoff_minutes,
       b.model AS bus_model, b.amenities AS bus_amenities, l.seat_count
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN route_terminals rt ON rt.route_id = r.id
JOIN cities dc ON dc.id = rt.destination_city_id
JOIN companies co ON co.id = d.company_id
JOIN buses b ON b.id = d.bus_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE d.id = $1;

-- name: SearchDepartures :many
-- Départs qui desservent la ville de montée puis la ville de descente (n'importe quels arrêts
-- de la ligne, pas seulement ses extrémités). Une ligne par agence de montée possible ;
-- descente au premier arrêt de la ville d'arrivée. Places libres comptées sur ce tronçon.
-- Vente possible tant que le car n'a pas quitté l'arrêt de montée (journal des arrêts),
-- et en ligne jusqu'à l'heure prévue à cet arrêt moins le délai de vente.
SELECT d.id, d.company_id, d.route_id, d.departure_at, d.arrival_eta, d.base_price, d.status,
       r.name AS route_name, r.included_baggage_kg, r.pricing_mode, r.base_price AS route_price,
       bs.seq AS boarding_seq, bs.scheduled_at AS boarding_at, bsp.name AS boarding_stop_name, bsp.agency_id AS boarding_agency_id,
       bc.id AS boarding_city_id, bc.name AS boarding_city,
       al.seq AS alighting_seq, al.scheduled_at AS alighting_at, asp.name AS alighting_stop_name,
       ac.id AS alighting_city_id, ac.name AS alighting_city,
       co.name AS company_name, co.slug AS company_slug,
       b.model AS bus_model, b.amenities AS bus_amenities, l.seat_count,
       (l.seat_count
          - (SELECT count(DISTINCT g.seat_cell_id) FROM booking_seat_segments g
             JOIN booking_seats x ON x.id = g.booking_seat_id
             WHERE g.departure_id = d.id AND g.segment >= bs.seq AND g.segment < al.seq
               AND (x.status = 'sold' OR (x.status = 'held' AND x.hold_expires_at > now())))
          - (SELECT count(*) FROM departure_blocked_seats x WHERE x.departure_id = d.id))::int AS available_seats
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN departure_stops bs ON bs.departure_id = d.id AND bs.can_board
JOIN stop_points bsp ON bsp.id = bs.stop_point_id
JOIN cities bc ON bc.id = bsp.city_id
JOIN LATERAL (
    SELECT a.seq, a.scheduled_at, a.stop_point_id
    FROM departure_stops a
    JOIN stop_points ap ON ap.id = a.stop_point_id
    JOIN cities acx ON acx.id = ap.city_id
    WHERE a.departure_id = d.id AND a.can_alight AND a.seq > bs.seq AND acx.slug = @to_city
    ORDER BY a.seq
    LIMIT 1
) al ON true
JOIN stop_points asp ON asp.id = al.stop_point_id
JOIN cities ac ON ac.id = asp.city_id
JOIN companies co ON co.id = d.company_id
JOIN buses b ON b.id = d.bus_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE bc.slug = @from_city AND ac.id <> bc.id
  AND bs.scheduled_at >= @from_ts AND bs.scheduled_at < @to_ts
  AND d.status IN ('scheduled', 'boarding', 'departed')
  AND (d.status <> 'departed' OR bs.seq > 0)
  AND bs.scheduled_at > now() + make_interval(mins => co.sales_cutoff_minutes)
  AND NOT EXISTS (SELECT 1 FROM departure_stop_visits v
                  WHERE v.departure_id = d.id AND v.seq = bs.seq AND v.departed_at IS NOT NULL)
  AND co.status = 'active'
  AND (sqlc.narg('company_id')::uuid IS NULL OR d.company_id = sqlc.narg('company_id')::uuid)
ORDER BY bs.scheduled_at;

-- name: ListUpcomingDeparturesFromAgency :many
-- Prochains départs où l'on monte à cette agence (premier arrêt ou arrêt intermédiaire).
SELECT d.id, bs.scheduled_at AS departure_at, d.arrival_eta, d.base_price, d.status,
       dc.name AS destination_city, rt.destination_stop_name AS destination_agency_name,
       (l.seat_count
          - (SELECT count(DISTINCT g.seat_cell_id) FROM booking_seat_segments g
             JOIN booking_seats x ON x.id = g.booking_seat_id
             WHERE g.departure_id = d.id AND g.segment >= bs.seq
               AND (x.status = 'sold' OR (x.status = 'held' AND x.hold_expires_at > now())))
          - (SELECT count(*) FROM departure_blocked_seats x WHERE x.departure_id = d.id))::int AS available_seats
FROM departures d
JOIN departure_stops bs ON bs.departure_id = d.id AND bs.can_board
JOIN stop_points bsp ON bsp.id = bs.stop_point_id
JOIN route_terminals rt ON rt.route_id = d.route_id
JOIN cities dc ON dc.id = rt.destination_city_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE bsp.agency_id = $1
  AND d.status IN ('scheduled', 'boarding', 'departed')
  AND bs.scheduled_at > now()
  AND bs.scheduled_at < now() + interval '3 days'
ORDER BY bs.scheduled_at
LIMIT 30;

-- name: GetDepartureSeatMap :many
-- État de chaque case du plan sur le tronçon [from_seq, to_seq) : un siège est pris
-- s'il est réservé ou vendu sur au moins un tronçon élémentaire de cet intervalle.
-- Tout le trajet : from_seq = 0, to_seq = dernier arrêt.
SELECT c.id, c.row_index, c.col_index, c.kind, c.seat_label, c.category_id, c.extra_surcharge, c.attributes,
       (CASE
            WHEN c.kind <> 'seat' THEN 'none'
            WHEN blk.seat_cell_id IS NOT NULL THEN 'blocked'
            WHEN occ.ticket_status = 'used' THEN 'boarded'
            WHEN occ.status = 'sold' THEN 'sold'
            WHEN occ.status = 'held' THEN 'held'
            ELSE 'available'
        END)::text AS state,
       -- Texte vide pour un siège libre (sqlc ne déduit pas la nullabilité d'une sous-requête jointe à gauche).
       COALESCE(occ.booking_id::text, '')::text AS booking_id,
       COALESCE(occ.passenger_name, '')::text AS passenger_name
FROM seat_layout_cells c
LEFT JOIN (
    SELECT DISTINCT ON (bs.seat_cell_id) bs.seat_cell_id, bs.status, bs.booking_id, bs.passenger_name, t.status AS ticket_status
    FROM booking_seats bs
    LEFT JOIN tickets t ON t.booking_seat_id = bs.id
    WHERE bs.departure_id = @departure_id
      AND (bs.status = 'sold' OR (bs.status = 'held' AND bs.hold_expires_at > now()))
      AND bs.from_seq < sqlc.arg(to_seq)::int AND bs.to_seq > sqlc.arg(from_seq)::int
    ORDER BY bs.seat_cell_id, (bs.status = 'sold') DESC, bs.from_seq
) occ ON occ.seat_cell_id = c.id
LEFT JOIN departure_blocked_seats blk ON blk.seat_cell_id = c.id AND blk.departure_id = @departure_id
WHERE c.layout_id = @layout_id
ORDER BY c.row_index, c.col_index;

-- name: BlockDepartureSeat :exec
INSERT INTO departure_blocked_seats (departure_id, seat_cell_id, company_id, reason, blocked_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (departure_id, seat_cell_id) DO UPDATE SET reason = EXCLUDED.reason;

-- name: UnblockDepartureSeat :exec
DELETE FROM departure_blocked_seats WHERE departure_id = $1 AND seat_cell_id = $2 AND company_id = $3;

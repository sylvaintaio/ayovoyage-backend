-- name: ShowcaseStats :one
-- Chiffres clés réels affichés sur le site public.
SELECT
    (SELECT count(DISTINCT a.city_id) FROM agencies a JOIN companies co ON co.id = a.company_id
     WHERE a.is_active AND co.status = 'active')::int AS cities,
    (SELECT count(*) FROM agencies a JOIN companies co ON co.id = a.company_id
     WHERE a.is_active AND co.status = 'active')::int AS agencies,
    (SELECT count(*) FROM routes r JOIN companies co ON co.id = r.company_id
     WHERE r.is_active AND co.status = 'active')::int AS routes,
    (SELECT count(*) FROM departures d
     WHERE d.status IN ('scheduled', 'boarding') AND d.departure_at > now()
       AND d.departure_at < now() + interval '7 days')::int AS departures_next_7_days,
    (SELECT count(*) FROM tickets t WHERE t.status = 'used')::int AS passengers_carried;

-- name: ShowcaseRoutes :many
-- Lignes actives avec tarif de départ et fréquence sur 7 jours.
SELECT r.id, r.name, r.base_price, r.duration_minutes, r.distance_km, r.included_baggage_kg,
       oc.name AS origin_city, oc.slug AS origin_slug, dc.name AS destination_city, dc.slug AS destination_slug,
       co.name AS company_name,
       (SELECT count(*) FROM departures d WHERE d.route_id = r.id AND d.status IN ('scheduled', 'boarding')
          AND d.departure_at > now() AND d.departure_at < now() + interval '7 days')::int AS departures_next_7_days
FROM routes r
JOIN route_terminals rt ON rt.route_id = r.id
JOIN cities oc ON oc.id = rt.origin_city_id
JOIN cities dc ON dc.id = rt.destination_city_id
JOIN companies co ON co.id = r.company_id
WHERE r.is_active AND co.status = 'active'
ORDER BY departures_next_7_days DESC, r.name
LIMIT 24;

-- name: ShowcaseSeatCategories :many
SELECT sc.code, sc.name, sc.description, sc.surcharge_amount, sc.color, sc.perks, sc.sort_order, co.name AS company_name
FROM seat_categories sc
JOIN companies co ON co.id = sc.company_id
WHERE co.status = 'active'
ORDER BY co.name, sc.sort_order;

-- name: ShowcaseLoyalty :many
SELECT lt.code, lt.name, lt.min_trips, lt.discount_percent, lt.perks, co.name AS company_name,
       lt.cancellation_fee_percent, lt.cancellation_cutoff_hours, lt.free_change,
       lp.points_per_100, lp.point_value, lp.signup_bonus_points
FROM loyalty_tiers lt
JOIN loyalty_programs lp ON lp.company_id = lt.company_id AND lp.is_enabled
JOIN companies co ON co.id = lt.company_id
WHERE co.status = 'active'
ORDER BY co.name, lt.min_trips;

-- name: ShowcasePolicies :many
-- Conditions d'annulation et de modification publiées par chaque compagnie.
SELECT name, cancellation_cutoff_hours, cancellation_fee_percent,
       change_enabled, change_cutoff_hours, change_fee_amount, change_max_count, change_window_days
FROM companies
WHERE status = 'active'
ORDER BY name;

-- name: ShowcaseNextDepartures :many
-- Prochains départs ouverts à la vente en ligne (tableau des départs de l'accueil).
SELECT d.id, d.departure_at, d.base_price, d.status,
       oc.name AS origin_city, oc.slug AS origin_slug, dc.name AS destination_city, dc.slug AS destination_slug,
       co.name AS company_name,
       -- Places libres sur tout le trajet (siège libre de bout en bout).
       (l.seat_count
          - (SELECT count(DISTINCT g.seat_cell_id) FROM booking_seat_segments g
             JOIN booking_seats x ON x.id = g.booking_seat_id
             WHERE g.departure_id = d.id
               AND (x.status = 'sold' OR (x.status = 'held' AND x.hold_expires_at > now())))
          - (SELECT count(*) FROM departure_blocked_seats x WHERE x.departure_id = d.id))::int AS available_seats
FROM departures d
JOIN route_terminals rt ON rt.route_id = d.route_id
JOIN cities oc ON oc.id = rt.origin_city_id
JOIN cities dc ON dc.id = rt.destination_city_id
JOIN companies co ON co.id = d.company_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE d.status IN ('scheduled', 'boarding')
  AND d.departure_at > now() + make_interval(mins => co.sales_cutoff_minutes)
  AND co.status = 'active'
ORDER BY d.departure_at
LIMIT 6;

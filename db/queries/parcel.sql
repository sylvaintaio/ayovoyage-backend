-- ============================ Tarifs ============================

-- name: ListParcelTariffs :many
SELECT * FROM parcel_tariffs WHERE company_id = $1 ORDER BY base_price;

-- name: GetParcelTariff :one
SELECT * FROM parcel_tariffs WHERE company_id = @company_id AND category = @category;

-- name: UpsertParcelTariff :one
INSERT INTO parcel_tariffs (company_id, category, label, base_price, included_kg, price_per_kg, max_weight_kg, insurance_percent)
VALUES (@company_id, @category, @label, @base_price, @included_kg, @price_per_kg, @max_weight_kg, @insurance_percent)
ON CONFLICT (company_id, category) DO UPDATE SET label = EXCLUDED.label, base_price = EXCLUDED.base_price,
    included_kg = EXCLUDED.included_kg, price_per_kg = EXCLUDED.price_per_kg,
    max_weight_kg = EXCLUDED.max_weight_kg, insurance_percent = EXCLUDED.insurance_percent
RETURNING *;

-- ============================ Colis ============================

-- name: InsertParcel :one
INSERT INTO parcels (company_id, tracking_number, sender_name, sender_phone, sender_email, sender_customer_id,
    recipient_name, recipient_phone, recipient_email, recipient_customer_id, origin_agency_id, destination_agency_id,
    current_agency_id, category, description, weight_kg, declared_value, pieces, price, currency, payer, paid_at, photo, created_by)
VALUES (@company_id, @tracking_number, @sender_name, @sender_phone, sqlc.narg(sender_email), sqlc.narg(sender_customer_id),
    @recipient_name, @recipient_phone, sqlc.narg(recipient_email), sqlc.narg(recipient_customer_id), @origin_agency_id, @destination_agency_id,
    @origin_agency_id, @category, @description, @weight_kg, @declared_value, @pieces, @price, @currency, @payer, sqlc.narg(paid_at),
    sqlc.narg(photo), sqlc.narg(created_by))
RETURNING *;

-- name: GetParcelForUpdate :one
SELECT * FROM parcels WHERE id = $1 FOR UPDATE;

-- name: GetParcelByTrackingForUpdate :one
SELECT * FROM parcels WHERE tracking_number = upper(@tracking::text) FOR UPDATE;

-- name: GetParcelView :one
-- Colis avec les noms d'agences et de villes (affichage).
SELECT p.*,
       oa.name AS origin_agency_name, oc.name AS origin_city,
       da.name AS destination_agency_name, dc.name AS destination_city, da.address AS destination_address,
       da.phone AS destination_phone,
       ca.name AS current_agency_name,
       d.departure_at AS departure_at, r.name AS departure_route_name,
       co.name AS company_name
FROM parcels p
JOIN agencies oa ON oa.id = p.origin_agency_id JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = p.destination_agency_id JOIN cities dc ON dc.id = da.city_id
LEFT JOIN agencies ca ON ca.id = p.current_agency_id
LEFT JOIN departures d ON d.id = p.departure_id
LEFT JOIN routes r ON r.id = d.route_id
JOIN companies co ON co.id = p.company_id
WHERE p.id = $1;

-- name: GetParcelIDByTracking :one
SELECT id FROM parcels WHERE tracking_number = upper(@tracking::text);

-- name: ListParcels :many
SELECT p.id, p.tracking_number, p.status, p.sender_name, p.sender_phone, p.recipient_name, p.recipient_phone,
       p.category, p.weight_kg, p.pieces, p.price, p.payer, p.paid_at, p.created_at, p.departure_id,
       oa.name AS origin_agency_name, da.name AS destination_agency_name, ca.name AS current_agency_name
FROM parcels p
JOIN agencies oa ON oa.id = p.origin_agency_id
JOIN agencies da ON da.id = p.destination_agency_id
LEFT JOIN agencies ca ON ca.id = p.current_agency_id
WHERE p.company_id = @company_id
  AND (sqlc.narg('status')::text IS NULL OR p.status = sqlc.narg('status')::text)
  AND (sqlc.narg('agency_id')::uuid IS NULL OR p.current_agency_id = sqlc.narg('agency_id')::uuid
       OR p.origin_agency_id = sqlc.narg('agency_id')::uuid OR p.destination_agency_id = sqlc.narg('agency_id')::uuid)
  AND (sqlc.narg('search')::text IS NULL OR p.tracking_number ILIKE '%' || sqlc.narg('search')::text || '%'
       OR p.sender_phone LIKE '%' || sqlc.narg('search')::text || '%' OR p.recipient_phone LIKE '%' || sqlc.narg('search')::text || '%'
       OR p.recipient_name ILIKE '%' || sqlc.narg('search')::text || '%' OR p.sender_name ILIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY p.created_at DESC
LIMIT @lim OFFSET @off;

-- name: ListCustomerParcels :many
SELECT p.id, p.tracking_number, p.status, p.sender_name, p.recipient_name, p.created_at, p.delivered_at,
       (p.sender_customer_id = @customer_id::uuid) AS is_sender,
       oc.name AS origin_city, dc.name AS destination_city, co.name AS company_name
FROM parcels p
JOIN agencies oa ON oa.id = p.origin_agency_id JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = p.destination_agency_id JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = p.company_id
WHERE p.sender_customer_id = @customer_id::uuid OR p.recipient_customer_id = @customer_id::uuid
ORDER BY p.created_at DESC, p.id DESC
LIMIT @lim OFFSET @off;

-- name: SetParcelState :one
UPDATE parcels SET status = @status, current_agency_id = sqlc.narg(current_agency_id), departure_id = sqlc.narg(departure_id)
WHERE id = @id
RETURNING *;

-- name: SetParcelPickupCode :exec
UPDATE parcels SET pickup_code_hash = @code_hash, pickup_code_sent_at = now() WHERE id = @id;

-- name: MarkParcelPaid :exec
UPDATE parcels SET paid_at = now() WHERE id = $1 AND paid_at IS NULL;

-- name: MarkParcelDelivered :one
UPDATE parcels SET status = 'delivered', delivered_at = now(), delivered_by = @delivered_by, delivered_to_name = @delivered_to_name,
    delivery_proof = @delivery_proof, delivery_identity = sqlc.narg(delivery_identity), signature = @signature,
    current_agency_id = NULL, pickup_code_hash = NULL
WHERE id = @id
RETURNING *;

-- name: ListParcelsOnDeparture :many
SELECT * FROM parcels WHERE departure_id = $1 AND status IN ('assigned', 'loaded', 'in_transit') FOR UPDATE;

-- name: ListDepartureParcels :many
-- Manifeste de chargement d'un départ.
-- is_final_leg : le car s'arrête à l'agence de destination du colis (déchargé là, sans correspondance).
SELECT p.id, p.tracking_number, p.status, p.recipient_name, p.category, p.weight_kg, p.pieces, p.description,
       da.name AS destination_agency_name,
       EXISTS (SELECT 1 FROM departure_stops ds JOIN stop_points sp ON sp.id = ds.stop_point_id
               WHERE ds.departure_id = d.id AND sp.agency_id = p.destination_agency_id)::bool AS is_final_leg
FROM parcels p
JOIN departures d ON d.id = p.departure_id
JOIN agencies da ON da.id = p.destination_agency_id
WHERE p.departure_id = $1
ORDER BY p.status, p.tracking_number;

-- name: ListParcelDepartureOptions :many
-- Départs qui s'arrêtent à l'agence où se trouve le colis (7 jours), avant d'avoir quitté cet arrêt.
-- Direct : le car s'arrête ensuite à l'agence de destination ; sinon, correspondance au terminus.
SELECT d.id, ls.scheduled_at AS departure_at, d.status, r.name AS route_name,
       CASE WHEN dst.seq IS NOT NULL THEN @destination_agency_id::uuid ELSE r.destination_agency_id END AS to_agency_id,
       COALESCE(dsp.name, rt.destination_stop_name)::text AS to_agency_name,
       (dst.seq IS NOT NULL)::bool AS is_direct,
       (SELECT count(*) FROM parcels x WHERE x.departure_id = d.id AND x.status IN ('assigned', 'loaded'))::int AS parcels_count
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN route_terminals rt ON rt.route_id = r.id
JOIN departure_stops ls ON ls.departure_id = d.id
JOIN stop_points lsp ON lsp.id = ls.stop_point_id AND lsp.agency_id = @from_agency_id
LEFT JOIN LATERAL (
    SELECT x.seq, x.stop_point_id FROM departure_stops x JOIN stop_points sp ON sp.id = x.stop_point_id
    WHERE x.departure_id = d.id AND x.seq > ls.seq AND sp.agency_id = @destination_agency_id::uuid
    ORDER BY x.seq LIMIT 1
) dst ON true
LEFT JOIN stop_points dsp ON dsp.id = dst.stop_point_id
WHERE d.company_id = @company_id
  AND d.status IN ('scheduled', 'boarding', 'departed') AND (d.status <> 'departed' OR ls.seq > 0)
  AND ls.scheduled_at > now() AND ls.scheduled_at < now() + interval '7 days'
  AND NOT EXISTS (SELECT 1 FROM departure_stop_visits v WHERE v.departure_id = d.id AND v.seq = ls.seq AND v.departed_at IS NOT NULL)
ORDER BY (dst.seq IS NOT NULL) DESC, ls.scheduled_at
LIMIT 30;

-- name: DepartureStopAtAgency :one
-- Arrêt d'un départ situé à une agence (premier passage).
SELECT ds.seq FROM departure_stops ds JOIN stop_points sp ON sp.id = ds.stop_point_id
WHERE ds.departure_id = @departure_id AND sp.agency_id = @agency_id
ORDER BY ds.seq LIMIT 1;

-- name: GetDepartureRouteAgencies :one
SELECT d.id, d.company_id, d.status, d.departure_at, r.origin_agency_id, r.destination_agency_id
FROM departures d JOIN routes r ON r.id = d.route_id
WHERE d.id = $1;

-- ============================ Historique ============================

-- name: InsertParcelEvent :exec
INSERT INTO parcel_events (company_id, parcel_id, event_type, status_after, agency_id, departure_id, staff_id, note)
VALUES (@company_id, @parcel_id, @event_type, @status_after, sqlc.narg(agency_id), sqlc.narg(departure_id), sqlc.narg(staff_id), @note);

-- name: ListParcelEvents :many
SELECT e.id, e.event_type, e.status_after, e.note, e.created_at, a.name AS agency_name, c.name AS city_name,
       s.full_name AS staff_name
FROM parcel_events e
LEFT JOIN agencies a ON a.id = e.agency_id
LEFT JOIN cities c ON c.id = a.city_id
LEFT JOIN staff_users s ON s.id = e.staff_id
WHERE e.parcel_id = $1
ORDER BY e.id;

-- name: FindCustomerByPhoneOrEmail :one
SELECT id FROM customers
WHERE status = 'active' AND (phone = @phone OR (sqlc.narg('email')::text IS NOT NULL AND lower(email) = lower(sqlc.narg('email')::text)))
LIMIT 1;

-- ============================ Bagages ============================

-- name: InsertBaggageTag :one
INSERT INTO baggage_tags (company_id, tag_number, booking_id, booking_seat_id, departure_id, weight_kg, description, extra_fee, created_by)
VALUES (@company_id, @tag_number, @booking_id, @booking_seat_id, @departure_id, @weight_kg, @description, @extra_fee, @created_by)
RETURNING *;

-- name: ListBookingBaggage :many
SELECT t.*, bs.passenger_name, bs.seat_label
FROM baggage_tags t JOIN booking_seats bs ON bs.id = t.booking_seat_id
WHERE t.booking_id = $1 AND t.status <> 'cancelled'
ORDER BY bs.seat_label, t.created_at;

-- name: ListDepartureBaggage :many
SELECT t.*, bs.passenger_name, bs.seat_label, b.reference AS booking_reference
FROM baggage_tags t
JOIN booking_seats bs ON bs.id = t.booking_seat_id
JOIN bookings b ON b.id = t.booking_id
WHERE t.departure_id = $1 AND t.status <> 'cancelled'
ORDER BY bs.seat_label, t.tag_number;

-- name: GetBaggageByTagForUpdate :one
SELECT * FROM baggage_tags WHERE tag_number = upper(@tag::text) FOR UPDATE;

-- name: GetBaggageView :one
SELECT t.*, bs.passenger_name, bs.seat_label, b.reference AS booking_reference, b.contact_phone,
       d.departure_at, r.name AS route_name, co.name AS company_name, da.name AS destination_agency_name
FROM baggage_tags t
JOIN booking_seats bs ON bs.id = t.booking_seat_id
JOIN bookings b ON b.id = t.booking_id
JOIN departures d ON d.id = t.departure_id
JOIN routes r ON r.id = d.route_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN companies co ON co.id = t.company_id
WHERE t.id = $1;

-- name: SetBaggageStatus :one
UPDATE baggage_tags SET status = @status,
    loaded_at    = CASE WHEN @status::text = 'loaded' THEN now() ELSE loaded_at END,
    unloaded_at  = CASE WHEN @status::text = 'unloaded' THEN now() ELSE unloaded_at END,
    delivered_at = CASE WHEN @status::text = 'delivered' THEN now() ELSE delivered_at END
WHERE id = @id
RETURNING *;

-- ============================ Réclamations ============================

-- name: InsertIncident :one
INSERT INTO incidents (company_id, reference, subject_type, parcel_id, baggage_id, booking_id, category, description,
    reporter_type, reporter_name, reporter_phone, reporter_customer_id, priority)
VALUES (@company_id, @reference, @subject_type, sqlc.narg(parcel_id), sqlc.narg(baggage_id), sqlc.narg(booking_id), @category, @description,
    @reporter_type, @reporter_name, @reporter_phone, sqlc.narg(reporter_customer_id), @priority)
RETURNING *;

-- name: GetIncident :one
SELECT i.*, p.tracking_number, t.tag_number, b.reference AS booking_reference, s.full_name AS assigned_name
FROM incidents i
LEFT JOIN parcels p ON p.id = i.parcel_id
LEFT JOIN baggage_tags t ON t.id = i.baggage_id
LEFT JOIN bookings b ON b.id = coalesce(i.booking_id, t.booking_id)
LEFT JOIN staff_users s ON s.id = i.assigned_to
WHERE i.id = $1;

-- name: GetIncidentIDByReference :one
SELECT id FROM incidents WHERE reference = upper(@reference::text);

-- name: ListIncidents :many
SELECT i.id, i.reference, i.subject_type, i.category, i.priority, i.status, i.reporter_name, i.created_at, i.updated_at,
       p.tracking_number, t.tag_number, b.reference AS booking_reference, s.full_name AS assigned_name
FROM incidents i
LEFT JOIN parcels p ON p.id = i.parcel_id
LEFT JOIN baggage_tags t ON t.id = i.baggage_id
LEFT JOIN bookings b ON b.id = coalesce(i.booking_id, t.booking_id)
LEFT JOIN staff_users s ON s.id = i.assigned_to
WHERE i.company_id = @company_id
  AND (sqlc.narg('status')::text IS NULL OR i.status = sqlc.narg('status')::text)
  AND (sqlc.narg('subject')::text IS NULL OR i.subject_type = sqlc.narg('subject')::text)
ORDER BY CASE i.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END, i.created_at DESC
LIMIT @lim OFFSET @off;

-- name: ListCustomerIncidents :many
SELECT i.id, i.reference, i.subject_type, i.category, i.status, i.created_at, i.updated_at, p.tracking_number, b.reference AS booking_reference
FROM incidents i
LEFT JOIN parcels p ON p.id = i.parcel_id
LEFT JOIN baggage_tags t ON t.id = i.baggage_id
LEFT JOIN bookings b ON b.id = coalesce(i.booking_id, t.booking_id)
WHERE i.reporter_customer_id = @customer_id
ORDER BY i.created_at DESC, i.id DESC
LIMIT @lim OFFSET @off;

-- name: UpdateIncident :one
UPDATE incidents SET status = @status, priority = @priority, assigned_to = sqlc.narg(assigned_to), resolution = @resolution,
    resolved_at = CASE WHEN @status::text IN ('resolved', 'rejected') THEN coalesce(resolved_at, now()) ELSE NULL END
WHERE id = @id
RETURNING *;

-- name: InsertIncidentEvent :exec
INSERT INTO incident_events (company_id, incident_id, author_type, author_id, author_name, action, note, is_public)
VALUES (@company_id, @incident_id, @author_type, sqlc.narg(author_id), @author_name, @action, @note, @is_public);

-- name: ListIncidentEvents :many
SELECT * FROM incident_events WHERE incident_id = @incident_id AND (@include_internal::bool OR is_public) ORDER BY id;

-- name: InsertAttachment :one
INSERT INTO attachments (company_id, incident_id, content_type, data) VALUES ($1, $2, $3, $4) RETURNING id;

-- name: ListIncidentAttachmentIDs :many
SELECT id, content_type FROM attachments WHERE incident_id = $1 ORDER BY created_at;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE id = $1;

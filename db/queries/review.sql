-- name: ListReviewCandidates :many
-- Réservations à inviter : départ arrivé depuis au moins 2 h (moins de 14 jours),
-- au moins un passager embarqué, pas encore d'invitation.
SELECT b.id, b.company_id, b.contact_name, b.contact_phone, b.contact_email, b.customer_id,
       oc.name AS origin_city, dc.name AS destination_city, d.departure_at, co.name AS company_name
FROM bookings b
JOIN departures d ON d.id = b.departure_id
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = b.company_id
WHERE b.status = 'confirmed'
  AND d.status = 'arrived' AND d.updated_at <= now() - interval '2 hours'
  AND d.departure_at > now() - interval '14 days'
  AND EXISTS (SELECT 1 FROM tickets t WHERE t.booking_id = b.id AND t.status = 'used')
  AND NOT EXISTS (SELECT 1 FROM review_invitations ri WHERE ri.booking_id = b.id)
ORDER BY d.departure_at
LIMIT 300;

-- name: InsertReviewInvitation :one
INSERT INTO review_invitations (company_id, booking_id, token_hash, expires_at)
VALUES (@company_id, @booking_id, @token_hash, @expires_at)
ON CONFLICT (booking_id) DO NOTHING
RETURNING *;

-- name: GetReviewInvitation :one
SELECT ri.*, b.reference, b.contact_name, b.customer_id, b.departure_id, d.route_id, r.origin_agency_id,
       d.departure_at, oc.name AS origin_city, dc.name AS destination_city, co.name AS company_name,
       EXISTS (SELECT 1 FROM reviews rv WHERE rv.booking_id = b.id) AS reviewed
FROM review_invitations ri
JOIN bookings b ON b.id = ri.booking_id
JOIN departures d ON d.id = b.departure_id
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = b.company_id
WHERE ri.token_hash = $1;

-- name: MarkReviewInvitationUsed :exec
UPDATE review_invitations SET used_at = now() WHERE id = $1;

-- name: InsertReview :one
INSERT INTO reviews (company_id, booking_id, departure_id, route_id, agency_id, customer_id, rating, punctuality,
                     comfort, staff, cleanliness, comment, display_name, city, publish_consent)
VALUES (@company_id, @booking_id, @departure_id, @route_id, @agency_id, sqlc.narg(customer_id), @rating,
        sqlc.narg(punctuality), sqlc.narg(comfort), sqlc.narg(staff), sqlc.narg(cleanliness), @comment,
        @display_name, @city, @publish_consent)
RETURNING *;

-- name: SetReviewIncident :exec
UPDATE reviews SET incident_id = @incident_id WHERE id = @id;

-- name: GetReview :one
SELECT * FROM reviews WHERE id = $1;

-- name: ListReviews :many
SELECT rv.*, b.reference, r.name AS route_name, d.departure_at, i.reference AS incident_reference
FROM reviews rv
JOIN bookings b ON b.id = rv.booking_id
JOIN routes r ON r.id = rv.route_id
JOIN departures d ON d.id = rv.departure_id
LEFT JOIN incidents i ON i.id = rv.incident_id
WHERE rv.company_id = @company_id
  AND (sqlc.narg('status')::text IS NULL OR rv.status = sqlc.narg('status')::text)
  AND (sqlc.narg('max_rating')::int IS NULL OR rv.rating <= sqlc.narg('max_rating')::int)
ORDER BY rv.created_at DESC
LIMIT @lim OFFSET @off;

-- name: ReviewStats :one
SELECT count(*)::int AS total,
       coalesce(avg(rating), 0)::float8 AS average,
       count(*) FILTER (WHERE rating >= 4)::int AS positive,
       count(*) FILTER (WHERE rating <= 2)::int AS negative,
       count(*) FILTER (WHERE status = 'pending')::int AS to_moderate,
       count(*) FILTER (WHERE reply <> '')::int AS replied,
       coalesce(avg(punctuality), 0)::float8 AS punctuality,
       coalesce(avg(comfort), 0)::float8 AS comfort,
       coalesce(avg(staff), 0)::float8 AS staff,
       coalesce(avg(cleanliness), 0)::float8 AS cleanliness
FROM reviews
WHERE company_id = @company_id AND created_at >= @since;

-- name: ModerateReview :one
UPDATE reviews SET
    status = @status, hidden_reason = @hidden_reason, moderated_by = @staff_id, moderated_at = now(),
    reply = @reply,
    replied_by = CASE WHEN @reply::text <> '' AND @reply::text <> reply THEN @staff_id ELSE replied_by END,
    replied_at = CASE WHEN @reply::text <> '' AND @reply::text <> reply THEN now() ELSE replied_at END
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: ListPublishedReviews :many
-- Avis publiés pour le site : les mieux notés d'abord, puis les plus récents.
SELECT rv.id, rv.rating, rv.comment, rv.display_name, rv.city, rv.reply, rv.created_at,
       oc.name AS origin_city, dc.name AS destination_city, co.name AS company_name
FROM reviews rv
JOIN routes r ON r.id = rv.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = rv.company_id
WHERE rv.status = 'published' AND co.status = 'active'
  AND (sqlc.narg('route_id')::uuid IS NULL OR rv.route_id = sqlc.narg('route_id')::uuid)
  AND (sqlc.narg('agency_id')::uuid IS NULL OR rv.agency_id = sqlc.narg('agency_id')::uuid)
  AND rv.comment <> ''
ORDER BY rv.rating DESC, rv.created_at DESC
LIMIT @lim;

-- name: PublishedReviewSummary :one
SELECT count(*)::int AS total, coalesce(avg(rating), 0)::float8 AS average
FROM reviews rv
JOIN companies co ON co.id = rv.company_id
WHERE rv.status = 'published' AND co.status = 'active'
  AND (sqlc.narg('route_id')::uuid IS NULL OR rv.route_id = sqlc.narg('route_id')::uuid)
  AND (sqlc.narg('agency_id')::uuid IS NULL OR rv.agency_id = sqlc.narg('agency_id')::uuid);

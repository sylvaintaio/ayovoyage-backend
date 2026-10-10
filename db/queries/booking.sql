-- name: LockDepartureForBooking :one
-- Sérialise les réservations d'un même départ (verrou de ligne court).
SELECT d.*, co.booking_hold_minutes, co.sales_cutoff_minutes, co.currency
FROM departures d
JOIN companies co ON co.id = d.company_id
WHERE d.id = $1
FOR UPDATE OF d;

-- name: ExpireStaleSeatHolds :exec
UPDATE booking_seats SET status = 'expired'
WHERE departure_id = $1 AND status = 'held' AND hold_expires_at <= now();

-- name: InsertBooking :one
INSERT INTO bookings (company_id, reference, departure_id, customer_id, channel, agency_id, created_by_staff_id,
                      contact_name, contact_phone, contact_email, subtotal_amount, discount_amount,
                      loyalty_points_used, loyalty_points_amount, total_amount, currency, expires_at,
                      billing_name, billing_tax_id, promo_code_id, promo_code, promo_amount, early_bird_amount, referral_amount)
VALUES (@company_id, @reference, @departure_id, @customer_id, @channel, @agency_id, @created_by_staff_id,
        @contact_name, @contact_phone, @contact_email, @subtotal_amount, @discount_amount,
        @loyalty_points_used, @loyalty_points_amount, @total_amount, @currency, @expires_at,
        sqlc.narg(billing_name), sqlc.narg(billing_tax_id), sqlc.narg(promo_code_id), sqlc.narg(promo_code),
        @promo_amount, @early_bird_amount, @referral_amount)
RETURNING *;

-- name: InsertBookingSeat :one
INSERT INTO booking_seats (company_id, booking_id, departure_id, seat_cell_id, seat_label, category_id,
                           passenger_name, passenger_phone, base_price, category_surcharge, seat_surcharge,
                           discount_amount, total_price, hold_expires_at, early_bird_amount)
VALUES (@company_id, @booking_id, @departure_id, @seat_cell_id, @seat_label, @category_id,
        @passenger_name, @passenger_phone, @base_price, @category_surcharge, @seat_surcharge,
        @discount_amount, @total_price, @hold_expires_at, @early_bird_amount)
RETURNING *;

-- name: GetBooking :one
SELECT * FROM bookings WHERE id = $1;

-- name: GetBookingByReference :one
SELECT * FROM bookings WHERE reference = $1;

-- name: GetBookingForUpdate :one
SELECT * FROM bookings WHERE id = $1 FOR UPDATE;

-- name: ListBookingSeats :many
SELECT bs.*, sc.name AS category_name, sc.code AS category_code
FROM booking_seats bs
JOIN seat_categories sc ON sc.id = bs.category_id
WHERE bs.booking_id = $1
ORDER BY bs.seat_label;

-- name: ListCustomerBookings :many
SELECT b.*, d.departure_at, r.name AS route_name, co.name AS company_name
FROM bookings b
JOIN departures d ON d.id = b.departure_id
JOIN routes r ON r.id = d.route_id
JOIN companies co ON co.id = b.company_id
WHERE b.customer_id = @customer_id
  AND (sqlc.narg('status')::text IS NULL OR b.status = sqlc.narg('status')::text)
ORDER BY b.created_at DESC
LIMIT @lim OFFSET @off;

-- name: ListCompanyBookings :many
SELECT b.*, d.departure_at, r.name AS route_name
FROM bookings b
JOIN departures d ON d.id = b.departure_id
JOIN routes r ON r.id = d.route_id
WHERE b.company_id = @company_id
  AND (sqlc.narg('departure_id')::uuid IS NULL OR b.departure_id = sqlc.narg('departure_id')::uuid)
  AND (sqlc.narg('status')::text IS NULL OR b.status = sqlc.narg('status')::text)
  AND (sqlc.narg('agency_id')::uuid IS NULL OR b.agency_id = sqlc.narg('agency_id')::uuid)
  AND (sqlc.narg('payment_mode')::text IS NULL OR b.payment_mode = sqlc.narg('payment_mode')::text)
  AND (sqlc.narg('q')::text IS NULL OR b.reference = upper(sqlc.narg('q')::text) OR b.contact_phone = sqlc.narg('q')::text)
ORDER BY b.created_at DESC
LIMIT @lim OFFSET @off;

-- name: ConfirmBooking :one
UPDATE bookings SET status = 'confirmed', confirmed_at = now() WHERE id = $1 RETURNING *;

-- name: SellBookingSeats :many
-- Passe les sièges en "vendu". Si un siège expiré a été repris entre-temps,
-- l'index unique lève une violation : le paiement tardif part en remboursement.
UPDATE booking_seats SET status = 'sold'
WHERE booking_id = $1 AND status IN ('held', 'expired')
RETURNING *;

-- name: SetBookingStatus :one
UPDATE bookings SET status = @status,
                    cancelled_at = CASE WHEN @status::text IN ('cancelled', 'expired') THEN now() ELSE cancelled_at END,
                    cancel_reason = @cancel_reason
WHERE id = @id
RETURNING *;

-- name: ReleaseBookingSeats :exec
UPDATE booking_seats SET status = @status WHERE booking_id = @booking_id AND status IN ('held', 'sold');

-- name: ExpirePendingBookings :many
-- Exécuté périodiquement : réservations non payées dont le délai est dépassé.
UPDATE bookings SET status = 'expired', cancelled_at = now(), cancel_reason = 'Délai de paiement dépassé'
WHERE id IN (
    SELECT id FROM bookings
    WHERE status = 'pending' AND expires_at <= now()
    ORDER BY expires_at
    LIMIT 500
    FOR UPDATE SKIP LOCKED
)
RETURNING id, company_id, customer_id, loyalty_points_used, payment_mode, contact_phone;

-- name: ExpireSeatsOfBookings :exec
UPDATE booking_seats SET status = 'expired' WHERE booking_id = ANY (@booking_ids::uuid[]) AND status = 'held';

-- ============================ Billets ============================

-- name: InsertTicket :one
INSERT INTO tickets (id, company_id, booking_id, booking_seat_id, departure_id, ticket_number, qr_payload)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListBookingTickets :many
SELECT t.*, bs.seat_label, bs.passenger_name
FROM tickets t
JOIN booking_seats bs ON bs.id = t.booking_seat_id
WHERE t.booking_id = $1
ORDER BY bs.seat_label;

-- name: CancelBookingTickets :exec
UPDATE tickets SET status = 'cancelled' WHERE booking_id = $1 AND status = 'valid';

-- name: ListCustomerTickets :many
SELECT t.*, bs.seat_label, bs.passenger_name, d.departure_at, r.name AS route_name, co.name AS company_name
FROM tickets t
JOIN bookings b ON b.id = t.booking_id
JOIN booking_seats bs ON bs.id = t.booking_seat_id
JOIN departures d ON d.id = t.departure_id
JOIN routes r ON r.id = d.route_id
JOIN companies co ON co.id = t.company_id
WHERE b.customer_id = @customer_id
  AND (NOT @upcoming_only::boolean OR d.departure_at > now() - interval '6 hours')
ORDER BY d.departure_at DESC
LIMIT @lim OFFSET @off;

-- name: GetTicketDetail :one
SELECT t.*, bs.seat_label, bs.passenger_name, bs.passenger_phone, bs.total_price,
       sc.name AS category_name, b.reference AS booking_reference, b.customer_id,
       d.departure_at, d.arrival_eta, r.name AS route_name, r.included_baggage_kg,
       oa.name AS origin_agency_name, oa.address AS origin_agency_address, oc.name AS origin_city,
       da.name AS destination_agency_name, dc.name AS destination_city,
       co.name AS company_name, co.phone AS company_phone, co.currency, bu.plate_number
FROM tickets t
JOIN booking_seats bs ON bs.id = t.booking_seat_id
JOIN seat_categories sc ON sc.id = bs.category_id
JOIN bookings b ON b.id = t.booking_id
JOIN departures d ON d.id = t.departure_id
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
JOIN companies co ON co.id = t.company_id
JOIN buses bu ON bu.id = d.bus_id
WHERE t.id = $1;

-- name: GetTicketForUpdate :one
SELECT t.*, bs.seat_label, bs.passenger_name
FROM tickets t
JOIN booking_seats bs ON bs.id = t.booking_seat_id
WHERE t.id = $1
FOR UPDATE OF t;

-- name: MarkTicketUsed :exec
UPDATE tickets SET status = 'used', boarded_at = $2, boarded_by = $3 WHERE id = $1;

-- ============================ Embarquement ============================

-- name: InsertBoardingScan :one
INSERT INTO boarding_scans (company_id, departure_id, ticket_id, staff_id, device_id, result, offline, scanned_at, client_scan_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (device_id, client_scan_id) WHERE client_scan_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: GetDepartureManifest :many
SELECT bs.seat_label, bs.passenger_name, bs.passenger_phone, b.reference AS booking_reference,
       t.id AS ticket_id, t.ticket_number, t.status AS ticket_status, t.boarded_at, sc.code AS category_code
FROM booking_seats bs
JOIN bookings b ON b.id = bs.booking_id
JOIN tickets t ON t.booking_seat_id = bs.id
JOIN seat_categories sc ON sc.id = bs.category_id
WHERE bs.departure_id = $1 AND bs.status = 'sold'
ORDER BY bs.seat_label;

-- name: ListActiveBookingIDsForDeparture :many
SELECT id FROM bookings WHERE departure_id = $1 AND status IN ('pending', 'confirmed');

-- name: CountUsedTicketsForBooking :one
SELECT count(*)::int FROM tickets WHERE booking_id = $1 AND status = 'used';

-- name: GetBoardingScanByClientID :one
SELECT * FROM boarding_scans WHERE device_id = $1 AND client_scan_id = $2;

-- ============================ Modification de billet ============================

-- name: GetDepartureCities :one
SELECT d.id, d.company_id, d.departure_at, oc.id AS origin_city_id, oc.name AS origin_city,
       dc.id AS destination_city_id, dc.name AS destination_city
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN agencies oa ON oa.id = r.origin_agency_id
JOIN cities oc ON oc.id = oa.city_id
JOIN agencies da ON da.id = r.destination_agency_id
JOIN cities dc ON dc.id = da.city_id
WHERE d.id = $1;

-- name: ListChangeCandidates :many
-- Départs de remplacement : même compagnie, mêmes villes, ouverts à la vente,
-- avec assez de places pour tous les passagers.
SELECT * FROM (
    SELECT d.id, d.departure_at, d.arrival_eta, d.base_price,
           oa.name AS origin_agency_name, da.name AS destination_agency_name,
           b.model AS bus_model,
           (l.seat_count
              - (SELECT count(*) FROM booking_seats bs
                 WHERE bs.departure_id = d.id
                   AND (bs.status = 'sold' OR (bs.status = 'held' AND bs.hold_expires_at > now())))
              - (SELECT count(*) FROM departure_blocked_seats x WHERE x.departure_id = d.id))::int AS available_seats
    FROM departures d
    JOIN routes r ON r.id = d.route_id
    JOIN agencies oa ON oa.id = r.origin_agency_id
    JOIN agencies da ON da.id = r.destination_agency_id
    JOIN companies co ON co.id = d.company_id
    JOIN buses b ON b.id = d.bus_id
    JOIN seat_layouts l ON l.id = d.layout_id
    WHERE d.company_id = @company_id
      AND oa.city_id = @origin_city_id AND da.city_id = @destination_city_id
      AND d.id <> @exclude_departure_id
      AND d.departure_at >= @from_ts AND d.departure_at < @to_ts
      AND d.status IN ('scheduled', 'boarding')
      AND d.departure_at > now() + make_interval(mins => co.sales_cutoff_minutes)
) c
WHERE c.available_seats >= @passengers::int
ORDER BY c.departure_at
LIMIT 50;

-- name: ExpirePendingChanges :many
-- Abandonne une modification non payée avant d'en commencer une autre.
UPDATE bookings SET status = 'expired', cancelled_at = now(), cancel_reason = 'Modification remplacée par une nouvelle demande'
WHERE changed_from_booking_id = @booking_id AND status = 'pending'
RETURNING id;

-- name: SetBookingChange :one
UPDATE bookings SET
    changed_from_booking_id = @changed_from_booking_id,
    change_count            = @change_count,
    change_fee_amount       = @change_fee_amount,
    change_credit_amount    = @change_credit_amount,
    total_amount            = @total_amount
WHERE id = @id
RETURNING *;

-- name: TransferLoyaltyPoints :exec
-- Les points utilisés sur la réservation d'origine suivent la nouvelle (restitués si elle est annulée).
UPDATE bookings SET loyalty_points_used = @points_used, loyalty_points_amount = @points_amount WHERE id = @id;

-- name: CountSucceededPaymentsForBooking :one
SELECT count(*)::int FROM payments WHERE booking_id = $1 AND status = 'succeeded';

-- name: GetReplacementBooking :one
-- Réservation qui a remplacé celle-ci après une modification.
SELECT id, reference FROM bookings
WHERE changed_from_booking_id = $1 AND status IN ('confirmed', 'changed', 'cancelled', 'refunded')
ORDER BY created_at DESC LIMIT 1;

-- ============================ Paiement en agence ============================

-- name: SetBookingPayLater :one
-- Passe une réservation en attente au paiement en agence : places bloquées jusqu'à pay_by.
UPDATE bookings SET payment_mode = 'agency', pay_by = @pay_by, expires_at = @pay_by
WHERE id = @id AND status = 'pending' AND payment_mode = 'online'
RETURNING *;

-- name: ExtendBookingSeatHolds :exec
UPDATE booking_seats SET hold_expires_at = @until WHERE booking_id = @booking_id AND status = 'held';

-- name: CountActivePayLater :one
SELECT count(*)::int FROM bookings
WHERE company_id = @company_id AND contact_phone = @phone
  AND status = 'pending' AND payment_mode = 'agency' AND expires_at > now();

-- name: CountPayLaterSeatsOnDeparture :one
SELECT count(*)::int FROM booking_seats bs
JOIN bookings b ON b.id = bs.booking_id
WHERE bs.departure_id = @departure_id AND bs.status = 'held' AND bs.hold_expires_at > now()
  AND b.payment_mode = 'agency';

-- name: CountExpiredPayLaterSince :one
SELECT count(*)::int FROM bookings
WHERE company_id = @company_id AND contact_phone = @phone
  AND payment_mode = 'agency' AND status = 'expired' AND cancelled_at >= @since;

-- name: GetActivePayLaterBlock :one
SELECT * FROM pay_later_blocks
WHERE company_id = @company_id AND phone = @phone AND lifted_at IS NULL AND blocked_until > now()
ORDER BY blocked_until DESC
LIMIT 1;

-- name: InsertPayLaterBlock :one
INSERT INTO pay_later_blocks (company_id, phone, blocked_until, reason)
VALUES (@company_id, @phone, @blocked_until, @reason)
RETURNING *;

-- name: LiftPayLaterBlocks :many
UPDATE pay_later_blocks SET lifted_at = now(), lifted_by = @staff_id
WHERE company_id = @company_id AND phone = @phone AND lifted_at IS NULL AND blocked_until > now()
RETURNING *;

-- name: ListPayLaterReminders :many
-- Réservations « payer en agence » proches de leur échéance, sans rappel envoyé :
-- moins de 3 h, ou moins d'un quart du délai accordé.
SELECT id FROM bookings
WHERE status = 'pending' AND payment_mode = 'agency' AND pay_reminder_sent_at IS NULL
  AND pay_by > now()
  AND pay_by - now() < GREATEST(interval '3 hours', (pay_by - created_at) / 4)
ORDER BY pay_by
LIMIT 200;

-- name: MarkPayLaterReminded :one
UPDATE bookings SET pay_reminder_sent_at = now()
WHERE id = @id AND pay_reminder_sent_at IS NULL AND status = 'pending'
RETURNING *;

-- name: ListPayLaterAgencies :many
-- Agences où régler une réservation : agences actives de la compagnie qui vendent des billets.
SELECT a.id, a.name, a.address, c.name AS city
FROM agencies a
JOIN cities c ON c.id = a.city_id
WHERE a.company_id = @company_id AND a.is_active AND 'ticketing' = ANY (a.services)
ORDER BY c.name, a.name;

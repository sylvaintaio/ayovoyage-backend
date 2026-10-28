-- name: InsertPayment :one
INSERT INTO payments (company_id, booking_id, parcel_id, provider, method, amount, currency, payer_phone,
                      idempotency_key, agency_id, received_by_staff_id, provider_reference)
VALUES (@company_id, sqlc.narg(booking_id), sqlc.narg(parcel_id), @provider, @method, @amount, @currency, @payer_phone,
        @idempotency_key, @agency_id, @received_by_staff_id, sqlc.narg(provider_reference))
RETURNING *;

-- name: GetPaymentByIdempotencyKey :one
SELECT * FROM payments WHERE idempotency_key = $1;

-- name: GetPaymentForUpdate :one
SELECT * FROM payments WHERE id = $1 FOR UPDATE;

-- name: GetPaymentByProviderRefForUpdate :one
SELECT * FROM payments WHERE provider = $1 AND provider_reference = $2 FOR UPDATE;

-- name: SetPaymentCheckout :one
UPDATE payments SET provider_reference = $2, checkout_url = $3 WHERE id = $1 RETURNING *;

-- name: MarkPaymentSucceeded :one
UPDATE payments SET status = 'succeeded', succeeded_at = now() WHERE id = $1 RETURNING *;

-- name: MarkPaymentFailed :one
UPDATE payments SET status = $2, failure_reason = $3 WHERE id = $1 RETURNING *;

-- name: ListBookingPayments :many
SELECT * FROM payments WHERE booking_id = $1 ORDER BY created_at;

-- name: GetSucceededPaymentForBooking :one
SELECT * FROM payments WHERE booking_id = $1 AND status = 'succeeded' ORDER BY succeeded_at DESC LIMIT 1;

-- name: InsertPaymentEvent :one
INSERT INTO payment_events (provider, event_id, payload)
VALUES ($1, $2, $3)
ON CONFLICT (provider, event_id) DO NOTHING
RETURNING id;

-- name: MarkPaymentEventProcessed :exec
UPDATE payment_events SET processed_at = now() WHERE id = $1;

-- ============================ Remboursements ============================

-- name: InsertRefund :one
INSERT INTO refunds (company_id, booking_id, payment_id, amount, reason, requested_by_type, requested_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetRefundForUpdate :one
SELECT * FROM refunds WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: DecideRefund :one
UPDATE refunds SET status = $2, approved_by = $3, decided_at = now() WHERE id = $1 RETURNING *;

-- name: MarkRefundProcessed :one
UPDATE refunds SET status = 'processed', processed_at = now(),
    process_method = @process_method, processed_agency_id = sqlc.narg(processed_agency_id), processed_by = @processed_by
WHERE id = @id RETURNING *;

-- name: ListRefunds :many
SELECT rf.*, b.reference AS booking_reference
FROM refunds rf
JOIN bookings b ON b.id = rf.booking_id
WHERE rf.company_id = @company_id
  AND (sqlc.narg('status')::text IS NULL OR rf.status = sqlc.narg('status')::text)
ORDER BY rf.created_at DESC
LIMIT @lim OFFSET @off;

-- ============================ Tableau de bord ============================

-- name: DashboardTotals :one
SELECT
    coalesce((SELECT sum(p.amount) FROM payments p
              WHERE p.company_id = @company_id AND p.status = 'succeeded'
                AND p.succeeded_at >= @from_ts::timestamptz AND p.succeeded_at < @to_ts::timestamptz), 0)::bigint AS revenue,
    (SELECT count(*) FROM booking_seats bs JOIN bookings b ON b.id = bs.booking_id
     WHERE b.company_id = @company_id AND bs.status = 'sold'
       AND b.confirmed_at >= @from_ts::timestamptz AND b.confirmed_at < @to_ts::timestamptz)::int AS tickets_sold,
    (SELECT count(*) FROM bookings b
     WHERE b.company_id = @company_id AND b.status IN ('cancelled', 'refunded')
       AND b.cancelled_at >= @from_ts::timestamptz AND b.cancelled_at < @to_ts::timestamptz)::int AS cancellations,
    coalesce((SELECT sum(rf.amount) FROM refunds rf
              WHERE rf.company_id = @company_id AND rf.status IN ('approved', 'processed')
                AND rf.decided_at >= @from_ts::timestamptz AND rf.decided_at < @to_ts::timestamptz), 0)::bigint AS refunds_amount,
    (SELECT count(*) FROM refunds rf
     WHERE rf.company_id = @company_id AND rf.status = 'requested')::int AS pending_refunds;

-- name: DashboardRevenueByAgency :many
SELECT a.id AS agency_id, a.name AS agency_name,
       count(DISTINCT b.id)::int AS bookings,
       coalesce(sum(b.total_amount), 0)::bigint AS revenue
FROM bookings b
JOIN departures d ON d.id = b.departure_id
JOIN routes r ON r.id = d.route_id
JOIN departure_stops bst ON bst.departure_id = d.id AND bst.seq = b.boarding_seq
JOIN stop_points bsp ON bsp.id = bst.stop_point_id
JOIN agencies a ON a.id = coalesce(b.agency_id, bsp.agency_id, r.origin_agency_id)
WHERE b.company_id = @company_id AND b.status IN ('confirmed', 'changed')
  AND b.confirmed_at >= @from_ts::timestamptz AND b.confirmed_at < @to_ts::timestamptz
GROUP BY a.id, a.name
ORDER BY revenue DESC;

-- name: DashboardFillRates :many
SELECT d.id AS departure_id, d.departure_at, r.name AS route_name, l.seat_count,
       (SELECT count(*) FROM booking_seats bs WHERE bs.departure_id = d.id AND bs.status = 'sold')::int AS sold_seats
FROM departures d
JOIN routes r ON r.id = d.route_id
JOIN seat_layouts l ON l.id = d.layout_id
WHERE d.company_id = @company_id AND d.status <> 'cancelled'
  AND d.departure_at >= @from_ts::timestamptz AND d.departure_at < @to_ts::timestamptz
ORDER BY d.departure_at;

-- name: DashboardCategoryUsage :many
SELECT sc.code, sc.name, count(bs.id)::int AS seats_sold, coalesce(sum(bs.total_price), 0)::bigint AS revenue
FROM seat_categories sc
LEFT JOIN booking_seats bs ON bs.category_id = sc.id AND bs.status = 'sold'
     AND bs.created_at >= @from_ts::timestamptz AND bs.created_at < @to_ts::timestamptz
WHERE sc.company_id = @company_id
GROUP BY sc.id, sc.code, sc.name, sc.sort_order
ORDER BY sc.sort_order;

-- ============================ Caisse ============================

-- name: CashTotals :one
-- Espèces d'une agence sur une journée : encaissements et remboursements versés.
SELECT
    coalesce((SELECT sum(p.amount) FROM payments p
              WHERE p.company_id = @company_id AND p.agency_id = @agency_id AND p.provider = 'cash' AND p.method = 'cash'
                AND p.status IN ('succeeded', 'refunded')
                AND p.succeeded_at >= @from_ts::timestamptz AND p.succeeded_at < @to_ts::timestamptz), 0)::bigint AS cash_in,
    (SELECT count(*) FROM payments p
     WHERE p.company_id = @company_id AND p.agency_id = @agency_id AND p.provider = 'cash' AND p.method = 'cash'
       AND p.status IN ('succeeded', 'refunded')
       AND p.succeeded_at >= @from_ts::timestamptz AND p.succeeded_at < @to_ts::timestamptz)::int AS sales_count,
    coalesce((SELECT sum(r.amount) FROM refunds r
              WHERE r.company_id = @company_id AND r.processed_agency_id = @agency_id AND r.process_method = 'cash'
                AND r.status = 'processed'
                AND r.processed_at >= @from_ts::timestamptz AND r.processed_at < @to_ts::timestamptz), 0)::bigint AS cash_out,
    (SELECT count(*) FROM refunds r
     WHERE r.company_id = @company_id AND r.processed_agency_id = @agency_id AND r.process_method = 'cash'
       AND r.status = 'processed'
       AND r.processed_at >= @from_ts::timestamptz AND r.processed_at < @to_ts::timestamptz)::int AS refunds_count,
    -- Caisses de bord remises à l'agence par les contrôleurs (montant compté).
    coalesce((SELECT sum(h.counted_cash) FROM onboard_cash_handovers h
              WHERE h.company_id = @company_id AND h.agency_id = @agency_id
                AND h.received_at >= @from_ts::timestamptz AND h.received_at < @to_ts::timestamptz), 0)::bigint AS onboard_in,
    (SELECT count(*) FROM onboard_cash_handovers h
     WHERE h.company_id = @company_id AND h.agency_id = @agency_id
       AND h.received_at >= @from_ts::timestamptz AND h.received_at < @to_ts::timestamptz)::int AS handovers_count;

-- name: CashByCashier :many
SELECT s.id AS staff_id, s.full_name, count(*)::int AS sales_count, sum(p.amount)::bigint AS amount
FROM payments p
JOIN staff_users s ON s.id = p.received_by_staff_id
WHERE p.company_id = @company_id AND p.agency_id = @agency_id AND p.provider = 'cash' AND p.method = 'cash'
  AND p.status IN ('succeeded', 'refunded')
  AND p.succeeded_at >= @from_ts::timestamptz AND p.succeeded_at < @to_ts::timestamptz
GROUP BY s.id, s.full_name
ORDER BY amount DESC;

-- name: GetCashClosure :one
SELECT c.*, s.full_name AS closed_by_name
FROM cash_closures c JOIN staff_users s ON s.id = c.closed_by
WHERE c.company_id = @company_id AND c.agency_id = @agency_id AND c.business_date = @business_date;

-- name: InsertCashClosure :one
INSERT INTO cash_closures (company_id, agency_id, business_date, cash_in, cash_out, expected_cash, counted_cash, sales_count, note, closed_by)
VALUES (@company_id, @agency_id, @business_date, @cash_in, @cash_out, @expected_cash, @counted_cash, @sales_count, @note, @closed_by)
RETURNING *;

-- name: ListCashClosures :many
SELECT c.*, s.full_name AS closed_by_name, a.name AS agency_name
FROM cash_closures c
JOIN staff_users s ON s.id = c.closed_by
JOIN agencies a ON a.id = c.agency_id
WHERE c.company_id = @company_id
  AND (sqlc.narg('agency_id')::uuid IS NULL OR c.agency_id = sqlc.narg('agency_id')::uuid)
ORDER BY c.business_date DESC, a.name, c.id
LIMIT @lim OFFSET @off;

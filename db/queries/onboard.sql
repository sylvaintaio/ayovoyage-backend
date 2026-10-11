-- ============================ Vente à bord ============================

-- name: GetBookingByClientSale :one
SELECT * FROM bookings WHERE created_by_staff_id = @staff_id AND client_sale_id = @client_sale_id;

-- name: SetBookingClientSale :exec
UPDATE bookings SET client_sale_id = @client_sale_id, sold_offline = @sold_offline WHERE id = @id;

-- name: ListOnboardSeats :many
-- Places du quota du contrôleur, dans l'ordre du plan.
SELECT c.id, c.seat_label, c.category_id, c.extra_surcharge, c.row_index, c.col_index
FROM departure_blocked_seats blk
JOIN seat_layout_cells c ON c.id = blk.seat_cell_id
WHERE blk.departure_id = @departure_id AND blk.kind = 'onboard'
ORDER BY c.row_index, c.col_index;

-- name: BlockOnboardSeat :exec
INSERT INTO departure_blocked_seats (departure_id, seat_cell_id, company_id, reason, blocked_by, kind)
VALUES (@departure_id, @seat_cell_id, @company_id, 'Place réservée à la vente à bord', @blocked_by, 'onboard')
ON CONFLICT (departure_id, seat_cell_id) DO NOTHING;

-- name: ReleaseOnboardSeat :exec
DELETE FROM departure_blocked_seats
WHERE departure_id = @departure_id AND seat_cell_id = @seat_cell_id AND kind = 'onboard';

-- name: SeatHasActiveBooking :one
SELECT EXISTS (
    SELECT 1 FROM booking_seats
    WHERE departure_id = @departure_id AND seat_cell_id = @seat_cell_id
      AND (status = 'sold' OR (status = 'held' AND hold_expires_at > now()))
)::bool;

-- name: ListOnboardSales :many
-- Ventes à bord d'un départ, avec l'encaissement en espèces.
SELECT b.id, b.reference, b.contact_name, b.contact_phone, b.total_amount, b.status, b.boarding_seq, b.alighting_seq,
       b.sold_offline, b.client_sale_id, b.created_at, b.created_by_staff_id,
       COALESCE(s.full_name, '')::text AS seller_name,
       (SELECT count(*) FROM booking_seats bs WHERE bs.booking_id = b.id AND bs.status = 'sold')::int AS seats,
       COALESCE((SELECT string_agg(bs.seat_label, ', ' ORDER BY bs.seat_label) FROM booking_seats bs
                 WHERE bs.booking_id = b.id AND bs.status = 'sold'), '')::text AS seat_labels,
       COALESCE((SELECT sum(p.amount) FROM payments p
                 WHERE p.booking_id = b.id AND p.provider = 'cash' AND p.status IN ('succeeded', 'refunded')), 0)::bigint AS cash
FROM bookings b
LEFT JOIN staff_users s ON s.id = b.created_by_staff_id
WHERE b.departure_id = @departure_id AND b.channel = 'onboard'
ORDER BY b.created_at;

-- name: OnboardCashTotal :one
-- Espèces encaissées à bord pour un départ (caisse du contrôleur).
SELECT COALESCE(sum(p.amount), 0)::bigint AS cash, count(DISTINCT b.id)::int AS sales_count
FROM bookings b
JOIN payments p ON p.booking_id = b.id AND p.provider = 'cash' AND p.status IN ('succeeded', 'refunded')
WHERE b.departure_id = @departure_id AND b.channel = 'onboard';

-- name: InsertOnboardSaleIssue :one
INSERT INTO onboard_sale_issues (company_id, departure_id, staff_id, client_sale_id, reason, amount, payload, sold_at)
VALUES (@company_id, @departure_id, @staff_id, @client_sale_id, @reason, @amount, @payload, @sold_at)
ON CONFLICT (staff_id, client_sale_id) DO UPDATE SET reason = EXCLUDED.reason
RETURNING *;

-- name: GetOnboardSaleIssue :one
SELECT * FROM onboard_sale_issues WHERE staff_id = @staff_id AND client_sale_id = @client_sale_id;

-- name: ListOnboardSaleIssues :many
SELECT i.*, s.full_name AS staff_name
FROM onboard_sale_issues i JOIN staff_users s ON s.id = i.staff_id
WHERE i.departure_id = @departure_id
ORDER BY i.sold_at;

-- name: ResolveOnboardSaleIssue :one
UPDATE onboard_sale_issues SET resolved_at = now(), resolved_by = @resolved_by, resolution = @resolution
WHERE id = @id AND departure_id = @departure_id AND resolved_at IS NULL
RETURNING *;

-- name: GetOnboardCashHandover :one
SELECT h.*, s.full_name AS received_by_name, a.name AS agency_name
FROM onboard_cash_handovers h
JOIN staff_users s ON s.id = h.received_by
JOIN agencies a ON a.id = h.agency_id
WHERE h.departure_id = @departure_id;

-- name: InsertOnboardCashHandover :one
INSERT INTO onboard_cash_handovers (company_id, departure_id, controller_id, agency_id, expected_cash, counted_cash, sales_count, note, received_by)
VALUES (@company_id, @departure_id, @controller_id, @agency_id, @expected_cash, @counted_cash, @sales_count, @note, @received_by)
RETURNING *;

-- ============================ Codes promo ============================

-- name: GetPromoCodeByCode :one
SELECT * FROM promo_codes WHERE company_id = @company_id AND code = upper(@code::text);

-- name: GetPromoCode :one
SELECT * FROM promo_codes WHERE id = $1;

-- name: CountPromoUses :one
-- Utilisations en cours ou confirmées (les réservations expirées ou annulées avant paiement libèrent le code).
SELECT count(*)::int FROM promo_redemptions WHERE promo_code_id = $1 AND status <> 'released';

-- name: CountCustomerPromoUses :one
SELECT count(*)::int FROM promo_redemptions
WHERE promo_code_id = @promo_code_id AND status <> 'released'
  AND (phone = @phone OR (sqlc.narg('customer_id')::uuid IS NOT NULL AND customer_id = sqlc.narg('customer_id')::uuid));

-- name: CountCustomerPaidTrips :one
-- Voyages déjà payés auprès de la compagnie (premier voyage, parrainage).
SELECT count(*)::int FROM bookings
WHERE company_id = @company_id AND status IN ('confirmed', 'refunded')
  AND (contact_phone = @phone OR (sqlc.narg('customer_id')::uuid IS NOT NULL AND customer_id = sqlc.narg('customer_id')::uuid));

-- name: InsertPromoRedemption :one
INSERT INTO promo_redemptions (company_id, promo_code_id, booking_id, customer_id, phone, amount)
VALUES (@company_id, @promo_code_id, @booking_id, sqlc.narg(customer_id), @phone, @amount)
RETURNING *;

-- name: ConfirmPromoRedemption :exec
UPDATE promo_redemptions SET status = 'confirmed' WHERE booking_id = $1 AND status = 'applied';

-- name: ReleasePromoRedemption :exec
UPDATE promo_redemptions SET status = 'released' WHERE booking_id = $1 AND status = 'applied';

-- name: ListPromoCodes :many
SELECT pc.*,
       (SELECT count(*) FROM promo_redemptions r WHERE r.promo_code_id = pc.id AND r.status = 'confirmed')::int AS uses,
       (SELECT coalesce(sum(r.amount), 0) FROM promo_redemptions r WHERE r.promo_code_id = pc.id AND r.status = 'confirmed')::bigint AS discount_total
FROM promo_codes pc
WHERE pc.company_id = @company_id
  AND (sqlc.narg('q')::text IS NULL OR pc.code ILIKE '%' || sqlc.narg('q')::text || '%' OR pc.label ILIKE '%' || sqlc.narg('q')::text || '%')
  AND (sqlc.narg('batch_id')::uuid IS NULL OR pc.batch_id = sqlc.narg('batch_id')::uuid)
ORDER BY pc.created_at DESC
LIMIT @lim OFFSET @off;

-- name: InsertPromoCode :one
INSERT INTO promo_codes (company_id, code, label, kind, value, max_discount, min_amount, valid_from, valid_to,
                         usage_limit, per_customer_limit, first_trip_only, channels, route_ids, category_codes,
                         batch_id, is_active, created_by)
VALUES (@company_id, upper(@code::text), @label, @kind, @value, sqlc.narg(max_discount), @min_amount, sqlc.narg(valid_from),
        sqlc.narg(valid_to), sqlc.narg(usage_limit), @per_customer_limit, @first_trip_only, @channels, @route_ids,
        @category_codes, sqlc.narg(batch_id), @is_active, sqlc.narg(created_by))
RETURNING *;

-- name: UpdatePromoCode :one
UPDATE promo_codes SET
    label = @label, kind = @kind, value = @value, max_discount = sqlc.narg(max_discount), min_amount = @min_amount,
    valid_from = sqlc.narg(valid_from), valid_to = sqlc.narg(valid_to), usage_limit = sqlc.narg(usage_limit),
    per_customer_limit = @per_customer_limit, first_trip_only = @first_trip_only, channels = @channels,
    route_ids = @route_ids, category_codes = @category_codes, is_active = @is_active
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: PromoCodeStats :one
SELECT
    (SELECT count(*) FROM promo_redemptions r WHERE r.promo_code_id = @id AND r.status = 'confirmed')::int AS uses,
    (SELECT count(*) FROM promo_redemptions r WHERE r.promo_code_id = @id AND r.status = 'applied')::int AS pending,
    (SELECT coalesce(sum(r.amount), 0) FROM promo_redemptions r WHERE r.promo_code_id = @id AND r.status = 'confirmed')::bigint AS discount_total,
    (SELECT coalesce(sum(b.total_amount), 0) FROM promo_redemptions r JOIN bookings b ON b.id = r.booking_id
       WHERE r.promo_code_id = @id AND r.status = 'confirmed')::bigint AS revenue,
    (SELECT count(DISTINCT r.phone) FROM promo_redemptions r WHERE r.promo_code_id = @id AND r.status = 'confirmed')::int AS customers;

-- ============================ Réservez tôt ============================

-- name: ListActivePricingRules :many
SELECT * FROM pricing_rules WHERE company_id = $1 AND is_active ORDER BY percent DESC;

-- name: ListPricingRules :many
SELECT pr.*, r.name AS route_name FROM pricing_rules pr
LEFT JOIN routes r ON r.id = pr.route_id
WHERE pr.company_id = $1
ORDER BY pr.is_active DESC, pr.created_at DESC;

-- name: InsertPricingRule :one
INSERT INTO pricing_rules (company_id, label, route_id, min_days_before, percent, seat_quota, valid_from, valid_to,
                           excluded_dates, is_active)
VALUES (@company_id, @label, sqlc.narg(route_id), @min_days_before, @percent, @seat_quota, sqlc.narg(valid_from),
        sqlc.narg(valid_to), @excluded_dates, @is_active)
RETURNING *;

-- name: UpdatePricingRule :one
UPDATE pricing_rules SET
    label = @label, route_id = sqlc.narg(route_id), min_days_before = @min_days_before, percent = @percent,
    seat_quota = @seat_quota, valid_from = sqlc.narg(valid_from), valid_to = sqlc.narg(valid_to),
    excluded_dates = @excluded_dates, is_active = @is_active
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: CountEarlyBirdSeats :one
-- Places déjà vendues (ou bloquées) au tarif « réservez tôt » sur un départ.
SELECT count(*)::int FROM booking_seats
WHERE departure_id = $1 AND early_bird_amount > 0
  AND (status = 'sold' OR (status = 'held' AND hold_expires_at > now()));

-- ============================ Parrainage ============================

-- name: GetCustomerByReferralCode :one
SELECT * FROM customers WHERE referral_code = upper(@code::text) AND status = 'active';

-- name: InsertReferral :one
INSERT INTO referrals (referrer_id, referee_id) VALUES ($1, $2)
ON CONFLICT (referee_id) DO NOTHING
RETURNING *;

-- name: GetReferralByReferee :one
SELECT * FROM referrals WHERE referee_id = $1;

-- name: SetReferralBooking :exec
UPDATE referrals SET booking_id = @booking_id, company_id = @company_id WHERE id = @id AND status = 'pending';

-- name: ClearReferralBooking :exec
-- Première réservation expirée ou annulée avant paiement : la remise reste disponible.
UPDATE referrals SET booking_id = NULL, company_id = NULL WHERE booking_id = $1 AND status = 'pending';

-- name: MarkReferralRewarded :exec
UPDATE referrals SET status = 'rewarded', rewarded_at = now() WHERE id = $1 AND status = 'pending';

-- name: MarkReferralRejected :exec
UPDATE referrals SET status = 'rejected', reject_reason = @reason WHERE id = @id AND status = 'pending';

-- name: CountReferrerRewardsSince :one
SELECT count(*)::int FROM referrals WHERE referrer_id = @referrer_id AND status = 'rewarded' AND rewarded_at >= @since;

-- name: ListReferralsByReferrer :many
SELECT r.*, c.full_name AS referee_name FROM referrals r
JOIN customers c ON c.id = r.referee_id
WHERE r.referrer_id = $1
ORDER BY r.created_at DESC
LIMIT 100;

-- name: CountCustomerPaidTripsAll :one
-- Le client a-t-il déjà payé un voyage (toutes compagnies) ? Le parrainage n'est ouvert qu'aux nouveaux voyageurs.
SELECT count(*)::int FROM bookings WHERE customer_id = $1 AND status IN ('confirmed', 'refunded');

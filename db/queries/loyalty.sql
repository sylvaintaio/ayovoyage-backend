-- name: GetLoyaltyProgram :one
SELECT * FROM loyalty_programs WHERE company_id = $1;

-- name: UpsertLoyaltyProgram :one
INSERT INTO loyalty_programs (company_id, is_enabled, points_per_100, point_value, max_redeem_percent,
                              points_validity_months, signup_bonus_points, referee_reward_amount,
                              referrer_reward_points, referrer_monthly_cap)
VALUES (@company_id, @is_enabled, @points_per_100, @point_value, @max_redeem_percent,
        @points_validity_months, @signup_bonus_points, @referee_reward_amount,
        @referrer_reward_points, @referrer_monthly_cap)
ON CONFLICT (company_id) DO UPDATE SET
    is_enabled             = EXCLUDED.is_enabled,
    points_per_100         = EXCLUDED.points_per_100,
    point_value            = EXCLUDED.point_value,
    max_redeem_percent     = EXCLUDED.max_redeem_percent,
    points_validity_months = EXCLUDED.points_validity_months,
    signup_bonus_points    = EXCLUDED.signup_bonus_points,
    referee_reward_amount  = EXCLUDED.referee_reward_amount,
    referrer_reward_points = EXCLUDED.referrer_reward_points,
    referrer_monthly_cap   = EXCLUDED.referrer_monthly_cap
RETURNING *;

-- name: ListLoyaltyTiers :many
SELECT * FROM loyalty_tiers WHERE company_id = $1 ORDER BY min_trips;

-- name: DeleteLoyaltyTiers :exec
DELETE FROM loyalty_tiers WHERE company_id = $1;

-- name: InsertLoyaltyTier :one
INSERT INTO loyalty_tiers (company_id, code, name, min_trips, discount_percent, perks, sort_order,
                           cancellation_fee_percent, cancellation_cutoff_hours, free_change)
VALUES (@company_id, @code, @name, @min_trips, @discount_percent, @perks, @sort_order,
        sqlc.narg(cancellation_fee_percent), sqlc.narg(cancellation_cutoff_hours), @free_change)
RETURNING *;

-- name: EnsureLoyaltyAccount :one
INSERT INTO loyalty_accounts (company_id, customer_id)
VALUES ($1, $2)
ON CONFLICT (company_id, customer_id) DO UPDATE SET company_id = loyalty_accounts.company_id
RETURNING *, (xmax = 0) AS inserted;

-- name: GetLoyaltyAccount :one
SELECT * FROM loyalty_accounts WHERE company_id = $1 AND customer_id = $2;

-- name: GetLoyaltyAccountForUpdate :one
SELECT * FROM loyalty_accounts WHERE company_id = $1 AND customer_id = $2 FOR UPDATE;

-- name: AddLoyaltyPoints :one
UPDATE loyalty_accounts SET points_balance = points_balance + @delta WHERE id = @id RETURNING *;

-- name: SetLoyaltyTier :exec
UPDATE loyalty_accounts SET tier_code = $2, trips_12m = $3 WHERE id = $1;

-- name: InsertLoyaltyEntry :one
INSERT INTO loyalty_ledger (company_id, account_id, entry_type, points, booking_id, ticket_id, description, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: CountCustomerTrips12m :one
SELECT count(*)::int FROM tickets t
JOIN bookings b ON b.id = t.booking_id
WHERE b.company_id = $1 AND b.customer_id = $2
  AND t.status = 'used' AND t.boarded_at > now() - interval '12 months';

-- name: ListLoyaltyLedger :many
SELECT * FROM loyalty_ledger WHERE account_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: ListCustomerLoyaltyAccounts :many
SELECT la.*, co.name AS company_name, co.slug AS company_slug
FROM loyalty_accounts la
JOIN companies co ON co.id = la.company_id
WHERE la.customer_id = $1
ORDER BY co.name;

-- name: ListDormantLoyaltyAccounts :many
-- Comptes sans activité depuis la durée de validité du programme : leurs points expirent.
SELECT la.* FROM loyalty_accounts la
JOIN loyalty_programs lp ON lp.company_id = la.company_id
WHERE la.points_balance > 0
  AND la.updated_at < now() - make_interval(months => lp.points_validity_months)
LIMIT 500;

-- name: GetLoyaltyStats :one
SELECT count(*)::int AS members,
       coalesce(sum(points_balance), 0)::bigint AS points_outstanding,
       count(*) FILTER (WHERE updated_at > now() - interval '90 days')::int AS active_members
FROM loyalty_accounts WHERE company_id = $1;

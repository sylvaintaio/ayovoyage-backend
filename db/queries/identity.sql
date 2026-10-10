-- ============================ Clients ============================

-- name: GetCustomer :one
SELECT * FROM customers WHERE id = $1;

-- name: GetCustomerByPhone :one
SELECT * FROM customers WHERE phone = $1;

-- name: UpsertCustomerByEmail :one
-- Crée le client s'il n'existe pas (connexion par e-mail).
INSERT INTO customers (email, full_name, referral_code)
VALUES (lower(@email::text), @full_name, @referral_code)
ON CONFLICT (lower(email)) WHERE email IS NOT NULL DO UPDATE
    SET full_name = CASE WHEN customers.full_name = '' THEN EXCLUDED.full_name ELSE customers.full_name END
RETURNING *, (xmax = 0) AS inserted;

-- name: UpsertCustomerByPhone :one
-- Crée le client s'il n'existe pas (inscription par OTP ou vente au guichet).
INSERT INTO customers (phone, full_name, referral_code)
VALUES (@phone, @full_name, @referral_code)
ON CONFLICT (phone) DO UPDATE
    SET full_name = CASE WHEN customers.full_name = '' THEN EXCLUDED.full_name ELSE customers.full_name END
RETURNING *, (xmax = 0) AS inserted;

-- name: UpdateCustomerProfile :one
-- Mise à jour partielle : un e-mail ou une date absents (NULL) sont conservés,
-- sinon un compte créé par e-mail perdrait son seul moyen de contact.
UPDATE customers SET
    full_name  = @full_name,
    email      = coalesce(lower(nullif(trim(sqlc.narg(email)::text), '')), email),
    birth_date = coalesce(sqlc.narg(birth_date)::date, birth_date)
WHERE id = @id
RETURNING *;

-- name: ListCustomerPassengers :many
SELECT * FROM customer_passengers WHERE customer_id = $1 ORDER BY full_name;

-- name: CreateCustomerPassenger :one
INSERT INTO customer_passengers (customer_id, full_name, phone, relationship)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: DeleteCustomerPassenger :execrows
DELETE FROM customer_passengers WHERE id = $1 AND customer_id = $2;

-- ============================ Personnel ============================

-- name: GetStaffByEmail :one
SELECT * FROM staff_users WHERE email = lower(@email::text);

-- name: GetStaff :one
SELECT * FROM staff_users WHERE id = $1;

-- name: ListStaff :many
SELECT * FROM staff_users WHERE company_id = $1 ORDER BY full_name;

-- name: CreateStaff :one
INSERT INTO staff_users (company_id, agency_id, email, password_hash, full_name, phone, role)
VALUES (@company_id, @agency_id, lower(@email::text), @password_hash, @full_name, @phone, @role)
RETURNING *;

-- name: UpdateStaff :one
UPDATE staff_users SET agency_id = @agency_id, full_name = @full_name, phone = @phone,
                       role = @role, is_active = @is_active
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- name: SetStaffPassword :exec
UPDATE staff_users SET password_hash = $2, failed_logins = 0, locked_until = NULL WHERE id = $1;

-- name: RecordStaffLoginSuccess :exec
UPDATE staff_users SET last_login_at = now(), failed_logins = 0, locked_until = NULL WHERE id = $1;

-- name: RecordStaffLoginFailure :exec
-- Verrouillage progressif : 15 min après 5 échecs consécutifs.
UPDATE staff_users SET
    failed_logins = failed_logins + 1,
    locked_until  = CASE WHEN failed_logins + 1 >= 5 THEN now() + interval '15 minutes' ELSE locked_until END
WHERE id = $1;

-- ============================ Terminaux ============================

-- name: CreateDevice :one
INSERT INTO devices (company_id, staff_id, name, platform) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetDevice :one
SELECT * FROM devices WHERE id = $1;

-- name: ListDevices :many
SELECT * FROM devices WHERE company_id = $1 ORDER BY created_at DESC;

-- name: SetDeviceActive :one
UPDATE devices SET is_active = $3 WHERE id = $1 AND company_id = $2 RETURNING *;

-- name: TouchDevice :exec
UPDATE devices SET last_seen_at = now() WHERE id = $1;

-- ============================ OTP ============================

-- name: CreateOTP :one
INSERT INTO otp_codes (destination, channel, code_hash, expires_at, requester_ip)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CountOTPsByDestinationSince :one
SELECT count(*) FROM otp_codes WHERE destination = $1 AND created_at > $2;

-- name: CountOTPsByIPSince :one
SELECT count(*) FROM otp_codes WHERE requester_ip = $1 AND created_at > $2;

-- name: GetLatestActiveOTP :one
SELECT * FROM otp_codes
WHERE destination = $1 AND consumed_at IS NULL AND expires_at > now()
ORDER BY created_at DESC
LIMIT 1
FOR UPDATE;

-- name: IncrementOTPAttempts :exec
UPDATE otp_codes SET attempts = attempts + 1 WHERE id = $1;

-- name: ConsumeOTP :exec
UPDATE otp_codes SET consumed_at = now() WHERE id = $1;

-- ============================ Jetons de rafraîchissement ============================

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (subject_type, subject_id, family_id, token_hash, expires_at, user_agent, ip, mfa_at)
VALUES (@subject_type, @subject_id, @family_id, @token_hash, @expires_at, @user_agent, @ip, sqlc.narg(mfa_at))
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: RotateRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now(), replaced_by = $2 WHERE id = $1;

-- name: RevokeRefreshFamily :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL;

-- name: RevokeSubjectRefreshTokens :exec
UPDATE refresh_tokens SET revoked_at = now()
WHERE subject_type = $1 AND subject_id = $2 AND revoked_at IS NULL;

-- ============================ Audit ============================

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (company_id, actor_type, actor_id, action, entity_type, entity_id, before, after, ip, user_agent)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListAuditLogs :many
SELECT * FROM audit_logs
WHERE company_id = @company_id
  AND (sqlc.narg('entity_type')::text IS NULL OR entity_type = sqlc.narg('entity_type')::text)
  AND (sqlc.narg('entity_id')::text IS NULL OR entity_id = sqlc.narg('entity_id')::text)
  AND (sqlc.narg('actor_id')::uuid IS NULL OR actor_id = sqlc.narg('actor_id')::uuid)
ORDER BY created_at DESC
LIMIT @lim OFFSET @off;

-- ============================ Double authentification (personnel) ============================

-- name: SetStaffTOTPPending :exec
-- Secret généré, en attente de confirmation par un premier code.
UPDATE staff_users SET totp_secret_enc = @secret_enc, totp_enabled_at = NULL, totp_last_step = 0 WHERE id = @id;

-- name: EnableStaffTOTP :exec
UPDATE staff_users SET totp_enabled_at = now(), totp_last_step = @last_step WHERE id = @id;

-- name: SetStaffTOTPStep :exec
UPDATE staff_users SET totp_last_step = @last_step WHERE id = @id AND totp_last_step < @last_step;

-- name: ResetStaffTOTP :exec
-- Réinitialisation par un administrateur : nouvelle activation obligatoire à la prochaine connexion.
UPDATE staff_users SET totp_secret_enc = NULL, totp_enabled_at = NULL, totp_last_step = 0, mfa_required_from = now()
WHERE id = @id AND company_id = @company_id;

-- name: DeleteStaffRecoveryCodes :exec
DELETE FROM staff_recovery_codes WHERE staff_id = $1;

-- name: InsertStaffRecoveryCode :exec
INSERT INTO staff_recovery_codes (company_id, staff_id, code_hash) VALUES ($1, $2, $3);

-- name: UseStaffRecoveryCode :one
UPDATE staff_recovery_codes SET used_at = now()
WHERE staff_id = @staff_id AND code_hash = @code_hash AND used_at IS NULL
RETURNING id;

-- name: CountStaffRecoveryCodesLeft :one
SELECT count(*)::int FROM staff_recovery_codes WHERE staff_id = $1 AND used_at IS NULL;

-- ============================ Suppression de compte client ============================

-- name: CountCustomerOpenItems :one
-- Ce qui empêche la suppression : voyages à venir et remboursements en cours.
SELECT
    (SELECT count(*) FROM bookings b JOIN departures d ON d.id = b.departure_id
     WHERE b.customer_id = @customer_id AND b.status IN ('pending', 'confirmed') AND d.departure_at > now())::int AS upcoming_trips,
    (SELECT count(*) FROM refunds r JOIN bookings b ON b.id = r.booking_id
     WHERE b.customer_id = @customer_id AND r.status IN ('requested', 'approved'))::int AS open_refunds;

-- name: AnonymizeCustomer :exec
UPDATE customers SET status = 'deleted', deleted_at = now(), full_name = 'Compte supprimé',
    phone = NULL, email = NULL, birth_date = NULL
WHERE id = $1;

-- name: AnonymizeCustomerBookings :exec
-- Les montants restent (obligations comptables) ; les données personnelles sont effacées.
UPDATE bookings SET contact_name = 'Client supprimé', contact_phone = '', contact_email = NULL WHERE customer_id = $1;

-- name: AnonymizeCustomerSeats :exec
UPDATE booking_seats SET passenger_name = 'Passager', passenger_phone = ''
WHERE booking_id IN (SELECT id FROM bookings WHERE customer_id = $1);

-- name: DeleteCustomerPassengers :exec
DELETE FROM customer_passengers WHERE customer_id = $1;

-- name: DeleteCustomerNotifications :exec
DELETE FROM notifications WHERE customer_id = $1;

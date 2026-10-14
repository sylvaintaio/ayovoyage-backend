-- ============================ Notifications push ============================

-- name: UpsertPushDevice :one
-- Un jeton appartient à un seul compte : réinscrit par un autre compte (changement d'utilisateur
-- sur le même téléphone), il change de propriétaire.
INSERT INTO push_devices (token, platform, app, customer_id, staff_id, company_id)
VALUES (@token, @platform, @app, sqlc.narg(customer_id), sqlc.narg(staff_id), sqlc.narg(company_id))
ON CONFLICT (token) DO UPDATE SET
    platform = EXCLUDED.platform, app = EXCLUDED.app, customer_id = EXCLUDED.customer_id,
    staff_id = EXCLUDED.staff_id, company_id = EXCLUDED.company_id, last_seen_at = now()
RETURNING *;

-- name: DeletePushDevice :exec
DELETE FROM push_devices
WHERE token = @token
  AND (customer_id = sqlc.narg(customer_id) OR staff_id = sqlc.narg(staff_id));

-- name: DeletePushDeviceByToken :exec
-- Jeton refusé par Firebase (application désinstallée, jeton périmé).
DELETE FROM push_devices WHERE token = $1;

-- name: ListPushDevicesForCustomer :many
SELECT * FROM push_devices WHERE customer_id = $1 AND app = 'voyageur';

-- name: ListPushDevicesForStaff :many
SELECT * FROM push_devices WHERE staff_id = $1 AND app = 'agent';

-- ============================ Boîte de réception ============================

-- name: InsertInboxMessage :one
-- Sans effet si cette personne a déjà reçu cet événement (dedup_key) : renvoie alors aucune ligne.
INSERT INTO inbox_messages (customer_id, staff_id, company_id, kind, title, body, link, data, dedup_key)
VALUES (sqlc.narg(customer_id), sqlc.narg(staff_id), sqlc.narg(company_id), @kind, @title, @body, @link, @data, sqlc.narg(dedup_key))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetInboxMessage :one
SELECT * FROM inbox_messages WHERE id = $1;

-- name: ClaimInboxToPush :many
-- Messages à pousser (récents, moins de 5 tentatives) ; verrouillés pour un seul envoi à la fois.
SELECT * FROM inbox_messages
WHERE pushed_at IS NULL AND push_attempts < 5 AND created_at > now() - interval '6 hours'
ORDER BY created_at
LIMIT @lim
FOR UPDATE SKIP LOCKED;

-- name: MarkInboxPushed :exec
UPDATE inbox_messages SET pushed_at = now(), push_attempts = push_attempts + 1 WHERE id = $1;

-- name: MarkInboxPushFailed :exec
UPDATE inbox_messages SET push_attempts = push_attempts + 1 WHERE id = $1;

-- name: ListInbox :many
SELECT * FROM inbox_messages
WHERE (customer_id = sqlc.narg(customer_id) OR staff_id = sqlc.narg(staff_id))
  AND (sqlc.narg(before)::timestamptz IS NULL OR created_at < sqlc.narg(before)::timestamptz)
ORDER BY created_at DESC
LIMIT @lim;

-- name: CountUnreadInbox :one
SELECT count(*)::int FROM inbox_messages
WHERE (customer_id = sqlc.narg(customer_id) OR staff_id = sqlc.narg(staff_id)) AND read_at IS NULL;

-- name: MarkInboxRead :execrows
UPDATE inbox_messages SET read_at = now()
WHERE id = @id AND read_at IS NULL
  AND (customer_id = sqlc.narg(customer_id) OR staff_id = sqlc.narg(staff_id));

-- name: MarkAllInboxRead :execrows
UPDATE inbox_messages SET read_at = now()
WHERE read_at IS NULL AND (customer_id = sqlc.narg(customer_id) OR staff_id = sqlc.narg(staff_id));

-- name: PurgeOldInbox :execrows
-- Messages de plus de 90 jours : effacés.
DELETE FROM inbox_messages WHERE created_at < now() - interval '90 days';

-- name: ListWaitingTravellers :many
-- Voyageurs avec un compte, pas encore à bord, qui montent après l'arrêt after_seq (avancement du car).
SELECT DISTINCT ON (b.id) b.id AS booking_id, b.customer_id, b.reference, b.boarding_seq
FROM bookings b
JOIN tickets t ON t.booking_id = b.id AND t.status = 'valid'
WHERE b.departure_id = @departure_id AND b.status = 'confirmed' AND b.customer_id IS NOT NULL
  AND b.boarding_seq > @after_seq
ORDER BY b.id;

-- name: ListDepartureStaffIDs :many
-- Contrôleur, chauffeur et autres membres d'équipage d'un départ.
SELECT x.staff_id::uuid AS staff_id, x.role::text AS role FROM (
    SELECT d.agent_id AS staff_id, 'controller' AS role FROM departures d WHERE d.id = @departure_id AND d.agent_id IS NOT NULL
    UNION
    SELECT d.driver_id, 'driver' FROM departures d WHERE d.id = @departure_id AND d.driver_id IS NOT NULL
    UNION
    SELECT c.staff_id, c.role FROM departure_crew c WHERE c.departure_id = @departure_id
) x;

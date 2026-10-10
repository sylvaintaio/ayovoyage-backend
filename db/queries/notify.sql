-- name: InsertNotification :exec
INSERT INTO notifications (company_id, customer_id, channel, template, recipient, status, error, subject, body)
VALUES (@company_id, @customer_id, @channel, @template, @recipient, @status, @error, @subject, @body);

-- name: ListDepartureContacts :many
-- Contacts des réservations confirmées d'un départ (alerte retard, départ, arrivée).
SELECT DISTINCT b.contact_phone, b.customer_id
FROM bookings b
WHERE b.departure_id = $1 AND b.status = 'confirmed';

-- name: WelcomeAlreadySent :one
SELECT EXISTS (
    SELECT 1 FROM notifications
    WHERE customer_id = @customer_id AND template = 'welcome' AND status = 'sent'
) AS sent;

-- name: ListCustomerMessages :many
-- Historique des messages d'un client (plus récents d'abord).
SELECT n.id, n.channel, n.template, n.recipient, n.status, n.subject, n.body, n.created_at,
       c.name AS company_name
FROM notifications n
LEFT JOIN companies c ON c.id = n.company_id
WHERE n.customer_id = @customer_id
ORDER BY n.created_at DESC, n.id DESC
LIMIT @lim;

-- name: ListBookingsToRemind :many
-- Rappel « la veille » : départ dans 12 à 26 h, réservation confirmée depuis plus d'une heure.
SELECT b.id
FROM bookings b
JOIN departures d ON d.id = b.departure_id
WHERE b.status = 'confirmed' AND b.reminder_sent_at IS NULL
  AND d.status IN ('scheduled', 'boarding')
  AND d.departure_at > now() + interval '12 hours' AND d.departure_at <= now() + interval '26 hours'
  AND b.confirmed_at <= now() - interval '1 hour'
ORDER BY d.departure_at
LIMIT 500
FOR UPDATE OF b SKIP LOCKED;

-- name: MarkReminderSent :exec
UPDATE bookings SET reminder_sent_at = now() WHERE id = $1;

-- name: ListBookingsToRemindToday :many
-- Rappel « quelques heures avant » : départ dans 3 à 7 h (envoi vers H-6), réservation confirmée depuis plus d'une heure.
SELECT b.id
FROM bookings b
JOIN departures d ON d.id = b.departure_id
WHERE b.status = 'confirmed' AND b.day_reminder_sent_at IS NULL
  AND d.status IN ('scheduled', 'boarding')
  AND d.departure_at > now() + interval '3 hours' AND d.departure_at <= now() + interval '7 hours'
  AND b.confirmed_at <= now() - interval '1 hour'
ORDER BY d.departure_at
LIMIT 500
FOR UPDATE OF b SKIP LOCKED;

-- name: MarkDayReminderSent :exec
UPDATE bookings SET day_reminder_sent_at = now() WHERE id = $1;

-- name: PurgeExpiredOTPs :execrows
DELETE FROM otp_codes WHERE expires_at < now() - interval '1 day';

-- name: PurgeOldRefreshTokens :execrows
-- Jetons expirés depuis 7 jours ou révoqués depuis 30 jours (la détection de vol reste possible entre-temps).
DELETE FROM refresh_tokens
WHERE expires_at < now() - interval '7 days' OR revoked_at < now() - interval '30 days';

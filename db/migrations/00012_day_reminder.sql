-- +goose Up
-- Second rappel, environ 6 heures avant le départ (une fois par réservation).
ALTER TABLE bookings ADD COLUMN day_reminder_sent_at timestamptz;

-- +goose Down
ALTER TABLE bookings DROP COLUMN day_reminder_sent_at;

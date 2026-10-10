-- +goose Up
-- Messages traités en mode test (pilote « log ») : enregistrés comme « simulated »,
-- pour ne pas les présenter comme envoyés.
ALTER TABLE notifications DROP CONSTRAINT notifications_status_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_status_check CHECK (status IN ('sent', 'failed', 'simulated'));

-- +goose Down
UPDATE notifications SET status = 'sent' WHERE status = 'simulated';
ALTER TABLE notifications DROP CONSTRAINT notifications_status_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_status_check CHECK (status IN ('sent', 'failed'));

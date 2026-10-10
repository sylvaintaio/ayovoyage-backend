-- +goose Up
-- Messages envoyés aux clients : on conserve le sujet et le texte pour que le
-- client retrouve son historique dans son espace (journal d'activité).
-- Les codes de connexion ne sont jamais enregistrés.
ALTER TABLE notifications
    ADD COLUMN subject text NOT NULL DEFAULT '',
    ADD COLUMN body    text NOT NULL DEFAULT '';

-- Un seul e-mail de bienvenue par client, même si la tâche est rejouée.
CREATE UNIQUE INDEX notifications_welcome_uq ON notifications (customer_id, channel)
    WHERE template = 'welcome' AND status = 'sent';

-- +goose Down
DROP INDEX notifications_welcome_uq;
ALTER TABLE notifications DROP COLUMN body, DROP COLUMN subject;

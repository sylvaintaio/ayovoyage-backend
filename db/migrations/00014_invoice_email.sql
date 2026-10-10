-- +goose Up
-- Envoi des factures par e-mail (PDF en pièce jointe).
--   none    : rien à envoyer automatiquement (pas d'adresse, ou facture jointe à l'e-mail de confirmation du billet)
--   pending : à envoyer (tâche toutes les minutes, 5 essais espacés)
--   sent / simulated / failed : résultat du dernier envoi (simulated : pilote « log », rien n'est parti)
ALTER TABLE invoices
    ADD COLUMN email_status   text NOT NULL DEFAULT 'none' CHECK (email_status IN ('none', 'pending', 'sent', 'simulated', 'failed')),
    ADD COLUMN email_to       text NOT NULL DEFAULT '',
    ADD COLUMN email_attempts int NOT NULL DEFAULT 0,
    ADD COLUMN email_next_at  timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN email_error    text NOT NULL DEFAULT '',
    ADD COLUMN emailed_at     timestamptz;

CREATE INDEX invoices_email_pending ON invoices (email_next_at) WHERE email_status = 'pending';

-- +goose Down
DROP INDEX invoices_email_pending;
ALTER TABLE invoices
    DROP COLUMN email_status, DROP COLUMN email_to, DROP COLUMN email_attempts,
    DROP COLUMN email_next_at, DROP COLUMN email_error, DROP COLUMN emailed_at;

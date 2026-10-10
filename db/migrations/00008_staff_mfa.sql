-- +goose Up
-- Double authentification (TOTP) du personnel.
ALTER TABLE staff_users
    ADD COLUMN totp_secret_enc  text,                 -- secret chiffré (AES-GCM, clé MFA_ENCRYPTION_KEY)
    ADD COLUMN totp_enabled_at  timestamptz,          -- NULL = pas encore activée
    ADD COLUMN totp_last_step   bigint NOT NULL DEFAULT 0, -- dernier pas utilisé (anti-rejeu)
    ADD COLUMN mfa_required_from timestamptz NOT NULL DEFAULT now(); -- activation obligatoire à partir de cette date

-- Comptes existants : 7 jours pour activer. Nouveaux comptes : activation dès la première connexion.
-- (portée « system » : la table est protégée par la sécurité au niveau des lignes)
SELECT set_config('app.scope', 'system', true);
UPDATE staff_users SET mfa_required_from = now() + interval '7 days';

CREATE TABLE staff_recovery_codes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id uuid NOT NULL REFERENCES companies(id),
    staff_id   uuid NOT NULL REFERENCES staff_users(id) ON DELETE CASCADE,
    code_hash  text NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (staff_id, code_hash)
);
ALTER TABLE staff_recovery_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_recovery_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON staff_recovery_codes USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Les sessions retiennent l'heure de la dernière vérification du code.
ALTER TABLE refresh_tokens ADD COLUMN mfa_at timestamptz;

-- +goose Down
ALTER TABLE refresh_tokens DROP COLUMN mfa_at;
DROP TABLE staff_recovery_codes;
ALTER TABLE staff_users DROP COLUMN mfa_required_from, DROP COLUMN totp_last_step,
    DROP COLUMN totp_enabled_at, DROP COLUMN totp_secret_enc;

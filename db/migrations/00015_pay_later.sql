-- +goose Up
-- Réservation sans paiement immédiat : les places sont bloquées jusqu'à une
-- date limite et payées en agence (ou en ligne plus tard).
SELECT set_config('app.scope', 'system', true);

ALTER TABLE bookings
    ADD COLUMN payment_mode     text NOT NULL DEFAULT 'online'
        CHECK (payment_mode IN ('online', 'agency', 'on_account')),
    ADD COLUMN pay_by           timestamptz,
    ADD COLUMN pay_reminder_sent_at timestamptz;

-- Canal USSD (évolution 10), prévu dès maintenant.
ALTER TABLE bookings DROP CONSTRAINT bookings_channel_check;
ALTER TABLE bookings ADD CONSTRAINT bookings_channel_check CHECK (channel IN ('app', 'web', 'counter', 'ussd'));

CREATE INDEX bookings_pay_later_idx ON bookings (company_id, pay_by) WHERE status = 'pending' AND payment_mode = 'agency';

ALTER TABLE companies
    ADD COLUMN pay_later_enabled        boolean NOT NULL DEFAULT false,
    ADD COLUMN pay_later_hold_hours     integer NOT NULL DEFAULT 24  CHECK (pay_later_hold_hours BETWEEN 1 AND 168),
    ADD COLUMN pay_later_cutoff_minutes integer NOT NULL DEFAULT 120 CHECK (pay_later_cutoff_minutes BETWEEN 0 AND 1440),
    ADD COLUMN pay_later_max_seats      integer NOT NULL DEFAULT 4   CHECK (pay_later_max_seats BETWEEN 1 AND 10),
    ADD COLUMN pay_later_max_active     integer NOT NULL DEFAULT 2   CHECK (pay_later_max_active BETWEEN 1 AND 10),
    ADD COLUMN pay_later_quota_percent  integer NOT NULL DEFAULT 30  CHECK (pay_later_quota_percent BETWEEN 1 AND 100),
    ADD COLUMN pay_later_strikes        integer NOT NULL DEFAULT 3   CHECK (pay_later_strikes BETWEEN 1 AND 20);

-- Numéros privés de l'option après trop de réservations non payées.
CREATE TABLE pay_later_blocks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id    uuid NOT NULL REFERENCES companies(id),
    phone         text NOT NULL,
    blocked_until timestamptz NOT NULL,
    reason        text NOT NULL,
    lifted_by     uuid REFERENCES staff_users(id),
    lifted_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pay_later_blocks_phone_idx ON pay_later_blocks (company_id, phone, blocked_until);
ALTER TABLE pay_later_blocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE pay_later_blocks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pay_later_blocks USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose Down
DROP TABLE pay_later_blocks;
ALTER TABLE companies
    DROP COLUMN pay_later_enabled, DROP COLUMN pay_later_hold_hours, DROP COLUMN pay_later_cutoff_minutes,
    DROP COLUMN pay_later_max_seats, DROP COLUMN pay_later_max_active, DROP COLUMN pay_later_quota_percent,
    DROP COLUMN pay_later_strikes;
DROP INDEX bookings_pay_later_idx;
ALTER TABLE bookings DROP CONSTRAINT bookings_channel_check;
ALTER TABLE bookings ADD CONSTRAINT bookings_channel_check CHECK (channel IN ('app', 'web', 'counter'));
ALTER TABLE bookings DROP COLUMN payment_mode, DROP COLUMN pay_by, DROP COLUMN pay_reminder_sent_at;

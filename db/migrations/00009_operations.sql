-- +goose Up
-- Exploitation : clôture de caisse, rappels de voyage, suppression de compte client.

-- Versement des remboursements : mode et agence (un remboursement en espèces sort de la caisse).
ALTER TABLE refunds
    ADD COLUMN process_method      text CHECK (process_method IN ('mobile_money', 'cash', 'bank_transfer', 'other')),
    ADD COLUMN processed_agency_id uuid REFERENCES agencies(id),
    ADD COLUMN processed_by        uuid REFERENCES staff_users(id);

-- Clôture de caisse quotidienne par agence (une par jour, définitive).
CREATE TABLE cash_closures (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id    uuid NOT NULL REFERENCES companies(id),
    agency_id     uuid NOT NULL REFERENCES agencies(id),
    business_date date NOT NULL,
    cash_in       bigint NOT NULL CHECK (cash_in >= 0),   -- encaissements en espèces
    cash_out      bigint NOT NULL CHECK (cash_out >= 0),  -- remboursements versés en espèces
    expected_cash bigint NOT NULL,                         -- cash_in - cash_out
    counted_cash  bigint NOT NULL CHECK (counted_cash >= 0),
    difference    bigint GENERATED ALWAYS AS (counted_cash - expected_cash) STORED,
    sales_count   integer NOT NULL DEFAULT 0,
    note          text NOT NULL DEFAULT '',
    closed_by     uuid NOT NULL REFERENCES staff_users(id),
    closed_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agency_id, business_date)
);
CREATE INDEX cash_closures_company_date_idx ON cash_closures (company_id, business_date DESC);
ALTER TABLE cash_closures ENABLE ROW LEVEL SECURITY;
ALTER TABLE cash_closures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cash_closures USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));
CREATE INDEX payments_cash_agency_idx ON payments (agency_id, succeeded_at) WHERE provider = 'cash' AND status = 'succeeded';

-- Rappel envoyé la veille du voyage (une seule fois par réservation).
ALTER TABLE bookings ADD COLUMN reminder_sent_at timestamptz;

-- Compte client supprimé : coordonnées effacées, l'historique comptable est conservé anonymisé.
ALTER TABLE customers ADD COLUMN deleted_at timestamptz;
ALTER TABLE customers DROP CONSTRAINT customers_contact_ck;
ALTER TABLE customers ADD CONSTRAINT customers_contact_ck
    CHECK (phone IS NOT NULL OR email IS NOT NULL OR status = 'deleted');

-- +goose Down
ALTER TABLE customers DROP CONSTRAINT customers_contact_ck;
ALTER TABLE customers ADD CONSTRAINT customers_contact_ck CHECK (phone IS NOT NULL OR email IS NOT NULL);
ALTER TABLE customers DROP COLUMN deleted_at;
ALTER TABLE bookings DROP COLUMN reminder_sent_at;
DROP INDEX payments_cash_agency_idx;
DROP TABLE cash_closures;
ALTER TABLE refunds DROP COLUMN processed_by, DROP COLUMN processed_agency_id, DROP COLUMN process_method;

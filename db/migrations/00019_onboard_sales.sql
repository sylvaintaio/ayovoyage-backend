-- +goose Up
-- Vente à bord par le contrôleur : places réservées au contrôleur (quota), ventes en ligne
-- et hors connexion, caisse du contrôleur remise à une agence (voir docs/lignes-arrets-equipage.md).
SELECT set_config('app.scope', 'system', true);

-- Nombre de places réservées au contrôleur sur chaque nouveau départ.
ALTER TABLE companies ADD COLUMN onboard_quota_seats integer NOT NULL DEFAULT 4
    CHECK (onboard_quota_seats BETWEEN 0 AND 20);

-- Places bloquées : « manual » (réserve de la compagnie) ou « onboard » (quota du contrôleur,
-- vendable seulement à bord).
ALTER TABLE departure_blocked_seats ADD COLUMN kind text NOT NULL DEFAULT 'manual'
    CHECK (kind IN ('manual', 'onboard'));

-- Canal « onboard » et identifiant de la vente côté téléphone (renvoi sans effet).
ALTER TABLE bookings DROP CONSTRAINT bookings_channel_check;
ALTER TABLE bookings ADD CONSTRAINT bookings_channel_check CHECK (channel IN ('app', 'web', 'counter', 'ussd', 'onboard'));
ALTER TABLE bookings ADD COLUMN client_sale_id text;
ALTER TABLE bookings ADD COLUMN sold_offline boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX bookings_client_sale_uq ON bookings (created_by_staff_id, client_sale_id) WHERE client_sale_id IS NOT NULL;
CREATE INDEX bookings_onboard_departure_idx ON bookings (departure_id) WHERE channel = 'onboard';

-- Vente hors connexion impossible à placer (plus aucun siège libre sur le tronçon) :
-- l'argent est encaissé et le voyageur à bord, l'anomalie est à régler par la compagnie.
CREATE TABLE onboard_sale_issues (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid NOT NULL REFERENCES companies(id),
    departure_id   uuid NOT NULL REFERENCES departures(id),
    staff_id       uuid NOT NULL REFERENCES staff_users(id),
    client_sale_id text NOT NULL,
    reason         text NOT NULL,
    amount         bigint NOT NULL DEFAULT 0,
    payload        jsonb NOT NULL,
    sold_at        timestamptz NOT NULL,
    resolved_at    timestamptz,
    resolved_by    uuid REFERENCES staff_users(id),
    resolution     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (staff_id, client_sale_id)
);
CREATE INDEX onboard_sale_issues_departure_idx ON onboard_sale_issues (departure_id);
ALTER TABLE onboard_sale_issues ENABLE ROW LEVEL SECURITY;
ALTER TABLE onboard_sale_issues FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON onboard_sale_issues USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Remise de la caisse du contrôleur à une agence, à la fin du voyage (une par départ).
CREATE TABLE onboard_cash_handovers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id    uuid NOT NULL REFERENCES companies(id),
    departure_id  uuid NOT NULL UNIQUE REFERENCES departures(id),
    controller_id uuid REFERENCES staff_users(id),
    agency_id     uuid NOT NULL REFERENCES agencies(id),
    expected_cash bigint NOT NULL,
    counted_cash  bigint NOT NULL CHECK (counted_cash >= 0),
    difference    bigint GENERATED ALWAYS AS (counted_cash - expected_cash) STORED,
    sales_count   integer NOT NULL DEFAULT 0,
    note          text NOT NULL DEFAULT '',
    received_by   uuid NOT NULL REFERENCES staff_users(id),
    received_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX onboard_cash_handovers_agency_idx ON onboard_cash_handovers (agency_id, received_at);
ALTER TABLE onboard_cash_handovers ENABLE ROW LEVEL SECURITY;
ALTER TABLE onboard_cash_handovers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON onboard_cash_handovers USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose Down
SELECT set_config('app.scope', 'system', true);
DROP TABLE onboard_cash_handovers;
DROP TABLE onboard_sale_issues;
DROP INDEX bookings_onboard_departure_idx;
DROP INDEX bookings_client_sale_uq;
ALTER TABLE bookings DROP COLUMN sold_offline;
ALTER TABLE bookings DROP COLUMN client_sale_id;
ALTER TABLE bookings DROP CONSTRAINT bookings_channel_check;
ALTER TABLE bookings ADD CONSTRAINT bookings_channel_check CHECK (channel IN ('app', 'web', 'counter', 'ussd'));
ALTER TABLE departure_blocked_seats DROP COLUMN kind;
ALTER TABLE companies DROP COLUMN onboard_quota_seats;

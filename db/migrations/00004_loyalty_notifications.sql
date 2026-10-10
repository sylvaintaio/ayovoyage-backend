-- +goose Up

-- =====================================================================
-- Fidélité (points propres à chaque compagnie)
-- =====================================================================

CREATE TABLE loyalty_programs (
    company_id             uuid PRIMARY KEY REFERENCES companies(id),
    is_enabled             boolean NOT NULL DEFAULT true,
    points_per_100         integer NOT NULL DEFAULT 1 CHECK (points_per_100 >= 0),   -- points gagnés par tranche de 100 XOF
    point_value            integer NOT NULL DEFAULT 5 CHECK (point_value >= 0),      -- valeur d'un point en XOF à l'utilisation
    max_redeem_percent     integer NOT NULL DEFAULT 50 CHECK (max_redeem_percent BETWEEN 0 AND 100),
    points_validity_months integer NOT NULL DEFAULT 12 CHECK (points_validity_months > 0),
    signup_bonus_points    integer NOT NULL DEFAULT 0 CHECK (signup_bonus_points >= 0),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER loyalty_programs_updated_at BEFORE UPDATE ON loyalty_programs FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE loyalty_programs ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_programs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON loyalty_programs USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Niveaux calculés sur 12 mois glissants (nombre de voyages effectués).
CREATE TABLE loyalty_tiers (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id       uuid NOT NULL REFERENCES companies(id),
    code             text NOT NULL,
    name             text NOT NULL,
    min_trips        integer NOT NULL CHECK (min_trips >= 0),
    discount_percent integer NOT NULL DEFAULT 0 CHECK (discount_percent BETWEEN 0 AND 50),
    perks            text[] NOT NULL DEFAULT '{}'::text[],
    sort_order       integer NOT NULL DEFAULT 0,
    UNIQUE (company_id, code),
    UNIQUE (company_id, min_trips)
);
ALTER TABLE loyalty_tiers ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_tiers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON loyalty_tiers USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE loyalty_accounts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid NOT NULL REFERENCES companies(id),
    customer_id    uuid NOT NULL REFERENCES customers(id),
    points_balance integer NOT NULL DEFAULT 0 CHECK (points_balance >= 0),
    tier_code      text NOT NULL DEFAULT '',
    trips_12m      integer NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, customer_id)
);
CREATE TRIGGER loyalty_accounts_updated_at BEFORE UPDATE ON loyalty_accounts FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE loyalty_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON loyalty_accounts USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Grand livre des points : ajout seul. Le solde du compte est un cache recalculable.
CREATE TABLE loyalty_ledger (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id   uuid NOT NULL REFERENCES companies(id),
    account_id   uuid NOT NULL REFERENCES loyalty_accounts(id),
    entry_type   text NOT NULL CHECK (entry_type IN ('earn', 'redeem', 'restore', 'revoke', 'expire', 'bonus', 'adjust')),
    points       integer NOT NULL CHECK (points <> 0),
    booking_id   uuid REFERENCES bookings(id),
    ticket_id    uuid REFERENCES tickets(id),
    description  text NOT NULL DEFAULT '',
    expires_at   timestamptz,
    created_by   uuid REFERENCES staff_users(id),
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- Un billet ne rapporte des points qu'une seule fois.
CREATE UNIQUE INDEX loyalty_ledger_earn_ticket_uq ON loyalty_ledger (ticket_id) WHERE entry_type = 'earn';
CREATE INDEX loyalty_ledger_account_idx ON loyalty_ledger (account_id, created_at DESC);
CREATE TRIGGER loyalty_ledger_append_only BEFORE UPDATE OR DELETE ON loyalty_ledger
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
ALTER TABLE loyalty_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_ledger FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON loyalty_ledger USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Historique des notifications envoyées
-- =====================================================================

CREATE TABLE notifications (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id  uuid REFERENCES companies(id),
    customer_id uuid REFERENCES customers(id),
    channel     text NOT NULL CHECK (channel IN ('sms', 'email', 'push', 'whatsapp')),
    template    text NOT NULL,
    recipient   text NOT NULL,
    status      text NOT NULL CHECK (status IN ('sent', 'failed')),
    error       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_customer_idx ON notifications (customer_id, created_at DESC);

-- +goose Down
DROP TABLE notifications;
DROP TABLE loyalty_ledger;
DROP TABLE loyalty_accounts;
DROP TABLE loyalty_tiers;
DROP TABLE loyalty_programs;

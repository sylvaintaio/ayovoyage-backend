-- +goose Up

-- =====================================================================
-- Multi-tenant : contexte de requête et Row-Level Security
-- =====================================================================
-- Chaque transaction applicative positionne (SET LOCAL via set_config) :
--   app.scope      : 'staff' | 'customer' | 'public' | 'system'
--   app.company_id : uuid de la compagnie (obligatoire pour 'staff')
-- Si rien n'est positionné, aucune ligne multi-tenant n'est visible (fail-closed).

-- +goose StatementBegin
CREATE FUNCTION app_scope() RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT coalesce(current_setting('app.scope', true), '')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION app_company_id() RETURNS uuid
LANGUAGE sql STABLE AS $$
    SELECT nullif(current_setting('app.company_id', true), '')::uuid
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION tenant_visible(row_company_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT CASE app_scope()
        WHEN 'staff' THEN row_company_id = app_company_id()
        WHEN 'customer' THEN true
        WHEN 'public' THEN true
        WHEN 'system' THEN true
        ELSE false
    END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'la table % est en ajout seul (append-only)', TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

-- =====================================================================
-- Compagnies, villes, agences
-- =====================================================================

CREATE TABLE companies (
    id                          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                        text NOT NULL,
    slug                        text NOT NULL UNIQUE,
    legal_name                  text,
    phone                       text,
    email                       text,
    logo_url                    text,
    currency                    text NOT NULL DEFAULT 'XOF',
    timezone                    text NOT NULL DEFAULT 'Africa/Porto-Novo',
    booking_hold_minutes        integer NOT NULL DEFAULT 10 CHECK (booking_hold_minutes BETWEEN 2 AND 60),
    sales_cutoff_minutes        integer NOT NULL DEFAULT 15 CHECK (sales_cutoff_minutes >= 0),
    cancellation_cutoff_hours   integer NOT NULL DEFAULT 6 CHECK (cancellation_cutoff_hours >= 0),
    cancellation_fee_percent    integer NOT NULL DEFAULT 10 CHECK (cancellation_fee_percent BETWEEN 0 AND 100),
    refund_approval_threshold   bigint  NOT NULL DEFAULT 50000 CHECK (refund_approval_threshold >= 0),
    status                      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER companies_updated_at BEFORE UPDATE ON companies FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE companies ENABLE ROW LEVEL SECURITY;
ALTER TABLE companies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON companies USING (tenant_visible(id)) WITH CHECK (tenant_visible(id));

CREATE TABLE cities (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    slug         text NOT NULL UNIQUE,
    country_code text NOT NULL DEFAULT 'BJ',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE agencies (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id    uuid NOT NULL REFERENCES companies(id),
    city_id       uuid NOT NULL REFERENCES cities(id),
    name          text NOT NULL,
    slug          text NOT NULL UNIQUE,
    address       text NOT NULL DEFAULT '',
    landmark      text NOT NULL DEFAULT '',
    latitude      double precision,
    longitude     double precision,
    phone         text NOT NULL DEFAULT '',
    whatsapp      text NOT NULL DEFAULT '',
    email         text NOT NULL DEFAULT '',
    opening_hours jsonb NOT NULL DEFAULT '[]'::jsonb,
    services      text[] NOT NULL DEFAULT ARRAY['ticketing']::text[],
    instructions  text NOT NULL DEFAULT '',
    photos        text[] NOT NULL DEFAULT '{}'::text[],
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (services <@ ARRAY['ticketing', 'parcel_drop', 'parcel_pickup', 'lounge']::text[])
);
CREATE INDEX agencies_company_idx ON agencies (company_id);
CREATE INDEX agencies_city_idx ON agencies (city_id);
CREATE TRIGGER agencies_updated_at BEFORE UPDATE ON agencies FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE agencies ENABLE ROW LEVEL SECURITY;
ALTER TABLE agencies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agencies USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Clients (comptes globaux : un client peut voyager avec plusieurs compagnies)
-- =====================================================================

CREATE TABLE customers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone         text NOT NULL UNIQUE,           -- format E.164
    full_name     text NOT NULL DEFAULT '',
    email         text,
    birth_date    date,
    referral_code text NOT NULL UNIQUE,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'blocked', 'deleted')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER customers_updated_at BEFORE UPDATE ON customers FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Proches / passagers fréquents
CREATE TABLE customer_passengers (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  uuid NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    full_name    text NOT NULL,
    phone        text NOT NULL DEFAULT '',
    relationship text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_passengers_customer_idx ON customer_passengers (customer_id);

-- =====================================================================
-- Personnel (back-office, agences, terrain)
-- =====================================================================

CREATE TABLE staff_users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid NOT NULL REFERENCES companies(id),
    agency_id      uuid REFERENCES agencies(id),
    email          text NOT NULL UNIQUE,
    password_hash  text NOT NULL,
    full_name      text NOT NULL,
    phone          text NOT NULL DEFAULT '',
    role           text NOT NULL CHECK (role IN (
                        'company_admin', 'agency_manager', 'cashier',
                        'boarding_agent', 'driver', 'parcel_agent', 'finance')),
    is_active      boolean NOT NULL DEFAULT true,
    last_login_at  timestamptz,
    failed_logins  integer NOT NULL DEFAULT 0,
    locked_until   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX staff_users_company_idx ON staff_users (company_id);
CREATE TRIGGER staff_users_updated_at BEFORE UPDATE ON staff_users FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE staff_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_users FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON staff_users USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Terminaux des agents (révocables à distance en cas de perte/vol)
CREATE TABLE devices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id   uuid NOT NULL REFERENCES companies(id),
    staff_id     uuid NOT NULL REFERENCES staff_users(id),
    name         text NOT NULL,
    platform     text NOT NULL DEFAULT 'android',
    is_active    boolean NOT NULL DEFAULT true,
    last_seen_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX devices_company_idx ON devices (company_id);
ALTER TABLE devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON devices USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Authentification
-- =====================================================================

CREATE TABLE otp_codes (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone        text NOT NULL,
    code_hash    text NOT NULL,
    attempts     integer NOT NULL DEFAULT 0,
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    requester_ip text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX otp_codes_phone_idx ON otp_codes (phone, created_at DESC);

CREATE TABLE refresh_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_type text NOT NULL CHECK (subject_type IN ('customer', 'staff')),
    subject_id   uuid NOT NULL,
    family_id    uuid NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    replaced_by  uuid,
    user_agent   text NOT NULL DEFAULT '',
    ip           text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_subject_idx ON refresh_tokens (subject_type, subject_id);

-- =====================================================================
-- Journal d'audit (ajout seul)
-- =====================================================================

CREATE TABLE audit_logs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id  uuid,
    actor_type  text NOT NULL CHECK (actor_type IN ('customer', 'staff', 'system')),
    actor_id    uuid,
    action      text NOT NULL,
    entity_type text NOT NULL,
    entity_id   text NOT NULL,
    before      jsonb,
    after       jsonb,
    ip          text NOT NULL DEFAULT '',
    user_agent  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_company_idx ON audit_logs (company_id, created_at DESC);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity_type, entity_id);
CREATE TRIGGER audit_logs_append_only BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_logs
    USING (company_id IS NULL AND app_scope() = 'system' OR tenant_visible(company_id))
    WITH CHECK (company_id IS NULL OR tenant_visible(company_id));

-- +goose Down
DROP TABLE audit_logs;
DROP TABLE refresh_tokens;
DROP TABLE otp_codes;
DROP TABLE devices;
DROP TABLE staff_users;
DROP TABLE customer_passengers;
DROP TABLE customers;
DROP TABLE agencies;
DROP TABLE cities;
DROP TABLE companies;
DROP FUNCTION forbid_mutation();
DROP FUNCTION set_updated_at();
DROP FUNCTION tenant_visible(uuid);
DROP FUNCTION app_company_id();
DROP FUNCTION app_scope();

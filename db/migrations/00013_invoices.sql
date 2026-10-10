-- +goose Up
-- Factures internes : une facture par encaissement, une facture d'avoir par remboursement versé.
-- Les colonnes emecef_* accueilleront la certification DGI (facture normalisée, phase 2).

-- Mentions légales du vendeur (figées sur chaque facture à l'émission).
ALTER TABLE companies
    ADD COLUMN tax_id          text,                          -- IFU
    ADD COLUMN trade_register  text,                          -- RCCM
    ADD COLUMN address         text,
    ADD COLUMN invoice_prefix  text NOT NULL DEFAULT 'FAC' CHECK (invoice_prefix ~ '^[A-Z0-9]{2,8}$'),
    ADD COLUMN invoice_footer  text NOT NULL DEFAULT '';

-- Facture au nom d'une entreprise (facultatif, saisi à la réservation).
ALTER TABLE bookings
    ADD COLUMN billing_name   text,
    ADD COLUMN billing_tax_id text;

-- Numérotation continue, sans trou, par compagnie, type et année.
CREATE TABLE invoice_counters (
    company_id uuid NOT NULL REFERENCES companies(id),
    kind       text NOT NULL CHECK (kind IN ('invoice', 'credit_note')),
    year       integer NOT NULL,
    last_value integer NOT NULL DEFAULT 0,
    PRIMARY KEY (company_id, kind, year)
);
ALTER TABLE invoice_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_counters USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE invoices (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id          uuid NOT NULL REFERENCES companies(id),
    kind                text NOT NULL CHECK (kind IN ('invoice', 'credit_note')),
    number              text NOT NULL,
    issued_at           timestamptz NOT NULL DEFAULT now(),
    -- Origine
    payment_id          uuid UNIQUE REFERENCES payments(id),
    refund_id           uuid UNIQUE REFERENCES refunds(id),
    booking_id          uuid REFERENCES bookings(id),
    parcel_id           uuid REFERENCES parcels(id),
    original_invoice_id uuid REFERENCES invoices(id),
    customer_id         uuid REFERENCES customers(id),
    -- Vendeur (copie à l'émission)
    seller_name         text NOT NULL,
    seller_legal_name   text NOT NULL DEFAULT '',
    seller_tax_id       text NOT NULL DEFAULT '',
    seller_trade_register text NOT NULL DEFAULT '',
    seller_address      text NOT NULL DEFAULT '',
    seller_phone        text NOT NULL DEFAULT '',
    seller_email        text NOT NULL DEFAULT '',
    footer              text NOT NULL DEFAULT '',
    -- Client (copie à l'émission)
    customer_name       text NOT NULL,
    customer_phone      text NOT NULL DEFAULT '',
    customer_email      text NOT NULL DEFAULT '',
    customer_company    text NOT NULL DEFAULT '',
    customer_tax_id     text NOT NULL DEFAULT '',
    -- Contexte de la vente
    agency_name         text NOT NULL DEFAULT '',
    operator_name       text NOT NULL DEFAULT '',
    payment_method      text NOT NULL DEFAULT '',
    reference           text NOT NULL DEFAULT '',   -- réservation BV… ou colis CL…
    currency            text NOT NULL DEFAULT 'XOF',
    total_amount        bigint NOT NULL CHECK (total_amount >= 0),
    -- Facture normalisée DGI (e-MECeF) — phase 2
    emecef_status       text NOT NULL DEFAULT 'none' CHECK (emecef_status IN ('none', 'pending', 'certified', 'failed')),
    emecef_code         text,
    emecef_qr           text,
    emecef_nim          text,
    emecef_counters     text,
    emecef_certified_at timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, number),
    CHECK ((kind = 'invoice' AND refund_id IS NULL) OR (kind = 'credit_note' AND refund_id IS NOT NULL AND payment_id IS NULL))
);
CREATE INDEX invoices_company_issued_idx ON invoices (company_id, issued_at DESC);
CREATE INDEX invoices_booking_idx ON invoices (booking_id) WHERE booking_id IS NOT NULL;
CREATE INDEX invoices_parcel_idx ON invoices (parcel_id) WHERE parcel_id IS NOT NULL;
CREATE INDEX invoices_customer_idx ON invoices (customer_id, issued_at DESC) WHERE customer_id IS NOT NULL;
ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoices USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE invoice_lines (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    invoice_id  uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    position    integer NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('ticket', 'fare_difference', 'change_fee', 'loyalty', 'parcel', 'baggage', 'refund')),
    label       text NOT NULL,
    detail      text NOT NULL DEFAULT '',
    quantity    integer NOT NULL DEFAULT 1 CHECK (quantity > 0),
    unit_price  bigint NOT NULL,
    amount      bigint NOT NULL,          -- négatif pour une remise (points de fidélité)
    tax_group   text,                     -- groupe de taxe e-MECeF (phase 2)
    UNIQUE (invoice_id, position)
);
ALTER TABLE invoice_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invoice_lines USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose Down
DROP TABLE invoice_lines;
DROP TABLE invoices;
DROP TABLE invoice_counters;
ALTER TABLE bookings DROP COLUMN billing_tax_id, DROP COLUMN billing_name;
ALTER TABLE companies DROP COLUMN invoice_footer, DROP COLUMN invoice_prefix, DROP COLUMN address,
    DROP COLUMN trade_register, DROP COLUMN tax_id;

-- +goose Up

-- =====================================================================
-- Réservations
-- =====================================================================

CREATE TABLE bookings (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id            uuid NOT NULL REFERENCES companies(id),
    reference             text NOT NULL UNIQUE,
    departure_id          uuid NOT NULL REFERENCES departures(id),
    customer_id           uuid REFERENCES customers(id),
    channel               text NOT NULL CHECK (channel IN ('app', 'web', 'counter')),
    agency_id             uuid REFERENCES agencies(id),   -- vente au guichet
    created_by_staff_id   uuid REFERENCES staff_users(id),
    status                text NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'confirmed', 'cancelled', 'expired', 'refunded')),
    contact_name          text NOT NULL,
    contact_phone         text NOT NULL,
    contact_email         text,
    subtotal_amount       bigint NOT NULL CHECK (subtotal_amount >= 0),
    discount_amount       bigint NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
    loyalty_points_used   integer NOT NULL DEFAULT 0 CHECK (loyalty_points_used >= 0),
    loyalty_points_amount bigint NOT NULL DEFAULT 0 CHECK (loyalty_points_amount >= 0),
    total_amount          bigint NOT NULL CHECK (total_amount >= 0),
    currency              text NOT NULL DEFAULT 'XOF',
    expires_at            timestamptz NOT NULL,
    confirmed_at          timestamptz,
    cancelled_at          timestamptz,
    cancel_reason         text NOT NULL DEFAULT '',
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bookings_customer_idx ON bookings (customer_id, created_at DESC);
CREATE INDEX bookings_departure_idx ON bookings (departure_id);
CREATE INDEX bookings_company_created_idx ON bookings (company_id, created_at DESC);
CREATE INDEX bookings_pending_expiry_idx ON bookings (expires_at) WHERE status = 'pending';
CREATE TRIGGER bookings_updated_at BEFORE UPDATE ON bookings FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE bookings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bookings USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Une ligne par passager / siège.
CREATE TABLE booking_seats (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id         uuid NOT NULL REFERENCES companies(id),
    booking_id         uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    departure_id       uuid NOT NULL REFERENCES departures(id),
    seat_cell_id       uuid NOT NULL REFERENCES seat_layout_cells(id),
    seat_label         text NOT NULL,
    category_id        uuid NOT NULL REFERENCES seat_categories(id),
    passenger_name     text NOT NULL,
    passenger_phone    text NOT NULL DEFAULT '',
    base_price         bigint NOT NULL,
    category_surcharge bigint NOT NULL DEFAULT 0,
    seat_surcharge     bigint NOT NULL DEFAULT 0,
    discount_amount    bigint NOT NULL DEFAULT 0,
    total_price        bigint NOT NULL CHECK (total_price >= 0),
    status             text NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'sold', 'cancelled', 'expired')),
    hold_expires_at    timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
-- GARANTIE ANTI-DOUBLE-VENTE : un siège ne peut être actif qu'une seule fois par départ,
-- quel que soit le canal (mobile, web, guichet). Contrôlé par la base, pas par le code.
CREATE UNIQUE INDEX booking_seats_active_seat_uq
    ON booking_seats (departure_id, seat_cell_id)
    WHERE status IN ('held', 'sold');
CREATE INDEX booking_seats_booking_idx ON booking_seats (booking_id);
CREATE INDEX booking_seats_held_expiry_idx ON booking_seats (departure_id, hold_expires_at) WHERE status = 'held';
CREATE TRIGGER booking_seats_updated_at BEFORE UPDATE ON booking_seats FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE booking_seats ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_seats FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON booking_seats USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Billets (un par siège vendu) — QR signé Ed25519
-- =====================================================================

CREATE TABLE tickets (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    booking_id      uuid NOT NULL REFERENCES bookings(id),
    booking_seat_id uuid NOT NULL UNIQUE REFERENCES booking_seats(id),
    departure_id    uuid NOT NULL REFERENCES departures(id),
    ticket_number   text NOT NULL UNIQUE,
    qr_payload      text NOT NULL,
    status          text NOT NULL DEFAULT 'valid' CHECK (status IN ('valid', 'used', 'cancelled')),
    boarded_at      timestamptz,
    boarded_by      uuid REFERENCES staff_users(id),
    issued_at       timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tickets_booking_idx ON tickets (booking_id);
CREATE INDEX tickets_departure_idx ON tickets (departure_id);
CREATE TRIGGER tickets_updated_at BEFORE UPDATE ON tickets FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE tickets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tickets USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Paiements, événements fournisseurs, remboursements
-- =====================================================================

CREATE TABLE payments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id          uuid NOT NULL REFERENCES companies(id),
    booking_id          uuid NOT NULL REFERENCES bookings(id),
    provider            text NOT NULL CHECK (provider IN ('fedapay', 'kkiapay', 'cinetpay', 'cash', 'mock')),
    method              text NOT NULL CHECK (method IN ('mobile_money', 'card', 'cash', 'wallet')),
    amount              bigint NOT NULL CHECK (amount > 0),
    currency            text NOT NULL DEFAULT 'XOF',
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'succeeded', 'failed', 'cancelled', 'refunded')),
    provider_reference  text,
    checkout_url        text,
    payer_phone         text NOT NULL DEFAULT '',
    idempotency_key     text NOT NULL UNIQUE,
    agency_id           uuid REFERENCES agencies(id),
    received_by_staff_id uuid REFERENCES staff_users(id),
    failure_reason      text NOT NULL DEFAULT '',
    succeeded_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payments_provider_ref_uq ON payments (provider, provider_reference) WHERE provider_reference IS NOT NULL;
CREATE INDEX payments_booking_idx ON payments (booking_id);
CREATE INDEX payments_company_created_idx ON payments (company_id, created_at DESC);
CREATE TRIGGER payments_updated_at BEFORE UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Journal brut des notifications fournisseurs (idempotence des webhooks).
CREATE TABLE payment_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider     text NOT NULL,
    event_id     text NOT NULL,
    payload      jsonb NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    UNIQUE (provider, event_id)
);

CREATE TABLE refunds (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id   uuid NOT NULL REFERENCES companies(id),
    booking_id   uuid NOT NULL REFERENCES bookings(id),
    payment_id   uuid REFERENCES payments(id),
    amount       bigint NOT NULL CHECK (amount > 0),
    reason       text NOT NULL,
    status       text NOT NULL DEFAULT 'requested'
                 CHECK (status IN ('requested', 'approved', 'rejected', 'processed')),
    requested_by_type text NOT NULL CHECK (requested_by_type IN ('customer', 'staff', 'system')),
    requested_by uuid,
    approved_by  uuid REFERENCES staff_users(id),
    decided_at   timestamptz,
    processed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refunds_company_status_idx ON refunds (company_id, status);
CREATE TRIGGER refunds_updated_at BEFORE UPDATE ON refunds FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE refunds ENABLE ROW LEVEL SECURITY;
ALTER TABLE refunds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON refunds USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Embarquement : chaque scan est conservé (en ligne ou synchronisé hors ligne)
-- =====================================================================

CREATE TABLE boarding_scans (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id   uuid NOT NULL REFERENCES companies(id),
    departure_id uuid NOT NULL REFERENCES departures(id),
    ticket_id    uuid REFERENCES tickets(id),
    staff_id     uuid NOT NULL REFERENCES staff_users(id),
    device_id    uuid REFERENCES devices(id),
    result       text NOT NULL CHECK (result IN (
                     'boarded', 'already_used', 'invalid_signature', 'wrong_departure',
                     'cancelled', 'not_found', 'conflict')),
    offline      boolean NOT NULL DEFAULT false,
    scanned_at   timestamptz NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    client_scan_id text
);
CREATE UNIQUE INDEX boarding_scans_client_uq ON boarding_scans (device_id, client_scan_id) WHERE client_scan_id IS NOT NULL;
CREATE INDEX boarding_scans_departure_idx ON boarding_scans (departure_id, scanned_at);
ALTER TABLE boarding_scans ENABLE ROW LEVEL SECURITY;
ALTER TABLE boarding_scans FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON boarding_scans USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose Down
DROP TABLE boarding_scans;
DROP TABLE refunds;
DROP TABLE payment_events;
DROP TABLE payments;
DROP TABLE tickets;
DROP TABLE booking_seats;
DROP TABLE bookings;

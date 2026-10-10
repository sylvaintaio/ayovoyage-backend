-- +goose Up
-- Module colis, bagages et réclamations.

-- Paiements : un paiement concerne une réservation ou un colis.
ALTER TABLE payments ALTER COLUMN booking_id DROP NOT NULL;
ALTER TABLE payments ADD COLUMN parcel_id uuid;
ALTER TABLE payments ADD CONSTRAINT payments_subject_ck CHECK ((booking_id IS NOT NULL) <> (parcel_id IS NOT NULL));

-- Excédent de bagage : prix par kilo au-delà du poids inclus sur la ligne.
ALTER TABLE companies ADD COLUMN baggage_extra_per_kg bigint NOT NULL DEFAULT 200 CHECK (baggage_extra_per_kg >= 0);

-- Tarifs colis par catégorie : prix de base + prix au kilo au-delà du poids inclus.
CREATE TABLE parcel_tariffs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    category        text NOT NULL CHECK (category IN ('document', 'small', 'medium', 'large', 'fragile')),
    label           text NOT NULL,
    base_price      bigint NOT NULL CHECK (base_price >= 0),
    included_kg     numeric(6,2) NOT NULL DEFAULT 0 CHECK (included_kg >= 0),
    price_per_kg    bigint NOT NULL DEFAULT 0 CHECK (price_per_kg >= 0),
    max_weight_kg   numeric(6,2) NOT NULL CHECK (max_weight_kg > 0),
    insurance_percent numeric(5,2) NOT NULL DEFAULT 0 CHECK (insurance_percent BETWEEN 0 AND 100), -- sur la valeur déclarée
    UNIQUE (company_id, category)
);
ALTER TABLE parcel_tariffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE parcel_tariffs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON parcel_tariffs USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE parcels (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id            uuid NOT NULL REFERENCES companies(id),
    tracking_number       text NOT NULL UNIQUE,
    status                text NOT NULL DEFAULT 'accepted' CHECK (status IN (
                              'accepted',        -- déposé et vérifié en agence
                              'assigned',        -- affecté à un départ
                              'loaded',          -- chargé dans le bus
                              'in_transit',      -- bus en route
                              'arrived',         -- arrivé dans une agence de correspondance
                              'ready',           -- disponible au retrait (destinataire prévenu)
                              'delivered',       -- remis au destinataire
                              'returned',        -- rendu à l'expéditeur
                              'cancelled')),
    sender_name           text NOT NULL,
    sender_phone          text NOT NULL,
    sender_email          text,
    sender_customer_id    uuid REFERENCES customers(id),
    recipient_name        text NOT NULL,
    recipient_phone       text NOT NULL,
    recipient_email       text,
    recipient_customer_id uuid REFERENCES customers(id),
    origin_agency_id      uuid NOT NULL REFERENCES agencies(id),
    destination_agency_id uuid NOT NULL REFERENCES agencies(id),
    current_agency_id     uuid REFERENCES agencies(id),     -- NULL pendant le transport
    departure_id          uuid REFERENCES departures(id),  -- départ actuel ou prévu
    category              text NOT NULL,
    description           text NOT NULL,
    weight_kg             numeric(6,2) NOT NULL CHECK (weight_kg > 0),
    declared_value        bigint NOT NULL DEFAULT 0 CHECK (declared_value >= 0),
    pieces                integer NOT NULL DEFAULT 1 CHECK (pieces BETWEEN 1 AND 50),
    price                 bigint NOT NULL CHECK (price >= 0),
    currency              text NOT NULL DEFAULT 'XOF',
    payer                 text NOT NULL CHECK (payer IN ('sender', 'recipient')), -- port payé / port dû
    paid_at               timestamptz,
    photo                 text,                                -- photo au dépôt (data URL JPEG)
    pickup_code_hash      text,
    pickup_code_sent_at   timestamptz,
    delivered_at          timestamptz,
    delivered_by          uuid REFERENCES staff_users(id),
    delivered_to_name     text,
    delivery_proof        text,                                -- 'otp' ou 'identity'
    delivery_identity     text,                                -- pièce présentée si pas de code
    signature             text,                                -- signature (data URL PNG)
    created_by            uuid REFERENCES staff_users(id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (origin_agency_id <> destination_agency_id)
);
CREATE INDEX parcels_company_created_idx ON parcels (company_id, created_at DESC);
CREATE INDEX parcels_departure_idx ON parcels (departure_id) WHERE departure_id IS NOT NULL;
CREATE INDEX parcels_current_agency_idx ON parcels (current_agency_id, status);
CREATE INDEX parcels_sender_phone_idx ON parcels (sender_phone);
CREATE INDEX parcels_recipient_phone_idx ON parcels (recipient_phone);
CREATE TRIGGER parcels_updated_at BEFORE UPDATE ON parcels FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE parcels ENABLE ROW LEVEL SECURITY;
ALTER TABLE parcels FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON parcels USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));
ALTER TABLE payments ADD CONSTRAINT payments_parcel_id_fkey FOREIGN KEY (parcel_id) REFERENCES parcels(id);

-- Historique (append-only) : chaque étape, transfert et correspondance.
CREATE TABLE parcel_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id   uuid NOT NULL REFERENCES companies(id),
    parcel_id    uuid NOT NULL REFERENCES parcels(id),
    event_type   text NOT NULL,      -- created, accepted, assigned, unassigned, loaded, departed, arrived, unloaded, ready, notified, delivered, returned, cancelled, incident
    status_after text NOT NULL,
    agency_id    uuid REFERENCES agencies(id),
    departure_id uuid REFERENCES departures(id),
    staff_id     uuid REFERENCES staff_users(id),
    note         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX parcel_events_parcel_idx ON parcel_events (parcel_id, id);
ALTER TABLE parcel_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE parcel_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON parcel_events USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Bagages des voyageurs : étiquette QR liée au passager, à la réservation et au départ.
CREATE TABLE baggage_tags (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    tag_number      text NOT NULL UNIQUE,
    booking_id      uuid NOT NULL REFERENCES bookings(id),
    booking_seat_id uuid NOT NULL REFERENCES booking_seats(id),
    departure_id    uuid NOT NULL REFERENCES departures(id),
    weight_kg       numeric(6,2) NOT NULL CHECK (weight_kg > 0),
    description     text NOT NULL DEFAULT '',
    extra_fee       bigint NOT NULL DEFAULT 0 CHECK (extra_fee >= 0),
    status          text NOT NULL DEFAULT 'checked' CHECK (status IN ('checked', 'loaded', 'unloaded', 'delivered', 'missing', 'cancelled')),
    loaded_at       timestamptz,
    unloaded_at     timestamptz,
    delivered_at    timestamptz,
    created_by      uuid REFERENCES staff_users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX baggage_tags_departure_idx ON baggage_tags (departure_id);
CREATE INDEX baggage_tags_booking_idx ON baggage_tags (booking_id);
CREATE TRIGGER baggage_tags_updated_at BEFORE UPDATE ON baggage_tags FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE baggage_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE baggage_tags FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON baggage_tags USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Réclamations (colis, bagages, voyages) avec dossier de suivi.
CREATE TABLE incidents (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid NOT NULL REFERENCES companies(id),
    reference      text NOT NULL UNIQUE,
    subject_type   text NOT NULL CHECK (subject_type IN ('parcel', 'baggage', 'booking')),
    parcel_id      uuid REFERENCES parcels(id),
    baggage_id     uuid REFERENCES baggage_tags(id),
    booking_id     uuid REFERENCES bookings(id),
    category       text NOT NULL CHECK (category IN ('not_received', 'delayed', 'damaged', 'opened', 'wrong_recipient',
                                                     'wrong_agency', 'info_error', 'lost', 'other')),
    description    text NOT NULL,
    reporter_type  text NOT NULL CHECK (reporter_type IN ('customer', 'staff', 'public')),
    reporter_name  text NOT NULL,
    reporter_phone text NOT NULL,
    reporter_customer_id uuid REFERENCES customers(id),
    priority       text NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
    status         text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'in_progress', 'resolved', 'rejected')),
    assigned_to    uuid REFERENCES staff_users(id),
    resolution     text NOT NULL DEFAULT '',
    resolved_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((subject_type = 'parcel' AND parcel_id IS NOT NULL) OR (subject_type = 'baggage' AND baggage_id IS NOT NULL)
        OR (subject_type = 'booking' AND booking_id IS NOT NULL))
);
CREATE INDEX incidents_company_status_idx ON incidents (company_id, status, created_at DESC);
CREATE TRIGGER incidents_updated_at BEFORE UPDATE ON incidents FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON incidents USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE incident_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    incident_id uuid NOT NULL REFERENCES incidents(id),
    author_type text NOT NULL CHECK (author_type IN ('customer', 'staff', 'public', 'system')),
    author_id   uuid,
    author_name text NOT NULL DEFAULT '',
    action      text NOT NULL,   -- created, comment, status, priority, assigned
    note        text NOT NULL DEFAULT '',
    is_public   boolean NOT NULL DEFAULT true, -- visible par le client
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX incident_events_incident_idx ON incident_events (incident_id, id);
ALTER TABLE incident_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE incident_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON incident_events USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Photos jointes (dépôt de colis, réclamation) : images JPEG/PNG compressées par le client, 2 Mo au plus.
CREATE TABLE attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id   uuid NOT NULL REFERENCES companies(id),
    incident_id  uuid REFERENCES incidents(id),
    content_type text NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/webp')),
    data         bytea NOT NULL CHECK (octet_length(data) <= 2097152),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_incident_idx ON attachments (incident_id);
ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON attachments USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Tarifs par défaut pour les compagnies existantes.
SELECT set_config('app.scope', 'system', true);
INSERT INTO parcel_tariffs (company_id, category, label, base_price, included_kg, price_per_kg, max_weight_kg, insurance_percent)
SELECT c.id, t.category, t.label, t.base_price, t.included_kg, t.price_per_kg, t.max_weight_kg, 1
FROM companies c
CROSS JOIN (VALUES
    ('document', 'Document / enveloppe', 1500, 1, 0, 1),
    ('small',    'Petit colis',          2500, 5, 300, 10),
    ('medium',   'Colis moyen',          4000, 15, 250, 30),
    ('large',    'Gros colis',           7000, 30, 200, 80),
    ('fragile',  'Fragile',              5000, 10, 400, 30)
) AS t(category, label, base_price, included_kg, price_per_kg, max_weight_kg);

-- +goose Down
DROP TABLE attachments;
DROP TABLE incident_events;
DROP TABLE incidents;
DROP TABLE baggage_tags;
ALTER TABLE payments DROP CONSTRAINT payments_parcel_id_fkey;
DROP TABLE parcel_events;
DROP TABLE parcels;
DROP TABLE parcel_tariffs;
ALTER TABLE payments DROP CONSTRAINT payments_subject_ck;
ALTER TABLE payments DROP COLUMN parcel_id;
DELETE FROM payments WHERE booking_id IS NULL;
ALTER TABLE payments ALTER COLUMN booking_id SET NOT NULL;
ALTER TABLE companies DROP COLUMN baggage_extra_per_kg;

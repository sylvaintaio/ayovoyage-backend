-- +goose Up

-- =====================================================================
-- Catégories de sièges (Standard, Premium, VIP…)
-- =====================================================================

CREATE TABLE seat_categories (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id       uuid NOT NULL REFERENCES companies(id),
    code             text NOT NULL,
    name             text NOT NULL,
    description      text NOT NULL DEFAULT '',
    surcharge_amount bigint NOT NULL DEFAULT 0 CHECK (surcharge_amount >= 0),
    color            text NOT NULL DEFAULT '#64748b',
    perks            text[] NOT NULL DEFAULT '{}'::text[],
    sort_order       integer NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code)
);
CREATE TRIGGER seat_categories_updated_at BEFORE UPDATE ON seat_categories FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE seat_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_categories FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON seat_categories USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Plans de sièges : une grille (rangées × colonnes) dont chaque case est
-- un siège, une allée, le poste chauffeur, une porte…
-- Un plan utilisé par un départ ne peut plus être supprimé (FK RESTRICT) :
-- pour le modifier structurellement, on en crée une nouvelle version.
-- =====================================================================

CREATE TABLE seat_layouts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id uuid NOT NULL REFERENCES companies(id),
    name       text NOT NULL,
    rows       integer NOT NULL CHECK (rows BETWEEN 1 AND 40),
    cols       integer NOT NULL CHECK (cols BETWEEN 1 AND 8),
    seat_count integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER seat_layouts_updated_at BEFORE UPDATE ON seat_layouts FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE seat_layouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_layouts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON seat_layouts USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE seat_layout_cells (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    layout_id       uuid NOT NULL REFERENCES seat_layouts(id) ON DELETE CASCADE,
    row_index       integer NOT NULL CHECK (row_index >= 0),
    col_index       integer NOT NULL CHECK (col_index >= 0),
    kind            text NOT NULL CHECK (kind IN ('seat', 'aisle', 'driver', 'door', 'toilet', 'empty')),
    seat_label      text,
    category_id     uuid REFERENCES seat_categories(id),
    extra_surcharge bigint NOT NULL DEFAULT 0 CHECK (extra_surcharge >= 0),
    attributes      text[] NOT NULL DEFAULT '{}'::text[],
    UNIQUE (layout_id, row_index, col_index),
    CHECK (kind <> 'seat' OR (seat_label IS NOT NULL AND category_id IS NOT NULL)),
    CHECK (attributes <@ ARRAY['window', 'aisle', 'family', 'accessible', 'front', 'legroom']::text[])
);
CREATE UNIQUE INDEX seat_layout_cells_label_uq ON seat_layout_cells (layout_id, seat_label) WHERE kind = 'seat';
ALTER TABLE seat_layout_cells ENABLE ROW LEVEL SECURITY;
ALTER TABLE seat_layout_cells FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON seat_layout_cells USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Bus
-- =====================================================================

CREATE TABLE buses (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid NOT NULL REFERENCES companies(id),
    plate_number   text NOT NULL,
    internal_code  text NOT NULL DEFAULT '',
    model          text NOT NULL DEFAULT '',
    layout_id      uuid NOT NULL REFERENCES seat_layouts(id),
    amenities      text[] NOT NULL DEFAULT '{}'::text[],
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'maintenance', 'out_of_service')),
    home_agency_id uuid REFERENCES agencies(id),
    notes          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, plate_number)
);
CREATE TRIGGER buses_updated_at BEFORE UPDATE ON buses FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE buses ENABLE ROW LEVEL SECURITY;
ALTER TABLE buses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON buses USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Lignes (trajets) et départs
-- =====================================================================

CREATE TABLE routes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id            uuid NOT NULL REFERENCES companies(id),
    origin_agency_id      uuid NOT NULL REFERENCES agencies(id),
    destination_agency_id uuid NOT NULL REFERENCES agencies(id),
    name                  text NOT NULL,
    base_price            bigint NOT NULL CHECK (base_price > 0),
    duration_minutes      integer NOT NULL CHECK (duration_minutes > 0),
    distance_km           integer,
    included_baggage_kg   integer NOT NULL DEFAULT 20,
    is_active             boolean NOT NULL DEFAULT true,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (origin_agency_id <> destination_agency_id)
);
CREATE INDEX routes_company_idx ON routes (company_id);
CREATE TRIGGER routes_updated_at BEFORE UPDATE ON routes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE routes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON routes USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE departures (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id   uuid NOT NULL REFERENCES companies(id),
    route_id     uuid NOT NULL REFERENCES routes(id),
    bus_id       uuid NOT NULL REFERENCES buses(id),
    layout_id    uuid NOT NULL REFERENCES seat_layouts(id), -- figé à la création
    departure_at timestamptz NOT NULL,
    arrival_eta  timestamptz NOT NULL,
    base_price   bigint NOT NULL CHECK (base_price > 0),
    status       text NOT NULL DEFAULT 'scheduled'
                 CHECK (status IN ('scheduled', 'boarding', 'departed', 'arrived', 'cancelled')),
    driver_id    uuid REFERENCES staff_users(id),
    agent_id     uuid REFERENCES staff_users(id),
    notes        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (arrival_eta > departure_at)
);
CREATE INDEX departures_company_date_idx ON departures (company_id, departure_at);
CREATE INDEX departures_route_date_idx ON departures (route_id, departure_at);
CREATE INDEX departures_bus_idx ON departures (bus_id, departure_at);
CREATE TRIGGER departures_updated_at BEFORE UPDATE ON departures FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE departures ENABLE ROW LEVEL SECURITY;
ALTER TABLE departures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON departures USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Sièges bloqués par la compagnie sur un départ (réserve, panne, personnel…)
CREATE TABLE departure_blocked_seats (
    departure_id uuid NOT NULL REFERENCES departures(id) ON DELETE CASCADE,
    seat_cell_id uuid NOT NULL REFERENCES seat_layout_cells(id),
    company_id   uuid NOT NULL REFERENCES companies(id),
    reason       text NOT NULL DEFAULT '',
    blocked_by   uuid REFERENCES staff_users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (departure_id, seat_cell_id)
);
ALTER TABLE departure_blocked_seats ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_blocked_seats FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON departure_blocked_seats USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose Down
DROP TABLE departure_blocked_seats;
DROP TABLE departures;
DROP TABLE routes;
DROP TABLE buses;
DROP TABLE seat_layout_cells;
DROP TABLE seat_layouts;
DROP TABLE seat_categories;

-- +goose Up
-- Lignes à arrêts, places par tronçon, équipage et journal des arrêts
-- (voir docs/lignes-arrets-equipage.md). Les données existantes sont converties :
-- chaque ligne devient « agence de départ → agence d'arrivée » (2 arrêts).
SELECT set_config('app.scope', 'system', true);

-- =====================================================================
-- Points d'arrêt : catalogue de la compagnie (agence ou simple lieu)
-- =====================================================================
CREATE TABLE stop_points (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id uuid NOT NULL REFERENCES companies(id),
    city_id    uuid NOT NULL REFERENCES cities(id),
    agency_id  uuid REFERENCES agencies(id),           -- point d'arrêt situé à une agence (montée possible)
    name       text NOT NULL CHECK (length(trim(name)) BETWEEN 2 AND 80),
    address    text NOT NULL DEFAULT '',
    latitude   double precision CHECK (latitude BETWEEN -90 AND 90),
    longitude  double precision CHECK (longitude BETWEEN -180 AND 180),
    is_active  boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, city_id, name)
);
CREATE UNIQUE INDEX stop_points_agency_uq ON stop_points (agency_id) WHERE agency_id IS NOT NULL;
CREATE INDEX stop_points_company_idx ON stop_points (company_id, city_id);
CREATE TRIGGER stop_points_updated_at BEFORE UPDATE ON stop_points FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE stop_points ENABLE ROW LEVEL SECURITY;
ALTER TABLE stop_points FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON stop_points USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Un point d'arrêt par agence existante.
-- Deux agences homonymes dans la même ville : la seconde prend son identifiant en suffixe.
INSERT INTO stop_points (company_id, city_id, agency_id, name, address, latitude, longitude, is_active)
SELECT company_id, city_id, id,
       CASE WHEN n = 1 THEN left(name, 80) ELSE left(name, 60) || ' (' || left(slug, 17) || ')' END,
       address, latitude, longitude, is_active
FROM (SELECT a.*, row_number() OVER (PARTITION BY a.company_id, a.city_id, lower(trim(a.name)) ORDER BY a.created_at) AS n
      FROM agencies a) x;

-- =====================================================================
-- Lignes : suite ordonnée de points d'arrêt, prix unique ou grille
-- =====================================================================
ALTER TABLE routes ADD COLUMN pricing_mode text NOT NULL DEFAULT 'flat' CHECK (pricing_mode IN ('flat', 'grid'));

CREATE TABLE route_stops (
    route_id       uuid NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    company_id     uuid NOT NULL REFERENCES companies(id),
    seq            integer NOT NULL CHECK (seq >= 0),
    stop_point_id  uuid NOT NULL REFERENCES stop_points(id),
    offset_minutes integer NOT NULL CHECK (offset_minutes >= 0),     -- depuis l'heure de départ
    pause_minutes  integer NOT NULL DEFAULT 0 CHECK (pause_minutes BETWEEN 0 AND 180),
    can_board      boolean NOT NULL,
    can_alight     boolean NOT NULL,
    PRIMARY KEY (route_id, seq),
    UNIQUE (route_id, stop_point_id)
);
ALTER TABLE route_stops ENABLE ROW LEVEL SECURITY;
ALTER TABLE route_stops FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON route_stops USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

INSERT INTO route_stops (route_id, company_id, seq, stop_point_id, offset_minutes, can_board, can_alight)
SELECT r.id, r.company_id, 0, sp.id, 0, true, false
FROM routes r JOIN stop_points sp ON sp.agency_id = r.origin_agency_id;
INSERT INTO route_stops (route_id, company_id, seq, stop_point_id, offset_minutes, can_board, can_alight)
SELECT r.id, r.company_id, 1, sp.id, r.duration_minutes, false, true
FROM routes r JOIN stop_points sp ON sp.agency_id = r.destination_agency_id;

-- Extrémités d'une ligne, déduites de ses arrêts. L'agence d'arrivée devient facultative :
-- une ligne peut finir à un point d'arrêt qui n'est pas une agence (gare routière).
ALTER TABLE routes ALTER COLUMN destination_agency_id DROP NOT NULL;

CREATE VIEW route_terminals WITH (security_invoker = true) AS
SELECT r.id AS route_id,
       fsp.id AS origin_stop_id, fsp.name AS origin_stop_name, fsp.city_id AS origin_city_id,
       lsp.id AS destination_stop_id, lsp.name AS destination_stop_name, lsp.city_id AS destination_city_id,
       l.offset_minutes AS total_minutes
FROM routes r
JOIN route_stops f ON f.route_id = r.id AND f.seq = 0
JOIN route_stops l ON l.route_id = r.id AND l.seq = (SELECT max(x.seq) FROM route_stops x WHERE x.route_id = r.id)
JOIN stop_points fsp ON fsp.id = f.stop_point_id
JOIN stop_points lsp ON lsp.id = l.stop_point_id;

-- Grille de prix de ville à ville (mode « grid ») ; le prix complet reste routes.base_price.
CREATE TABLE route_fares (
    route_id     uuid NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    company_id   uuid NOT NULL REFERENCES companies(id),
    from_city_id uuid NOT NULL REFERENCES cities(id),
    to_city_id   uuid NOT NULL REFERENCES cities(id),
    price        bigint NOT NULL CHECK (price > 0),
    PRIMARY KEY (route_id, from_city_id, to_city_id),
    CHECK (from_city_id <> to_city_id)
);
ALTER TABLE route_fares ENABLE ROW LEVEL SECURITY;
ALTER TABLE route_fares FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON route_fares USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Arrêts de chaque départ : copie figée de la ligne à la création du départ
-- (modifier une ligne ne déplace pas les billets déjà vendus)
-- =====================================================================
CREATE TABLE departure_stops (
    departure_id  uuid NOT NULL REFERENCES departures(id) ON DELETE CASCADE,
    company_id    uuid NOT NULL REFERENCES companies(id),
    seq           integer NOT NULL CHECK (seq >= 0),
    stop_point_id uuid NOT NULL REFERENCES stop_points(id),
    scheduled_at  timestamptz NOT NULL,
    pause_minutes integer NOT NULL DEFAULT 0,
    can_board     boolean NOT NULL,
    can_alight    boolean NOT NULL,
    PRIMARY KEY (departure_id, seq)
);
CREATE INDEX departure_stops_point_idx ON departure_stops (stop_point_id, scheduled_at);
ALTER TABLE departure_stops ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_stops FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON departure_stops USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

INSERT INTO departure_stops (departure_id, company_id, seq, stop_point_id, scheduled_at, pause_minutes, can_board, can_alight)
SELECT d.id, d.company_id, rs.seq, rs.stop_point_id,
       CASE WHEN rs.seq = 0 THEN d.departure_at ELSE d.arrival_eta END, 0, rs.can_board, rs.can_alight
FROM departures d JOIN route_stops rs ON rs.route_id = d.route_id;

-- =====================================================================
-- Réservations : arrêt de montée et de descente (indices dans departure_stops)
-- =====================================================================
ALTER TABLE bookings ADD COLUMN boarding_seq integer NOT NULL DEFAULT 0 CHECK (boarding_seq >= 0);
ALTER TABLE bookings ADD COLUMN alighting_seq integer NOT NULL DEFAULT 1;
ALTER TABLE bookings ADD CONSTRAINT bookings_segment_check CHECK (alighting_seq > boarding_seq);

ALTER TABLE booking_seats ADD COLUMN from_seq integer NOT NULL DEFAULT 0 CHECK (from_seq >= 0);
ALTER TABLE booking_seats ADD COLUMN to_seq integer NOT NULL DEFAULT 1;
ALTER TABLE booking_seats ADD CONSTRAINT booking_seats_segment_check CHECK (to_seq > from_seq);

-- =====================================================================
-- GARANTIE ANTI-DOUBLE-VENTE PAR TRONÇON
-- Une ligne par tronçon élémentaire (arrêt i → i+1) occupé par un siège actif
-- (réservé ou vendu). L'index unique interdit qu'un même siège soit actif deux fois
-- sur un même tronçon, quel que soit le canal. Tenu à jour par la base elle-même.
-- =====================================================================
CREATE TABLE booking_seat_segments (
    booking_seat_id uuid NOT NULL REFERENCES booking_seats(id) ON DELETE CASCADE,
    company_id      uuid NOT NULL REFERENCES companies(id),
    departure_id    uuid NOT NULL REFERENCES departures(id),
    seat_cell_id    uuid NOT NULL REFERENCES seat_layout_cells(id),
    segment         integer NOT NULL CHECK (segment >= 0),
    PRIMARY KEY (booking_seat_id, segment)
);
CREATE UNIQUE INDEX booking_seat_segments_uq ON booking_seat_segments (departure_id, seat_cell_id, segment);
ALTER TABLE booking_seat_segments ENABLE ROW LEVEL SECURITY;
ALTER TABLE booking_seat_segments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON booking_seat_segments USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose StatementBegin
CREATE FUNCTION sync_booking_seat_segments() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.status = OLD.status AND NEW.from_seq = OLD.from_seq AND NEW.to_seq = OLD.to_seq
           AND NEW.seat_cell_id = OLD.seat_cell_id THEN
            RETURN NEW;
        END IF;
        DELETE FROM booking_seat_segments WHERE booking_seat_id = NEW.id;
    END IF;
    IF NEW.status IN ('held', 'sold') THEN
        -- Violation d'unicité (23505) si un tronçon est déjà pris : l'appelant la traite
        -- comme aujourd'hui (« siège vient d'être pris », remboursement d'un paiement tardif).
        INSERT INTO booking_seat_segments (booking_seat_id, company_id, departure_id, seat_cell_id, segment)
        SELECT NEW.id, NEW.company_id, NEW.departure_id, NEW.seat_cell_id, s
        FROM generate_series(NEW.from_seq, NEW.to_seq - 1) AS s;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER booking_seats_segments AFTER INSERT OR UPDATE ON booking_seats
    FOR EACH ROW EXECUTE FUNCTION sync_booking_seat_segments();

INSERT INTO booking_seat_segments (booking_seat_id, company_id, departure_id, seat_cell_id, segment)
SELECT bs.id, bs.company_id, bs.departure_id, bs.seat_cell_id, s
FROM booking_seats bs, generate_series(bs.from_seq, bs.to_seq - 1) AS s
WHERE bs.status IN ('held', 'sold');

-- L'ancienne garantie (un siège actif pour tout le trajet) interdirait la revente après une descente.
DROP INDEX booking_seats_active_seat_uq;

-- =====================================================================
-- Équipage : contrôleur (departures.agent_id), chauffeur (departures.driver_id),
-- autres membres (second chauffeur, aide, convoyeur)
-- =====================================================================
CREATE TABLE departure_crew (
    departure_id uuid NOT NULL REFERENCES departures(id) ON DELETE CASCADE,
    company_id   uuid NOT NULL REFERENCES companies(id),
    staff_id     uuid NOT NULL REFERENCES staff_users(id),
    role         text NOT NULL CHECK (role IN ('driver', 'assistant')),
    PRIMARY KEY (departure_id, staff_id)
);
CREATE INDEX departure_crew_staff_idx ON departure_crew (staff_id);
ALTER TABLE departure_crew ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_crew FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON departure_crew USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- =====================================================================
-- Journal des arrêts : où et quand le car s'est arrêté (prévu ou imprévu)
-- =====================================================================
CREATE TABLE departure_stop_visits (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id        uuid NOT NULL REFERENCES companies(id),
    departure_id      uuid NOT NULL REFERENCES departures(id) ON DELETE CASCADE,
    seq               integer,                         -- arrêt prévu (departure_stops.seq) ; NULL = imprévu
    reason            text NOT NULL DEFAULT 'planned'
                      CHECK (reason IN ('planned', 'police', 'breakdown', 'puncture', 'pause', 'traffic', 'other')),
    note              text NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    arrived_at        timestamptz NOT NULL,
    departed_at       timestamptz,
    arrival_lat       double precision,
    arrival_lng       double precision,
    arrival_accuracy  real,                            -- précision du GPS en mètres
    departure_lat     double precision,
    departure_lng     double precision,
    location_mocked   boolean NOT NULL DEFAULT false,  -- position simulée signalée par le téléphone
    recorded_by       uuid NOT NULL REFERENCES staff_users(id),
    device_id         uuid REFERENCES devices(id),
    client_id         text,                            -- identifiant côté téléphone : envoi idempotent
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (departed_at IS NULL OR departed_at >= arrived_at),
    CHECK ((seq IS NULL) = (reason <> 'planned'))
);
CREATE UNIQUE INDEX departure_stop_visits_client_uq ON departure_stop_visits (device_id, client_id) WHERE client_id IS NOT NULL;
CREATE INDEX departure_stop_visits_departure_idx ON departure_stop_visits (departure_id, arrived_at);
CREATE TRIGGER departure_stop_visits_updated_at BEFORE UPDATE ON departure_stop_visits FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE departure_stop_visits ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_stop_visits FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON departure_stop_visits USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Scans : arrêt où le billet a été contrôlé (traçabilité).
ALTER TABLE boarding_scans ADD COLUMN stop_seq integer;

-- +goose Down
SELECT set_config('app.scope', 'system', true);
ALTER TABLE boarding_scans DROP COLUMN stop_seq;
DROP TABLE departure_stop_visits;
DROP TABLE departure_crew;
CREATE UNIQUE INDEX booking_seats_active_seat_uq ON booking_seats (departure_id, seat_cell_id) WHERE status IN ('held', 'sold');
DROP TRIGGER booking_seats_segments ON booking_seats;
DROP FUNCTION sync_booking_seat_segments();
DROP TABLE booking_seat_segments;
ALTER TABLE booking_seats DROP CONSTRAINT booking_seats_segment_check, DROP COLUMN to_seq, DROP COLUMN from_seq;
ALTER TABLE bookings DROP CONSTRAINT bookings_segment_check, DROP COLUMN alighting_seq, DROP COLUMN boarding_seq;
DROP TABLE departure_stops;
DROP TABLE route_fares;
DROP VIEW route_terminals;
DROP TABLE route_stops;
UPDATE routes SET destination_agency_id = origin_agency_id WHERE destination_agency_id IS NULL;
ALTER TABLE routes ALTER COLUMN destination_agency_id SET NOT NULL;
ALTER TABLE routes DROP COLUMN pricing_mode;
DROP TABLE stop_points;

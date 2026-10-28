-- +goose Up
-- Horaires permanents : « tous les jours à 07:00 avec tel bus ». Le serveur crée lui-même les départs
-- des semaines à venir ; chaque départ reste modifiable ou annulable un par un.
SELECT set_config('app.scope', 'system', true);

CREATE TABLE departure_schedules (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    route_id        uuid NOT NULL REFERENCES routes(id),
    bus_id          uuid NOT NULL REFERENCES buses(id),
    departure_time  text NOT NULL CHECK (departure_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'), -- HH:MM, heure du Bénin
    weekdays        smallint[] NOT NULL DEFAULT '{}',      -- 1 = lundi … 7 = dimanche ; vide = tous les jours
    base_price      bigint NOT NULL DEFAULT 0 CHECK (base_price >= 0), -- 0 : tarif de la ligne au moment de la création
    driver_id       uuid REFERENCES staff_users(id),
    agent_id        uuid REFERENCES staff_users(id),
    crew            jsonb NOT NULL DEFAULT '[]',           -- autres membres : [{staff_id, role}]
    notes           text NOT NULL DEFAULT '',
    starts_on       date NOT NULL,
    ends_on         date,                                  -- NULL : sans fin
    active          boolean NOT NULL DEFAULT true,
    -- Dates des semaines à venir non créées (bus ou équipage déjà pris…) : [{date, reason}].
    issues          jsonb NOT NULL DEFAULT '[]',
    generated_until date,
    created_by      uuid REFERENCES staff_users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_on IS NULL OR ends_on >= starts_on),
    CHECK (weekdays <@ ARRAY[1, 2, 3, 4, 5, 6, 7]::smallint[])
);
CREATE INDEX departure_schedules_company_idx ON departure_schedules (company_id, active);
CREATE TRIGGER departure_schedules_updated_at BEFORE UPDATE ON departure_schedules FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE departure_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON departure_schedules USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Départ créé par un horaire : une seule fois par jour de service (annulé à la main = jour sauté, pas recréé).
-- schedule_override : modifié à la main, les changements de l'horaire ne le touchent plus.
ALTER TABLE departures
    ADD COLUMN schedule_id       uuid REFERENCES departure_schedules(id) ON DELETE SET NULL,
    ADD COLUMN service_date      date,
    ADD COLUMN schedule_override boolean NOT NULL DEFAULT false,
    ADD CHECK ((schedule_id IS NULL) OR (service_date IS NOT NULL));
CREATE UNIQUE INDEX departures_schedule_date_uq ON departures (schedule_id, service_date) WHERE schedule_id IS NOT NULL;

-- +goose Down
SELECT set_config('app.scope', 'system', true);
DROP INDEX departures_schedule_date_uq;
ALTER TABLE departures DROP COLUMN schedule_override, DROP COLUMN service_date, DROP COLUMN schedule_id;
DROP TABLE departure_schedules;

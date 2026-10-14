-- +goose Up
-- Notifications push et boîte de réception (messages lus / non lus, compteur sur l'icône de l'app).
SELECT set_config('app.scope', 'system', true);

-- Téléphones inscrits aux notifications (jeton Firebase Cloud Messaging), par compte.
CREATE TABLE push_devices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token        text NOT NULL UNIQUE,
    platform     text NOT NULL CHECK (platform IN ('android', 'ios', 'web')),
    app          text NOT NULL CHECK (app IN ('voyageur', 'agent')),
    customer_id  uuid REFERENCES customers(id) ON DELETE CASCADE,
    staff_id     uuid REFERENCES staff_users(id) ON DELETE CASCADE,
    company_id   uuid REFERENCES companies(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((customer_id IS NULL) <> (staff_id IS NULL)),
    CHECK (staff_id IS NULL OR company_id IS NOT NULL)
);
CREATE INDEX push_devices_customer_idx ON push_devices (customer_id) WHERE customer_id IS NOT NULL;
CREATE INDEX push_devices_staff_idx ON push_devices (staff_id) WHERE staff_id IS NOT NULL;
ALTER TABLE push_devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE push_devices FORCE ROW LEVEL SECURITY;
-- Client : visible seulement dans une session client (ou système), jamais par le personnel d'une compagnie.
-- Personnel : cloisonné par compagnie.
CREATE POLICY owner_isolation ON push_devices
    USING ((customer_id IS NOT NULL AND app_scope() IN ('customer', 'system'))
           OR (staff_id IS NOT NULL AND tenant_visible(company_id)))
    WITH CHECK ((customer_id IS NOT NULL AND app_scope() IN ('customer', 'system'))
           OR (staff_id IS NOT NULL AND tenant_visible(company_id)));

-- Boîte de réception : chaque notification, lue ou non (le compteur de l'icône = non lues).
CREATE TABLE inbox_messages (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id uuid REFERENCES customers(id) ON DELETE CASCADE,
    staff_id    uuid REFERENCES staff_users(id) ON DELETE CASCADE,
    company_id  uuid REFERENCES companies(id),
    kind        text NOT NULL,
    title       text NOT NULL,
    body        text NOT NULL,
    link        text NOT NULL DEFAULT '',
    data        jsonb NOT NULL DEFAULT '{}',
    dedup_key   text,
    read_at     timestamptz,
    pushed_at   timestamptz,
    push_attempts integer NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((customer_id IS NULL) <> (staff_id IS NULL)),
    CHECK (staff_id IS NULL OR company_id IS NOT NULL)
);
CREATE INDEX inbox_customer_idx ON inbox_messages (customer_id, created_at DESC) WHERE customer_id IS NOT NULL;
CREATE INDEX inbox_staff_idx ON inbox_messages (staff_id, created_at DESC) WHERE staff_id IS NOT NULL;
CREATE INDEX inbox_unread_customer_idx ON inbox_messages (customer_id) WHERE read_at IS NULL AND customer_id IS NOT NULL;
CREATE INDEX inbox_unread_staff_idx ON inbox_messages (staff_id) WHERE read_at IS NULL AND staff_id IS NOT NULL;
-- Un même événement n'est notifié qu'une fois à une même personne (renvoi de tâche, double appel).
CREATE UNIQUE INDEX inbox_dedup_customer_uq ON inbox_messages (customer_id, dedup_key) WHERE dedup_key IS NOT NULL AND customer_id IS NOT NULL;
CREATE UNIQUE INDEX inbox_dedup_staff_uq ON inbox_messages (staff_id, dedup_key) WHERE dedup_key IS NOT NULL AND staff_id IS NOT NULL;
-- File d'envoi : messages pas encore poussés sur les téléphones.
CREATE INDEX inbox_to_push_idx ON inbox_messages (created_at) WHERE pushed_at IS NULL;
ALTER TABLE inbox_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbox_messages FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_isolation ON inbox_messages
    USING ((customer_id IS NOT NULL AND app_scope() IN ('customer', 'system'))
           OR (staff_id IS NOT NULL AND tenant_visible(company_id)))
    WITH CHECK ((customer_id IS NOT NULL AND app_scope() IN ('customer', 'system'))
           OR (staff_id IS NOT NULL AND tenant_visible(company_id)));

-- +goose Down
SELECT set_config('app.scope', 'system', true);
DROP TABLE inbox_messages;
DROP TABLE push_devices;

-- +goose Up
-- Avis des voyageurs : invitation après l'arrivée (lien personnel, sans connexion),
-- notes, modération, réponse publique de la compagnie.
SELECT set_config('app.scope', 'system', true);

CREATE TABLE review_invitations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id  uuid NOT NULL REFERENCES companies(id),
    booking_id  uuid NOT NULL UNIQUE REFERENCES bookings(id),
    token_hash  bytea NOT NULL UNIQUE,                  -- SHA-256 du jeton (le jeton n'est jamais stocké)
    sent_at     timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);
ALTER TABLE review_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE review_invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON review_invitations USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

CREATE TABLE reviews (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    booking_id      uuid NOT NULL UNIQUE REFERENCES bookings(id),
    departure_id    uuid NOT NULL REFERENCES departures(id),
    route_id        uuid NOT NULL REFERENCES routes(id),
    agency_id       uuid NOT NULL REFERENCES agencies(id),      -- agence de départ
    customer_id     uuid REFERENCES customers(id),
    rating          smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    punctuality     smallint CHECK (punctuality BETWEEN 1 AND 5),
    comfort         smallint CHECK (comfort BETWEEN 1 AND 5),
    staff           smallint CHECK (staff BETWEEN 1 AND 5),
    cleanliness     smallint CHECK (cleanliness BETWEEN 1 AND 5),
    comment         text NOT NULL DEFAULT '' CHECK (length(comment) <= 1000),
    display_name    text NOT NULL,                              -- prénom seulement
    city            text NOT NULL DEFAULT '',
    publish_consent boolean NOT NULL DEFAULT false,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'hidden')),
    hidden_reason   text NOT NULL DEFAULT '',
    reply           text NOT NULL DEFAULT '' CHECK (length(reply) <= 1000),
    replied_by      uuid REFERENCES staff_users(id),
    replied_at      timestamptz,
    moderated_by    uuid REFERENCES staff_users(id),
    moderated_at    timestamptz,
    incident_id     uuid REFERENCES incidents(id),              -- réclamation ouverte pour une mauvaise note
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (status <> 'published' OR publish_consent)
);
CREATE INDEX reviews_company_idx ON reviews (company_id, created_at DESC);
CREATE INDEX reviews_published_idx ON reviews (company_id, rating DESC, created_at DESC) WHERE status = 'published';
CREATE TRIGGER reviews_updated_at BEFORE UPDATE ON reviews FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE reviews FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON reviews USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- +goose Down
DROP TABLE reviews;
DROP TABLE review_invitations;

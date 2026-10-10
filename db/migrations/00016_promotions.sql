-- +goose Up
-- Codes promo, parrainage et tarifs « réservez tôt ».
SELECT set_config('app.scope', 'system', true);

-- Codes promo de la compagnie.
CREATE TABLE promo_codes (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id         uuid NOT NULL REFERENCES companies(id),
    code               text NOT NULL CHECK (code ~ '^[A-Z0-9-]{3,30}$'),
    label              text NOT NULL DEFAULT '',               -- usage interne (« Tabaski 2026 », « Partenaire X »)
    kind               text NOT NULL CHECK (kind IN ('percent', 'fixed')),
    value              bigint NOT NULL CHECK (value > 0),       -- pourcentage (1–100) ou montant en F CFA
    max_discount       bigint CHECK (max_discount > 0),         -- plafond (pourcentage)
    min_amount         bigint NOT NULL DEFAULT 0 CHECK (min_amount >= 0),
    valid_from         timestamptz,
    valid_to           timestamptz,
    usage_limit        integer CHECK (usage_limit > 0),         -- utilisations au total
    per_customer_limit integer NOT NULL DEFAULT 1 CHECK (per_customer_limit > 0),
    first_trip_only    boolean NOT NULL DEFAULT false,
    channels           text[] NOT NULL DEFAULT ARRAY['web', 'app', 'counter', 'ussd']::text[],
    route_ids          uuid[] NOT NULL DEFAULT '{}',            -- vide = toutes les lignes
    category_codes     text[] NOT NULL DEFAULT '{}',            -- vide = toutes les catégories
    batch_id           uuid,                                    -- codes générés en série
    is_active          boolean NOT NULL DEFAULT true,
    created_by         uuid REFERENCES staff_users(id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (kind <> 'percent' OR value <= 100),
    CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to > valid_from)
);
CREATE UNIQUE INDEX promo_codes_code_uq ON promo_codes (company_id, code);
CREATE INDEX promo_codes_batch_idx ON promo_codes (batch_id) WHERE batch_id IS NOT NULL;
CREATE TRIGGER promo_codes_updated_at BEFORE UPDATE ON promo_codes FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE promo_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE promo_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON promo_codes USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Une utilisation par réservation : appliquée → confirmée (payée) ou libérée (expirée, annulée avant paiement).
CREATE TABLE promo_redemptions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id    uuid NOT NULL REFERENCES companies(id),
    promo_code_id uuid NOT NULL REFERENCES promo_codes(id),
    booking_id    uuid NOT NULL UNIQUE REFERENCES bookings(id),
    customer_id   uuid REFERENCES customers(id),
    phone         text NOT NULL,
    amount        bigint NOT NULL CHECK (amount > 0),
    status        text NOT NULL DEFAULT 'applied' CHECK (status IN ('applied', 'confirmed', 'released')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX promo_redemptions_code_idx ON promo_redemptions (promo_code_id, status);
CREATE TRIGGER promo_redemptions_updated_at BEFORE UPDATE ON promo_redemptions FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE promo_redemptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE promo_redemptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON promo_redemptions USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Règles de prix : « réservez tôt » (remise à X jours ou plus du départ, avec quota de places).
CREATE TABLE pricing_rules (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      uuid NOT NULL REFERENCES companies(id),
    kind            text NOT NULL DEFAULT 'early_bird' CHECK (kind IN ('early_bird')),
    label           text NOT NULL DEFAULT 'Réservez tôt',
    route_id        uuid REFERENCES routes(id),                 -- NULL = toutes les lignes
    min_days_before integer NOT NULL CHECK (min_days_before BETWEEN 1 AND 365),
    percent         integer NOT NULL CHECK (percent BETWEEN 1 AND 90),
    seat_quota      integer NOT NULL CHECK (seat_quota BETWEEN 1 AND 100),  -- places à ce tarif par départ
    valid_from      date,
    valid_to        date,
    excluded_dates  date[] NOT NULL DEFAULT '{}',               -- jours de départ exclus (fêtes…)
    is_active       boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pricing_rules_company_idx ON pricing_rules (company_id) WHERE is_active;
CREATE TRIGGER pricing_rules_updated_at BEFORE UPDATE ON pricing_rules FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE pricing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE pricing_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pricing_rules USING (tenant_visible(company_id)) WITH CHECK (tenant_visible(company_id));

-- Parrainage : le filleul est lié à son parrain à l'inscription (code partagé).
-- Les comptes clients sont communs à la plateforme : la récompense est versée
-- dans la compagnie du premier voyage du filleul.
CREATE TABLE referrals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    referrer_id   uuid NOT NULL REFERENCES customers(id),
    referee_id    uuid NOT NULL UNIQUE REFERENCES customers(id),
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'rewarded', 'rejected')),
    reject_reason text NOT NULL DEFAULT '',
    company_id    uuid REFERENCES companies(id),               -- compagnie du premier voyage
    booking_id    uuid REFERENCES bookings(id),                -- premier voyage (remise filleul)
    rewarded_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (referrer_id <> referee_id)
);
CREATE INDEX referrals_referrer_idx ON referrals (referrer_id, created_at DESC);

ALTER TABLE loyalty_programs
    ADD COLUMN referee_reward_amount  bigint  NOT NULL DEFAULT 1000 CHECK (referee_reward_amount >= 0),
    ADD COLUMN referrer_reward_points integer NOT NULL DEFAULT 200  CHECK (referrer_reward_points >= 0),
    ADD COLUMN referrer_monthly_cap   integer NOT NULL DEFAULT 10   CHECK (referrer_monthly_cap >= 0);

-- Détail des remises sur la réservation et les sièges.
ALTER TABLE bookings
    ADD COLUMN promo_code_id     uuid REFERENCES promo_codes(id),
    ADD COLUMN promo_code        text,
    ADD COLUMN promo_amount      bigint NOT NULL DEFAULT 0 CHECK (promo_amount >= 0),
    ADD COLUMN early_bird_amount bigint NOT NULL DEFAULT 0 CHECK (early_bird_amount >= 0),  -- inclus dans discount_amount
    ADD COLUMN referral_amount   bigint NOT NULL DEFAULT 0 CHECK (referral_amount >= 0);
ALTER TABLE booking_seats
    ADD COLUMN early_bird_amount bigint NOT NULL DEFAULT 0 CHECK (early_bird_amount >= 0);  -- inclus dans discount_amount

-- Lignes de facture correspondantes.
ALTER TABLE invoice_lines DROP CONSTRAINT invoice_lines_kind_check;
ALTER TABLE invoice_lines ADD CONSTRAINT invoice_lines_kind_check
    CHECK (kind IN ('ticket', 'fare_difference', 'change_fee', 'loyalty', 'parcel', 'baggage', 'refund', 'promo', 'referral'));

-- +goose Down
ALTER TABLE invoice_lines DROP CONSTRAINT invoice_lines_kind_check;
ALTER TABLE invoice_lines ADD CONSTRAINT invoice_lines_kind_check
    CHECK (kind IN ('ticket', 'fare_difference', 'change_fee', 'loyalty', 'parcel', 'baggage', 'refund'));
ALTER TABLE booking_seats DROP COLUMN early_bird_amount;
ALTER TABLE bookings DROP COLUMN promo_code_id, DROP COLUMN promo_code, DROP COLUMN promo_amount,
    DROP COLUMN early_bird_amount, DROP COLUMN referral_amount;
ALTER TABLE loyalty_programs DROP COLUMN referee_reward_amount, DROP COLUMN referrer_reward_points, DROP COLUMN referrer_monthly_cap;
DROP TABLE referrals;
DROP TABLE pricing_rules;
DROP TABLE promo_redemptions;
DROP TABLE promo_codes;

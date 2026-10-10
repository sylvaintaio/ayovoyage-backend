-- +goose Up
-- Modification de billet (changement de départ) et conditions selon le niveau de fidélité.

-- Conditions de modification, propres à chaque compagnie.
ALTER TABLE companies
    ADD COLUMN change_enabled      boolean NOT NULL DEFAULT true,
    ADD COLUMN change_cutoff_hours integer NOT NULL DEFAULT 3 CHECK (change_cutoff_hours BETWEEN 0 AND 168),
    ADD COLUMN change_fee_amount   bigint  NOT NULL DEFAULT 1000 CHECK (change_fee_amount >= 0),
    ADD COLUMN change_max_count    integer NOT NULL DEFAULT 1 CHECK (change_max_count BETWEEN 0 AND 5),
    ADD COLUMN change_window_days  integer NOT NULL DEFAULT 30 CHECK (change_window_days BETWEEN 1 AND 365);

-- Avantages des niveaux de fidélité : NULL = règle générale de la compagnie.
ALTER TABLE loyalty_tiers
    ADD COLUMN cancellation_fee_percent  integer CHECK (cancellation_fee_percent BETWEEN 0 AND 100),
    ADD COLUMN cancellation_cutoff_hours integer CHECK (cancellation_cutoff_hours BETWEEN 0 AND 168),
    ADD COLUMN free_change               boolean NOT NULL DEFAULT false;

-- Une réservation modifiée passe au statut « changed » ; la nouvelle pointe vers elle.
ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings ADD CONSTRAINT bookings_status_check
    CHECK (status IN ('pending', 'confirmed', 'cancelled', 'expired', 'refunded', 'changed'));
ALTER TABLE bookings
    ADD COLUMN changed_from_booking_id uuid REFERENCES bookings(id),
    ADD COLUMN change_count         integer NOT NULL DEFAULT 0 CHECK (change_count >= 0),
    ADD COLUMN change_fee_amount    bigint  NOT NULL DEFAULT 0 CHECK (change_fee_amount >= 0),
    ADD COLUMN change_credit_amount bigint  NOT NULL DEFAULT 0 CHECK (change_credit_amount >= 0);
-- Au plus une modification en cours ou aboutie par réservation.
CREATE UNIQUE INDEX bookings_changed_from_uq ON bookings (changed_from_booking_id)
    WHERE changed_from_booking_id IS NOT NULL AND status IN ('pending', 'confirmed', 'changed');

-- +goose Down
DROP INDEX bookings_changed_from_uq;
ALTER TABLE bookings DROP COLUMN change_credit_amount, DROP COLUMN change_fee_amount,
    DROP COLUMN change_count, DROP COLUMN changed_from_booking_id;
UPDATE bookings SET status = 'cancelled' WHERE status = 'changed';
ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings ADD CONSTRAINT bookings_status_check
    CHECK (status IN ('pending', 'confirmed', 'cancelled', 'expired', 'refunded'));
ALTER TABLE loyalty_tiers DROP COLUMN free_change, DROP COLUMN cancellation_cutoff_hours, DROP COLUMN cancellation_fee_percent;
ALTER TABLE companies DROP COLUMN change_window_days, DROP COLUMN change_max_count, DROP COLUMN change_fee_amount,
    DROP COLUMN change_cutoff_hours, DROP COLUMN change_enabled;

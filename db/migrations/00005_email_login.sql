-- +goose Up

-- Connexion par e-mail : un client peut être identifié par son téléphone,
-- par son e-mail, ou les deux.
ALTER TABLE customers ALTER COLUMN phone DROP NOT NULL;
ALTER TABLE customers ADD CONSTRAINT customers_contact_ck CHECK (phone IS NOT NULL OR email IS NOT NULL);
UPDATE customers SET email = lower(trim(email)) WHERE email IS NOT NULL;
CREATE UNIQUE INDEX customers_email_uq ON customers (lower(email)) WHERE email IS NOT NULL;

-- Codes à usage unique : destination (téléphone ou e-mail) et canal d'envoi.
ALTER TABLE otp_codes RENAME COLUMN phone TO destination;
ALTER TABLE otp_codes ADD COLUMN channel text NOT NULL DEFAULT 'sms' CHECK (channel IN ('sms', 'email'));
DROP INDEX otp_codes_phone_idx;
CREATE INDEX otp_codes_destination_idx ON otp_codes (destination, created_at DESC);

-- +goose Down
DROP INDEX otp_codes_destination_idx;
ALTER TABLE otp_codes DROP COLUMN channel;
ALTER TABLE otp_codes RENAME COLUMN destination TO phone;
CREATE INDEX otp_codes_phone_idx ON otp_codes (phone, created_at DESC);
DROP INDEX customers_email_uq;
ALTER TABLE customers DROP CONSTRAINT customers_contact_ck;
DELETE FROM customers WHERE phone IS NULL;
ALTER TABLE customers ALTER COLUMN phone SET NOT NULL;

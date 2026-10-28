-- name: NextInvoiceNumber :one
-- Numéro suivant, sans trou (ligne verrouillée jusqu'à la fin de la transaction).
INSERT INTO invoice_counters (company_id, kind, year, last_value)
VALUES (@company_id, @kind, @year, 1)
ON CONFLICT (company_id, kind, year) DO UPDATE SET last_value = invoice_counters.last_value + 1
RETURNING last_value;

-- name: InsertInvoice :one
INSERT INTO invoices (company_id, kind, number, payment_id, refund_id, booking_id, parcel_id, original_invoice_id, customer_id,
    seller_name, seller_legal_name, seller_tax_id, seller_trade_register, seller_address, seller_phone, seller_email, footer,
    customer_name, customer_phone, customer_email, customer_company, customer_tax_id,
    agency_name, operator_name, payment_method, reference, currency, total_amount)
VALUES (@company_id, @kind, @number, sqlc.narg(payment_id), sqlc.narg(refund_id), sqlc.narg(booking_id), sqlc.narg(parcel_id),
    sqlc.narg(original_invoice_id), sqlc.narg(customer_id),
    @seller_name, @seller_legal_name, @seller_tax_id, @seller_trade_register, @seller_address, @seller_phone, @seller_email, @footer,
    @customer_name, @customer_phone, @customer_email, @customer_company, @customer_tax_id,
    @agency_name, @operator_name, @payment_method, @reference, @currency, @total_amount)
RETURNING *;

-- name: InsertInvoiceLine :exec
INSERT INTO invoice_lines (company_id, invoice_id, position, kind, label, detail, quantity, unit_price, amount)
VALUES (@company_id, @invoice_id, @position, @kind, @label, @detail, @quantity, @unit_price, @amount);

-- name: InvoiceExistsForPayment :one
SELECT EXISTS (SELECT 1 FROM invoices WHERE payment_id = $1) AS exists;

-- name: InvoiceExistsForRefund :one
SELECT EXISTS (SELECT 1 FROM invoices WHERE refund_id = $1) AS exists;

-- name: GetInvoice :one
SELECT * FROM invoices WHERE id = $1;

-- name: GetInvoiceByPayment :one
SELECT * FROM invoices WHERE payment_id = $1;

-- name: LatestBookingInvoice :one
SELECT * FROM invoices WHERE booking_id = $1 AND kind = 'invoice' ORDER BY issued_at DESC LIMIT 1;

-- name: ListInvoiceLines :many
SELECT * FROM invoice_lines WHERE invoice_id = $1 ORDER BY position;

-- name: ListInvoices :many
SELECT * FROM invoices
WHERE company_id = @company_id
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
  AND issued_at >= @from_ts::timestamptz AND issued_at < @to_ts::timestamptz
  AND (sqlc.narg('search')::text IS NULL OR number ILIKE '%' || sqlc.narg('search')::text || '%'
       OR reference ILIKE '%' || sqlc.narg('search')::text || '%' OR customer_name ILIKE '%' || sqlc.narg('search')::text || '%'
       OR customer_phone LIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY issued_at DESC
LIMIT @lim OFFSET @off;

-- name: InvoiceTotals :one
SELECT
    coalesce(sum(total_amount) FILTER (WHERE kind = 'invoice'), 0)::bigint AS invoiced,
    coalesce(sum(total_amount) FILTER (WHERE kind = 'credit_note'), 0)::bigint AS credited,
    count(*) FILTER (WHERE kind = 'invoice')::int AS invoices_count,
    count(*) FILTER (WHERE kind = 'credit_note')::int AS credit_notes_count
FROM invoices
WHERE company_id = @company_id AND issued_at >= @from_ts::timestamptz AND issued_at < @to_ts::timestamptz;

-- name: ListBookingInvoices :many
SELECT * FROM invoices WHERE booking_id = $1 ORDER BY issued_at;

-- name: ListParcelInvoices :many
SELECT * FROM invoices WHERE parcel_id = $1 ORDER BY issued_at;

-- name: ListCustomerInvoices :many
-- Factures d'un client : ses réservations et les colis qu'il a payés.
SELECT * FROM invoices WHERE customer_id = @customer_id ORDER BY issued_at DESC, id DESC LIMIT @lim OFFSET @off;

-- name: ListUninvoicedBookingPayments :many
-- Encaissements d'une réservation pas encore facturés.
SELECT p.* FROM payments p
WHERE p.booking_id = $1 AND p.status = 'succeeded'
  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.payment_id = p.id)
ORDER BY p.succeeded_at;

-- name: QueueInvoiceEmail :exec
UPDATE invoices SET email_status = 'pending', email_to = @email_to, email_attempts = 0, email_next_at = now(), email_error = ''
WHERE id = @id;

-- name: ClaimInvoiceEmails :many
SELECT * FROM invoices
WHERE email_status = 'pending' AND email_next_at <= now()
ORDER BY email_next_at
LIMIT 20
FOR UPDATE SKIP LOCKED;

-- name: SetInvoiceEmailResult :exec
-- result : sent, simulated ou failed. Échec : nouvel essai 5, 10, 15, 20 min plus tard ; abandon après 5 essais.
UPDATE invoices SET
    email_attempts = email_attempts + 1,
    email_to = @email_to,
    email_error = @email_error,
    email_status = CASE
        WHEN @result::text <> 'failed' THEN @result::text
        WHEN email_attempts + 1 >= 5 THEN 'failed'
        ELSE 'pending' END,
    email_next_at = now() + make_interval(mins => 5 * (email_attempts + 1)),
    emailed_at = CASE WHEN @result::text <> 'failed' THEN now() ELSE emailed_at END
WHERE id = @id;

-- name: RecordInvoiceEmail :exec
-- Envoi manuel ou joint à la confirmation : résultat immédiat, pas de nouvel essai automatique.
UPDATE invoices SET
    email_to = @email_to,
    email_error = @email_error,
    email_status = @result::text,
    emailed_at = CASE WHEN @result::text <> 'failed' THEN now() ELSE emailed_at END
WHERE id = @id;

-- name: GetCompany :one
SELECT * FROM companies WHERE id = $1;

-- name: ListActiveCompanies :many
SELECT * FROM companies WHERE status = 'active' ORDER BY name;

-- name: CreateCompany :one
INSERT INTO companies (name, slug, legal_name, phone, email)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateCompanySettings :one
UPDATE companies SET
    name                      = @name,
    legal_name                = @legal_name,
    phone                     = @phone,
    email                     = @email,
    logo_url                  = @logo_url,
    booking_hold_minutes      = @booking_hold_minutes,
    sales_cutoff_minutes      = @sales_cutoff_minutes,
    cancellation_cutoff_hours = @cancellation_cutoff_hours,
    cancellation_fee_percent  = @cancellation_fee_percent,
    refund_approval_threshold = @refund_approval_threshold,
    change_enabled            = @change_enabled,
    change_cutoff_hours       = @change_cutoff_hours,
    change_fee_amount         = @change_fee_amount,
    change_max_count          = @change_max_count,
    change_window_days        = @change_window_days,
    baggage_extra_per_kg      = @baggage_extra_per_kg,
    tax_id                    = sqlc.narg(tax_id),
    trade_register            = sqlc.narg(trade_register),
    address                   = sqlc.narg(address),
    invoice_prefix            = @invoice_prefix,
    invoice_footer            = @invoice_footer,
    pay_later_enabled         = @pay_later_enabled,
    pay_later_hold_hours      = @pay_later_hold_hours,
    pay_later_cutoff_minutes  = @pay_later_cutoff_minutes,
    pay_later_max_seats       = @pay_later_max_seats,
    pay_later_max_active      = @pay_later_max_active,
    pay_later_quota_percent   = @pay_later_quota_percent,
    pay_later_strikes         = @pay_later_strikes
WHERE id = @id
RETURNING *;

-- name: ListCities :many
SELECT * FROM cities ORDER BY name;

-- name: GetCity :one
SELECT * FROM cities WHERE id = $1;

-- name: CreateCity :one
INSERT INTO cities (name, slug, country_code) VALUES ($1, $2, $3) RETURNING *;

-- name: ListPublicAgencies :many
SELECT sqlc.embed(a), c.name AS city_name, c.slug AS city_slug,
       co.name AS company_name, co.slug AS company_slug
FROM agencies a
JOIN cities c ON c.id = a.city_id
JOIN companies co ON co.id = a.company_id
WHERE a.is_active AND co.status = 'active'
  AND (sqlc.narg('city_slug')::text IS NULL OR c.slug = sqlc.narg('city_slug')::text)
  AND (sqlc.narg('service')::text IS NULL OR sqlc.narg('service')::text = ANY (a.services))
  AND (sqlc.narg('company_id')::uuid IS NULL OR a.company_id = sqlc.narg('company_id')::uuid)
  AND (sqlc.narg('q')::text IS NULL
       OR a.name ILIKE '%' || sqlc.narg('q')::text || '%'
       OR c.name ILIKE '%' || sqlc.narg('q')::text || '%'
       OR a.address ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY c.name, a.name;

-- name: GetPublicAgencyBySlug :one
SELECT sqlc.embed(a), c.name AS city_name, c.slug AS city_slug,
       co.name AS company_name, co.slug AS company_slug
FROM agencies a
JOIN cities c ON c.id = a.city_id
JOIN companies co ON co.id = a.company_id
WHERE a.slug = $1 AND a.is_active AND co.status = 'active';

-- name: ListDestinationsFromAgency :many
-- Villes où l'on peut descendre après être monté à cette agence (lignes actives).
SELECT DISTINCT dc.name AS city_name, dc.slug AS city_slug
FROM route_stops b
JOIN stop_points bsp ON bsp.id = b.stop_point_id
JOIN routes r ON r.id = b.route_id
JOIN route_stops a ON a.route_id = b.route_id AND a.seq > b.seq AND a.can_alight
JOIN stop_points asp ON asp.id = a.stop_point_id
JOIN cities dc ON dc.id = asp.city_id
WHERE bsp.agency_id = $1 AND b.can_board AND r.is_active AND asp.city_id <> bsp.city_id
ORDER BY dc.name;

-- name: ListAgencies :many
SELECT sqlc.embed(a), c.name AS city_name
FROM agencies a
JOIN cities c ON c.id = a.city_id
WHERE a.company_id = $1
ORDER BY c.name, a.name;

-- name: GetAgency :one
SELECT * FROM agencies WHERE id = $1 AND company_id = $2;

-- name: CreateAgency :one
INSERT INTO agencies (company_id, city_id, name, slug, address, landmark, latitude, longitude,
                      phone, whatsapp, email, opening_hours, services, instructions, photos, is_active)
VALUES (@company_id, @city_id, @name, @slug, @address, @landmark, @latitude, @longitude,
        @phone, @whatsapp, @email, @opening_hours, @services, @instructions, @photos, @is_active)
RETURNING *;

-- name: UpdateAgency :one
UPDATE agencies SET
    city_id       = @city_id,
    name          = @name,
    address       = @address,
    landmark      = @landmark,
    latitude      = @latitude,
    longitude     = @longitude,
    phone         = @phone,
    whatsapp      = @whatsapp,
    email         = @email,
    opening_hours = @opening_hours,
    services      = @services,
    instructions  = @instructions,
    photos        = @photos,
    is_active     = @is_active
WHERE id = @id AND company_id = @company_id
RETURNING *;

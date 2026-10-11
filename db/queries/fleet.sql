-- ============================ Catégories de sièges ============================

-- name: ListSeatCategories :many
SELECT * FROM seat_categories WHERE company_id = $1 ORDER BY sort_order, name;

-- name: GetSeatCategory :one
SELECT * FROM seat_categories WHERE id = $1 AND company_id = $2;

-- name: CreateSeatCategory :one
INSERT INTO seat_categories (company_id, code, name, description, surcharge_amount, color, perks, sort_order)
VALUES (@company_id, @code, @name, @description, @surcharge_amount, @color, @perks, @sort_order)
RETURNING *;

-- name: UpdateSeatCategory :one
UPDATE seat_categories SET name = @name, description = @description, surcharge_amount = @surcharge_amount,
                           color = @color, perks = @perks, sort_order = @sort_order
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- ============================ Plans de sièges ============================

-- name: ListSeatLayouts :many
SELECT * FROM seat_layouts WHERE company_id = $1 ORDER BY name;

-- name: GetSeatLayout :one
SELECT * FROM seat_layouts WHERE id = $1 AND company_id = $2;

-- name: CreateSeatLayout :one
INSERT INTO seat_layouts (company_id, name, rows, cols, seat_count)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateSeatLayout :one
UPDATE seat_layouts SET name = $3, rows = $4, cols = $5, seat_count = $6
WHERE id = $1 AND company_id = $2
RETURNING *;

-- name: IsSeatLayoutInUse :one
SELECT EXISTS (SELECT 1 FROM departures WHERE layout_id = $1) AS in_use;

-- name: DeleteSeatLayoutCells :exec
DELETE FROM seat_layout_cells WHERE layout_id = $1;

-- name: InsertSeatLayoutCell :exec
INSERT INTO seat_layout_cells (company_id, layout_id, row_index, col_index, kind, seat_label,
                               category_id, extra_surcharge, attributes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListSeatLayoutCells :many
SELECT * FROM seat_layout_cells WHERE layout_id = $1 ORDER BY row_index, col_index;

-- ============================ Bus ============================

-- name: ListBuses :many
SELECT sqlc.embed(b), l.name AS layout_name, l.seat_count
FROM buses b
JOIN seat_layouts l ON l.id = b.layout_id
WHERE b.company_id = $1
ORDER BY b.plate_number;

-- name: GetBus :one
SELECT * FROM buses WHERE id = $1 AND company_id = $2;

-- name: CreateBus :one
INSERT INTO buses (company_id, plate_number, internal_code, model, layout_id, amenities, status, home_agency_id, notes)
VALUES (@company_id, upper(@plate_number::text), @internal_code, @model, @layout_id, @amenities, @status, @home_agency_id, @notes)
RETURNING *;

-- name: UpdateBus :one
UPDATE buses SET plate_number = upper(@plate_number::text), internal_code = @internal_code, model = @model,
                 layout_id = @layout_id, amenities = @amenities, status = @status,
                 home_agency_id = @home_agency_id, notes = @notes
WHERE id = @id AND company_id = @company_id
RETURNING *;

-- ============================ Lignes ============================

-- name: ListRoutes :many
-- Extrémités et noms des arrêts : voir ListRouteStops.
SELECT * FROM routes WHERE company_id = $1 ORDER BY name;

-- name: GetRoute :one
SELECT * FROM routes WHERE id = $1 AND company_id = $2;

-- name: CreateRoute :one
INSERT INTO routes (company_id, origin_agency_id, destination_agency_id, name, base_price,
                    duration_minutes, distance_km, included_baggage_kg, is_active)
VALUES (@company_id, @origin_agency_id, @destination_agency_id, @name, @base_price,
        @duration_minutes, @distance_km, @included_baggage_kg, @is_active)
RETURNING *;

-- name: UpdateRoute :one
UPDATE routes SET name = @name, base_price = @base_price, duration_minutes = @duration_minutes,
                  distance_km = @distance_km, included_baggage_kg = @included_baggage_kg, is_active = @is_active
WHERE id = @id AND company_id = @company_id
RETURNING *;

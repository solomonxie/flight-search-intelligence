-- Uniform timestamp naming across all tables: scraped_at was
-- flight_prices' own name for "when this row was written" -- rename to
-- created_at to match every other table, and add updated_at for the
-- same reason (unused today, flight_prices rows are insert-only, but
-- present for consistency).
ALTER TABLE flight_prices RENAME COLUMN scraped_at TO created_at;
ALTER TABLE flight_prices ADD COLUMN updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

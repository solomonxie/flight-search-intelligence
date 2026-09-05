-- Uniform timestamp naming across all tables: scraped_at was
-- flight_prices' own name for "when this row was written" -- rename to
-- created_at to match every other table, and add updated_at for the
-- same reason (unused today, flight_prices rows are insert-only, but
-- present for consistency).
--
-- No DEFAULT expression on updated_at: SQLite's ALTER TABLE ADD COLUMN
-- rejects a non-constant default (e.g. strftime(...)) -- only CREATE
-- TABLE allows one. catalog.InsertFlightPrices sets both columns
-- explicitly on every insert instead (same convention route_search_plans/
-- agent_requests/agent_tasks already use), so no DB-side default is
-- actually needed going forward; existing rows are backfilled below.
ALTER TABLE flight_prices RENAME COLUMN scraped_at TO created_at;
ALTER TABLE flight_prices ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
UPDATE flight_prices SET updated_at = created_at WHERE updated_at = '';

-- Raw/serving-layer tables for the local SQLite catalog (see
-- internal/catalog and DESIGN.md "Local development"). This is the
-- one source of truth for these tables' shape -- not Go code (which
-- only ever inserts/selects, never creates or alters schema -- see
-- DESIGN.md "Schema ownership"), and not etl/dbt's staging model
-- (which only describes the shape it expects to read, per
-- etl/dbt/models/staging/sources.yml).
--
-- Apply with the database's own tooling before running cmd/collector
-- or cmd/routesearch -- `make db-init`, or directly:
--   sqlite3 data/flight_search.db < db/schema.sql

-- One row per scraped price observation. Shape matches
-- etl/dbt/models/staging/stg_flights.sql's `raw.flight_prices` source.
CREATE TABLE IF NOT EXISTS flight_prices (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	origin       TEXT NOT NULL,
	destination  TEXT NOT NULL,
	airline      TEXT NOT NULL,
	depart_date  TEXT NOT NULL,
	return_date  TEXT NOT NULL DEFAULT '',
	price_cents  INTEGER NOT NULL,
	currency     TEXT NOT NULL,
	source       TEXT NOT NULL,
	scraped_at   TEXT NOT NULL
);

-- One row per routesearch request -- the audit trail from DESIGN.md
-- "Audit trail" (candidates considered, kept, or pruned, and why).
CREATE TABLE IF NOT EXISTS route_search_plans (
	id          TEXT PRIMARY KEY,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	status      TEXT NOT NULL,
	plan_json   TEXT NOT NULL
);

-- Raw/serving-layer tables for the local SQLite catalog (see
-- internal/catalog and DESIGN.md "Local development"). This is the
-- one source of truth for these tables' shape -- not Go code, not
-- etl/dbt's staging model (which only describes the shape it expects
-- to read, per etl/dbt/models/staging/sources.yml).
--
-- Apply before running cmd/collector or cmd/routesearch:
--   go run ./cmd/dbinit -db data/flight_search.db

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

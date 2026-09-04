-- One row per scraped price observation. Shape matches
-- etl/dbt/models/staging/stg_flights.sql's `raw.flight_prices` source.
CREATE TABLE flight_prices (
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

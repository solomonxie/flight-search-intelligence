-- Raw SearchFlightOffers responses, keyed by exact query params. Backs
-- routesearch's cache-first read: reuse a scrape within the freshness
-- window instead of re-scraping live (see routesearch.searchOffers).
CREATE TABLE flight_offers_cache (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	origin       TEXT NOT NULL,
	destination  TEXT NOT NULL,
	depart_date  TEXT NOT NULL,
	return_date  TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL,
	offers_json  TEXT NOT NULL,
	created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX idx_flight_offers_cache_lookup
	ON flight_offers_cache (origin, destination, depart_date, return_date, created_at DESC);

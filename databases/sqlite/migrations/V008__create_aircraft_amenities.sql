-- Static per-(airline, aircraft, cabin) amenity info shown on Google
-- Flights' expand panel (legroom, wifi, power/USB, on-demand video) --
-- reference data, not a scrape-freshness cache: an aircraft's cabin
-- config barely changes, so a row is written once and read many times
-- (see routesearch.searchOffers for the contrast -- that cache expires,
-- this one isn't expected to).
CREATE TABLE aircraft_amenities (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	airline           TEXT NOT NULL,
	plane_type        TEXT NOT NULL,
	cabin_class       TEXT NOT NULL DEFAULT '',
	legroom_inches    REAL,
	wifi              TEXT NOT NULL DEFAULT '',
	power_outlets     INTEGER NOT NULL DEFAULT 0,
	on_demand_video   INTEGER NOT NULL DEFAULT 0,
	source            TEXT NOT NULL,
	created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE UNIQUE INDEX idx_aircraft_amenities_key
	ON aircraft_amenities (airline, plane_type, cabin_class);

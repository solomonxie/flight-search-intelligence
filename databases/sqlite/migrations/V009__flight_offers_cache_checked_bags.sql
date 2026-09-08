-- CheckedBags changes Offer.Price (DESIGN.md "Baggage cost is a query
-- input") but wasn't part of the cache key, so a bags=2 search could
-- silently reuse a bags=0 scrape's prices within the freshness window.
ALTER TABLE flight_offers_cache ADD COLUMN checked_bags INTEGER NOT NULL DEFAULT 0;

DROP INDEX idx_flight_offers_cache_lookup;
CREATE INDEX idx_flight_offers_cache_lookup
	ON flight_offers_cache (origin, destination, depart_date, return_date, checked_bags, created_at DESC);

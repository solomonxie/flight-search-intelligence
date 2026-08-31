-- Initial schema: one row per (route, date, airline, source) price
-- observation scraped by cmd/collector. cmd/search-api reads from this
-- table.
-- TODO: revisit indexing once real query patterns are known (likely a
-- composite index on (origin, destination, depart_date)).

CREATE TABLE IF NOT EXISTS flight_prices (
    id             BIGSERIAL PRIMARY KEY,
    origin         TEXT NOT NULL,       -- IATA code, e.g. 'SFO'
    destination    TEXT NOT NULL,       -- IATA code, e.g. 'JFK'
    airline        TEXT NOT NULL,
    depart_date    DATE NOT NULL,
    return_date    DATE,                -- NULL for one-way
    price_cents    INTEGER NOT NULL,
    currency       TEXT NOT NULL DEFAULT 'USD',
    source         TEXT NOT NULL,       -- which provider/site this came from
    scraped_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

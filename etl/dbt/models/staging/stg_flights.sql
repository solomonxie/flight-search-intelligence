-- Staging model: one row per cleaned price observation, sourced from
-- whatever table/location etl/spark/clean_raw_flights.py writes to.
-- TODO: add dedup/tests once the source table's real shape is known.

select
    origin,
    destination,
    airline,
    depart_date,
    return_date,
    price_cents,
    currency,
    source,
    scraped_at
from {{ source('raw', 'flight_prices') }}

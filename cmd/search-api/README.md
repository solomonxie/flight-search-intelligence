# Flight Search API

The read path DESIGN.md describes: flexible, low-latency search (route,
date range, airline, price, stops...) against the serving store — SQLite
locally, Postgres in prod, synced from the Delta Lake gold layer once
`etl/` is real. On a miss (route/date not yet collected), it's also meant
to be a producer into the agent loop — creating an `agent_requests` row
directly (DESIGN.md "Collection scope") and returning a "pending, check
back" response instead of an empty one.

### `main.go`

**Scaffold only** — currently just prints a placeholder message. Not yet
wired to `internal/catalog`, and not yet creating `agent_requests` rows
on a miss. Both are open work; this file's package doc comment is the
up-to-date statement of intent until they're built.

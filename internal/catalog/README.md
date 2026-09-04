# Serving Store

The local-dev serving store — SQLite today, standing in for Postgres in
prod (see DESIGN.md "Local development"). This package only ever does
CRUD: it never creates or alters schema. Schema lives in
`databases/sqlite/migrations/`, applied by Flyway (`make db-init`) — see
`databases/README.md` and DESIGN.md "Schema ownership" for why that line
is drawn, and why it's drawn here specifically rather than left as a
convention someone could accidentally cross.

### `catalog.go`

`Open` connects to the SQLite file and fails fast — checking every table
this package expects actually exists (`checkSchema`, a `SELECT ... FROM
sqlite_master` read, not schema management) rather than letting the first
real query fail with a confusing "no such table" if `make db-init` was
never run.

Three tables' worth of methods live here: `InsertFlightPrices`/
`CachedPriceCents` (the `flight_prices` table — every scraped offer,
and the price-aware lower-bound lookup `routesearch`'s hub search uses
instead of a cruder distance × $/mile prior), and
`SaveRouteSearchPlan` (the `route_search_plans` table — one JSON audit
trail per search request, upserted as it runs so a crash mid-search still
leaves a trace).

### `agent.go`

The `agent_requests`/`agent_tasks` tables behind `internal/agents`' state
machine (see that package's README, and DESIGN.md "Agent loop" /
"Collector task dispatch"). `AgentRequestRow`/`AgentTaskRow` mirror the
tables directly — `spec_json`/`rounds_json`/`params_json`/`result_json`
are opaque strings here on purpose, so this package stays free of a
dependency on `internal/agents`' types.

The one function worth reading closely is `ClaimPendingAgentTask`: SQLite
has no `UPDATE ... LIMIT 1 RETURNING` multiple concurrent claimers can
rely on, so it selects a candidate task, then re-checks the following
`UPDATE ... WHERE status = 'pending'`'s affected-row count to detect (and
just skip, not error on) a lost race against another worker goroutine or
process. `ReapStaleAgentTaskClaims` is the other half of that same
lease pattern — a task claimed but never finished (its worker crashed
mid-fetch) gets reset to `pending` once its claim is older than a lease
timeout, the hand-rolled stand-in for what a workflow engine's Activity
timeout+retry would give for free.

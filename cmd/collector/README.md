# Flight Collector

Fetches real flight fares by scraping Google Flights, in two modes: a
direct-run CLI for one-off/manual use, and a poll worker that runs
searches the agent loop (`internal/agents`, `cmd/email-intake`)
dispatches. See DESIGN.md "Collector task dispatch" for why this is a
plain poll loop against the serving store rather than a Kafka consumer
or a Temporal worker.

### `main.go`

Flag parsing and mode dispatch. Default mode is the direct-run CLI:
`-origin`/`-destination`/`-date`/`-return-date`/`-adults` build one
`googleflights.SearchParams` call, the raw HTML response gets written to
`-out-dir` (`writeRaw`, standing in for the S3 raw zone), and parsed
offers get saved into the serving store (`saveOffers`, in the shape
`etl/dbt`'s `raw.flight_prices` source expects). `-worker` switches to
the poll-worker mode instead (`worker.go`), ignoring the search flags.

### `worker.go`

`runWorker` — the poll loop. Each tick: sweep stale task claims
(`ReapStaleAgentTaskClaims`, catching a worker that crashed mid-fetch),
then try to claim one pending `agent_tasks` row
(`ClaimPendingAgentTask`); if it gets one, hand it to `runTask` in its
own goroutine (capped by `-concurrency`, standing in for a Temporal
task queue's concurrency limit / a Kafka partition's per-consumer
throttle) and keep polling — claiming the next task never waits on the
fetch that's already running. `runTask` decodes the task's
`agents.CollectRouteRequest`, calls `fetchFare`, and writes the result
(or, on failure, the error) back to the row.

### `activities.go`

`fetchFare` — the one real unit of work: wraps
`internal/routesearch.Search` (one-way only, for now) with the fixed
`MinLayoverMinutes`/`MaxLayoverMinutes`/`PricePerMile` defaults, and
trims the resulting `routesearch.Plan` down to the thin
`agents.CollectRouteResult` shape a task's `result_json` holds — the full
per-candidate audit trail stays in `route_search_plans`
(`internal/catalog`), written by `routesearch.Search` itself, not
duplicated here.

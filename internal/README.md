# Application Internals

The non-`main` packages every `cmd/` binary is built from. Each section
below is one subfolder.

## `agents/`

The state machine behind DESIGN.md's "Agent loop: LLM drives the search,
Go stays narrow." Deliberately not a workflow engine — durability comes
from committing state to `agent_requests`/`agent_tasks` rows
(`internal/catalog`) before returning, not from a replay log, and "async"
means dispatching a search is an `INSERT` that returns immediately, never
a blocked wait (see the package doc at the top of `reconcile.go` for the
full reasoning). This package holds only the *logic*; the processes that
run it live in `cmd/email-intake` (the poll loop advancing requests) and
`cmd/collector` (the poll loop running the tasks this package dispatches).

- **`spec.go`** — shared types: `Spec` (a request's structured
  understanding — concrete fields plus a plain-language
  `SoftConstraints` list that deliberately never becomes typed fields),
  `Decision`/`Action` (dispatch, defer, or finalize), `RoundRecord` (one
  loop iteration's audit-trail entry), `Outcome` (the final drafted email
  plus why it stopped).
- **`task.go`** — `CollectRouteRequest`/`CollectRouteResult`/
  `CollectRouteOffer`, the payload shape written into
  `agent_tasks.params_json`/`result_json` — the whole contract between
  this package and `cmd/collector`.
- **`decide.go`** — `DecideNextAction` and `DraftFinalEmail`, currently
  **deterministic stubs standing in for a real LLM call** (DESIGN.md
  "LLM choice / call shape: deferred entirely"). The stub dispatches the
  spec as given on round 0, finalizes once a result has any offer,
  otherwise widens `MaxHours`/`QueryBudget` and retries — it does *not*
  evaluate soft constraints, flagged explicitly in `Reasoning` rather
  than silently skipped, since that judgment call is exactly what a real
  LLM call is for.
- **`reconcile.go`** — the state machine itself. `AdvanceRequest` is one
  poll tick's worth of work for one request: `awaiting_decision` calls
  `DecideNextAction` and either inserts a new task (bounded by
  `RedispatchCap = 3`) or finalizes; `dispatched` checks the current
  round's task and, once it's `done`/`failed`, folds the result in and
  goes back to `awaiting_decision`. `NewRequest`/`AppendSoftConstraint`
  are the entry points `cmd/email-intake -start`/`-signal` use.

## `catalog/`

The local-dev serving store — SQLite today, standing in for Postgres in
prod. This package only ever does CRUD; it never creates or alters
schema (see `databases/README.md` and DESIGN.md "Schema ownership").

- **`catalog.go`** — `Open` connects and fails fast if
  `databases/sqlite/migrations/` hasn't been applied yet (`make
  db-init`). Methods here cover `flight_prices` (`InsertFlightPrices`,
  `CachedPriceCents` — the price-aware lower bound `routesearch`'s hub
  search uses) and `route_search_plans` (`SaveRouteSearchPlan`, one JSON
  audit trail per search request, upserted as it runs).
- **`agent.go`** — the `agent_requests`/`agent_tasks` tables behind
  `agents`' state machine. `AgentRequestRow`/`AgentTaskRow` keep
  `spec_json`/`rounds_json`/`params_json`/`result_json` as opaque
  strings, so this package stays free of a dependency on `agents`'
  types. `ClaimPendingAgentTask` is the one worth reading closely: SQLite
  has no `UPDATE ... LIMIT 1 RETURNING` multiple claimers can rely on, so
  it selects a candidate then re-checks the following `UPDATE`'s
  affected-row count to detect (and just skip) a lost race.
  `ReapStaleAgentTaskClaims` resets a task whose worker crashed mid-fetch
  back to `pending` once its claim lease expires.

## `common/`

Small utilities with no better home of their own.

- **`env.go`** — `Load(path)` reads `KEY=VALUE` lines from a `.env`-style
  file into the process environment (a minimal stdlib substitute for a
  `.env` dependency); a missing file isn't an error, and an already-set
  environment variable always wins over the file. Every `cmd/` binary
  calls `common.Load(".env")` and ignores the error.

## `googleflights/`

A lightweight Google Flights scraper: builds the reverse-engineered
`tfs` protobuf query param, does a plain HTTP GET against the public
search page, and parses results back out of an embedded JS payload. No
headless browser, no API key — ported from
[AWeirdDev/flights](https://github.com/AWeirdDev/flights) (Python). Both
the query schema and the response payload shape are undocumented and can
change without notice.

- **`googleflights.go`** — the public surface: `Client.SearchFlightOffers`
  (plain one-way/round-trip route+date search) and `Client.Search` (a
  full `Query` for multi-leg/seat-class/bag-count cases). Returns both
  the parsed `[]Offer` and the raw response bytes.
- **`parse.go`** — `parseOffers` extracts offers from the search page's
  embedded `AF_initDataCallback` payload, a deeply nested, unlabeled JSON
  array navigated by fixed indices (`idx`/`asSlice`/`asString`/`asInt`
  helpers). A panic from an unexpected shape is converted into an error
  rather than crashing the caller — the reverse-engineered payload can
  change shape without notice.
- **`protobuf.go`** — hand-rolled proto3 wire-format encoding (varint/
  length-delimited primitives) for the small message set the `tfs` param
  needs, documented as a comment at the top of the file. No protobuf
  library/codegen needed for four small messages.

## `openflights/`

Loads the [OpenFlights](https://openflights.org/data.php) airports/routes
reference dataset — static data, downloaded once and cached locally, not
scraped — into the route-existence graph `routesearch`'s hub search
prunes candidates against before spending a single real query.

- **`openflights.go`** — the only file. `Load(cacheDir)` downloads
  `airports.dat`/`routes.dat` if not already cached, then parses both
  into a `Graph` (`Airports`: IATA → coordinates/timezone; `Routes`: a
  nonstop-only adjacency map). `CandidateHubs(origin, destination)` is
  the query `routesearch` actually calls — every airport with a nonstop
  route both from origin and to destination. `DistanceMiles` (haversine)
  feeds the geometry prune's feasibility filter — never a price-ranking
  signal, since fare price doesn't track distance.

## `routesearch/`

The "cheap multi-leg route search" algorithm from DESIGN.md: best-first,
budget-bounded branch-and-bound over hub candidates, spending one real
`googleflights` scrape per edge, to find split-ticket/hidden-city
combinations that beat a plain A→B fare — plus round-trip and
flexible-date phases on top. Every search writes a full JSON audit trail
(`catalog.SaveRouteSearchPlan`) as it runs. No Temporal here: every
`Search*` function is one plain, synchronous Go function, pacing is
`time.Sleep`, and a "complex" multi-leg request is just a longer loop
inside one function call, not a fan-out across processes.

- **`routesearch.go`** — the core one-way algorithm, `Search(ctx, deps,
  params) (*Plan, error)`: baseline direct query, geometry-pruned/
  price-ranked hub candidates, then a best-first loop that stops once the
  frontier's best remaining bound can't beat the current price
  (`markRemaining` records everything left unqueried and why).
- **`candidates.go`** — supporting pieces the loop calls into:
  `pickCheapestFeasible`, `bestConnection` (cheapest feasible leg-2
  connection), `lowerBoundUSD` (cache-first, else distance × $/mile),
  `paretoInsert`, and `recordOffers` (persists every scraped offer so a
  later search gets a cache hit instead of a cruder guess).
- **`roundtrip.go`** — `SearchRoundTrip`: compares Google's bundled
  round-trip fare against two independent one-way `Search()` calls,
  keeping whichever total is cheaper.
- **`flexible.go`** — `SearchFlexible`: Phase A sweeps a date window with
  cheap baseline-only queries; Phase B runs the full hub search (or
  `SearchRoundTrip`) only on the date(s) that won.
- **`timeutil.go`** — real, timezone-aware elapsed-time math
  (`tripDuration`, `layover`), so `MaxHours` means actual hours, not
  wall-clock arithmetic that's wrong across time zones.

# Command-Line Entry Points

Every binary this project ships. Each section below is one subfolder.

## `collector/`

Fetches real flight fares by scraping Google Flights, in two modes: a
direct-run CLI for one-off/manual use, and a poll worker that runs
searches the agent loop (`internal/agents`, `cmd/email-intake`)
dispatches. See DESIGN.md "Collector task dispatch" for why this is a
plain poll loop against the serving store rather than a Kafka consumer
or a Temporal worker.

- **`main.go`** — flag parsing and mode dispatch. Default mode is the
  direct-run CLI: builds one `googleflights.SearchParams` call from
  `-origin`/`-destination`/`-date`/etc., writes the raw HTML response
  (`writeRaw`, standing in for the S3 raw zone), and saves parsed offers
  into the serving store (`saveOffers`). `-worker` switches to the
  poll-worker mode instead.
- **`worker.go`** — `runWorker`, the poll loop: sweep stale task claims,
  try to claim one pending `agent_tasks` row, and if it gets one, hand it
  to `runTask` in its own goroutine (capped by `-concurrency`) without
  blocking the next claim attempt.
- **`activities.go`** — `fetchFare`, the one real unit of work: wraps
  `internal/routesearch.Search` (one-way only, for now) and trims the
  resulting `Plan` down to the thin `agents.CollectRouteResult` shape a
  task's `result_json` holds.

## `email-intake/`

The reconciler for `internal/agents`' agent loop, plus a dev-only CLI
standing in for SES inbound until that's actually built. In prod, this
is what SES inbound hands a request email to; today it's three CLI modes
in one `main.go`, all against the local SQLite serving store.

- **`main.go`** — three modes: `-worker` (`runWorker`, the actual
  reconciler — lists every non-finalized `agent_requests` row and calls
  `agents.AdvanceRequest` on each); `-start` (`startRequest`, creates a
  new request from flags, standing in for "an initial request email
  arrived" — `-wait` is a dev-only blocking poll for the result, real SES
  intake wouldn't block); `-signal` (`sendFollowUp`, appends a
  plain-language constraint to an existing request, standing in for "a
  reply arrived on an existing thread").

## `routesearch/`

Runs `internal/routesearch`'s full algorithm directly to completion: one
request, no email, no agent loop, no Kafka/Temporal. Today's actual entry
point for trying the search engine against real, live fare data (see the
root `README.md`'s "Try it now").

- **`main.go`** — flag parsing, dependency wiring, and mode dispatch by
  which flags are set (plain origin/destination/date →
  `routesearch.Search`; `+ -return-date` → `SearchRoundTrip`; `+
  -date-window-days` → `SearchFlexible`). The rest of the file
  (`printSummary`/`printRoundTrip`/`printFlexible`) renders each `Plan`
  type's results to stdout — a human-readable view of the same audit
  trail `routesearch` writes to `route_search_plans`, not a separate
  computation.

## `search-api/`

The read path DESIGN.md describes: flexible, low-latency search against
the serving store, synced from the Delta Lake gold layer once `etl/` is
real. On a miss, meant to also create an `agent_requests` row directly
and return "pending, check back" instead of an empty result.

- **`main.go`** — **scaffold only**, currently just prints a placeholder
  message. Not yet wired to `internal/catalog`, and not yet creating
  `agent_requests` rows on a miss.

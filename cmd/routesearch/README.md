# Route Search CLI

Runs `internal/routesearch`'s full algorithm directly to completion: one
request, no email, no agent loop, no Kafka/Temporal — the same
direct-run shortcut `cmd/collector`'s default mode takes, just for the
multi-leg/round-trip/flexible-date search instead of a single fetch. This
is today's actual entry point for trying the search engine against real,
live fare data (see the root `README.md`'s "Try it now").

### `main.go`

Flag parsing, dependency wiring (`openflights.Load`, `catalog.Open`,
`googleflights.NewClient`), and mode dispatch by which flags are set:

- Plain `-origin`/`-destination`/`-date` → `routesearch.Search` (one-way).
- `+ -return-date` → `routesearch.SearchRoundTrip`.
- `+ -date-window-days > 0` → `routesearch.SearchFlexible` (sweeps dates
  first, then runs the one-way or round-trip search on the winner —
  round-trip mode inferred from whether `-return-date` was also given,
  with `-trip-length-days` overriding the implied trip length if set).

The rest of the file is output formatting: `printSummary`/
`printRoundTrip`/`printFlexible` render each `Plan` type's results and
candidate table to stdout, matching the shape `internal/routesearch`
already writes to `route_search_plans` — this is a human-readable view
of the same audit trail, not a separate computation. `-delay` is the
plain `time.Sleep` pacing between scrapes (`routesearch.Params.Delay`) —
interactive-use-sized here (a few seconds), vs. the minutes-scale spacing
DESIGN.md "Pacing" calls for once this runs unattended via
`cmd/collector -worker`.

# Route Search Algorithm

The "cheap multi-leg route search" algorithm from DESIGN.md: best-first,
budget-bounded branch-and-bound over hub candidates drawn from
`internal/openflights`'s route-existence graph, spending one real
`internal/googleflights` scrape per edge, to find split-ticket/hidden-city
combinations that beat a plain A→B fare — plus the round-trip and
flexible-date phases built on top of it. Every search writes a full
JSON audit trail (`internal/catalog.SaveRouteSearchPlan`) as it runs, not
just the final answer.

No Temporal here (see `cmd/collector/README.md`): every `Search*`
function below is one plain, synchronous Go function — pacing between
scrapes is `time.Sleep` (`Params.Delay`), and a "complex" multi-leg
request never fans out across processes, it's just a longer loop inside
one function call.

### `routesearch.go`

The core one-way algorithm: `Search(ctx, deps, params) (*Plan, error)`.
Step 0 queries the plain direct baseline (both the answer-of-last-resort
and the bound everything else prunes against). Step 1 pulls candidate
hubs from the graph, geometry-prunes them (pure arithmetic, no scrapes),
and price-ranks the survivors by a cached-or-distance lower bound. Step 2
is the best-first loop itself: for each candidate, in ascending
lower-bound order, stop entirely once the frontier's best remaining bound
can't beat the current best price (`markRemaining` records everything
left unqueried and why), skip to the next candidate once leg 1's own
price already forecloses winning, and otherwise spend a second query on
leg 2 and keep the combined result if it survives the Pareto set. `Plan`/
`CandidateOutcome`/`LegOutcome` are the audit-trail shapes this all
writes into, matching DESIGN.md's `route_search_plans` JSON exactly.

### `candidates.go`

The supporting pieces `routesearch.go`'s loop calls into:
`pickCheapestFeasible` (cheapest offer that fits `MaxHours`),
`bestConnection` (cheapest leg-2 offer that connects to a given leg-1
within `[MinLayoverMinutes, MaxLayoverMinutes]` and keeps the whole trip
under `MaxHours`), `lowerBoundUSD` (cache-first, else distance × $/mile),
`paretoInsert` (keeps the final result set to genuinely non-dominated
price/duration pairs), and `recordOffers` (persists every scraped offer,
not just the ones kept, so a later search gets a cache hit instead of a
cruder guess — DESIGN.md "Collection scope"'s "repeat value comes from
accumulation," made literal).

### `roundtrip.go`

`SearchRoundTrip` — DESIGN.md "Round trips and flexible dates"'
three-way comparison: Google's own bundled round-trip fare vs. two
independent one-way `Search()` calls (outbound and return each get their
own full hub search), keeping whichever total is cheaper
(`combineRoundTrip`). `RoundTripPlan`'s `OutboundPlanID`/`ReturnPlanID`
point at those two one-way `Plan`s, already persisted separately with
their own full candidate tables — this file's own audit trail only
records the round-trip-specific decision on top.

### `flexible.go`

`SearchFlexible` — the two-phase flexible-date algorithm. Phase A sweeps
a bounded date window around the target date with baseline-only queries
(cheap: one query per date point, no hub search — `sweepOneDate`) to find
which date is actually cheap. Phase B then runs the *full* hub search (or
`SearchRoundTrip`) only on the date(s) that won, not on every date in the
grid — spending the expensive part of the budget only after the cheap
part has narrowed where to spend it.

### `timeutil.go`

Real, timezone-aware elapsed-time math — the part that makes `MaxHours`
mean actual hours, not wall-clock arithmetic that's wrong across time
zones. `tripDuration` and `layover` both resolve each airport's IANA
timezone via `internal/openflights`'s `Graph` (falling back to UTC if
unknown) before subtracting two `time.Time`s built from a `Segment`'s
raw `[year,month,day]`/`[hour,minute]` fields.

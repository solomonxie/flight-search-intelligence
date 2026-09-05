# Implementation Plan

Concrete, checkable tasks for the work DESIGN.md has already decided but
not yet built. One task = one focused PR-sized unit; pick the next
unchecked box, implement just that, check it off and cite the commit.
Not a design doc — see DESIGN.md for the *why* behind each group
(section named per group below).

## Done

- [x] Force leg-level queries nonstop-only, not Google's own connections
      (`d7367aa`) — DESIGN.md "Our hops vs. Google's hops"
- [x] Fix `$0` fares poisoning the price cache/ranking (`01fe022`)
- [x] Show the direct route alongside hub candidates in `-dry-run`
      (`1a62406`)

## Baggage as a query input

DESIGN.md "Baggage cost is a query input, not a scoring adjustment."
Pure plumbing — no scoring-logic changes anywhere in this group.

- [ ] `googleflights.SearchParams`: add `CheckedBags *int` (and
      `CarryOnBags *int` if needed), thread into `toQuery()` →
      `Query.CheckedBags`/`CarryOnBags` (already wired to the protobuf,
      `protobuf.go:126-127`)
- [ ] `routesearch.Params`: add `CheckedBags int`, pass through on every
      `searchOffers` call in `search.go` (baseline, leg1, leg2) and
      `roundtrip.go`/`flexible.go`
- [ ] `agents.Spec`: add `CheckedBags int`, thread into
      `CollectRouteRequest` in `decide.go`
- [ ] `cmd/routesearch`: add a `-checked-bags` flag
- [ ] Verify live: same route/date with `CheckedBags` 0 vs. 2 on a
      budget-carrier itinerary, confirm `Offer.Price` actually rises —
      don't just trust the field exists

## Agent-loop `Spec` field parity

DESIGN.md "Spec's concrete fields, precisely." `DecideNextAction` is
still a stub (no real LLM call) — these are the typed-field gaps that
need to exist *before* that call can ever set them.

- [ ] `agents.Spec`: add `MinLayoverMinutes`/`MaxLayoverMinutes`, thread
      into `CollectRouteRequest`/dispatch in `decide.go` (already on
      `routesearch.Params`, just missing from `Spec`)
- [ ] `routesearch.Params`: add `MaxPrice int`, enforce in
      `pickCheapestFeasible`/`bestConnection` (`scoring.go`); then add to
      `googleflights.SearchParams` → `Query.MaxPrice` (already wired,
      `protobuf.go:125`) and up to `agents.Spec`
- [ ] `agents.Spec`: add a date-window shape (reuse
      `FlexibleParams`' window/step fields) so `SearchFlexible` becomes a
      dispatchable tool from the agent loop — today `DecideNextAction`
      only ever builds a `CollectRouteRequest` (one exact date)

## Deeper itineraries: N-hop search + hop-country constraints

DESIGN.md "Deeper itineraries: N-hop search and hop-country
constraints." The big one — do the state-model change once, carefully;
everything else in this group builds on it.

- [ ] `routesearch.Params`: add `MaxLegs int`, default 1 (today's 1-stop
      behavior unchanged when unset)
- [ ] Generalize `search.go`'s fixed `A→hub→B` loop into label-setting
      search: state `(node, arrival_time, price_so_far, duration_so_far,
      legs_so_far)`, Pareto-dominance pruning per node, `legs_so_far`
      as a hard cutoff at `MaxLegs`
- [ ] Decide + implement `QUERY_BUDGET` scaling for `MaxLegs > 1` (flat
      default today plausibly too tight past 2 legs — DESIGN.md flags
      this as still open)
- [ ] `routesearch.Params`: add `MaxCountries int` and
      `ExcludedCountries []string`
- [ ] Wire both into `candidates.go`'s geometry-prune stage — country
      already available per airport via `openflights.Airport.Country`,
      no new data source needed
- [ ] Extend the audit trail (`CandidateOutcome`/`Plan`) to record legs
      and countries transited per kept candidate, not just hub + price
- [ ] Update self-transfer risk reporting from a single bool to a real
      transfer count once `MaxLegs > 2` is reachable

## Backlog — proposed, not yet a DESIGN.md decision

- [ ] Multi-airport origin/destination (e.g. treat PEK/PKX/NAY as
      interchangeable, price each, keep the cheapest) — needs a
      DESIGN.md write-up and an explicit decision first, not started

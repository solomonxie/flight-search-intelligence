# Implementation Plan

Dependency-ordered phases, per the `project-planning` skill. Each phase's
checklist is dependency-ordered too; a phase this doc marks done stays
that way in place — no separate "done" section. Not a design doc — see
DESIGN.md for the *why* behind each phase (section named per phase
below).

## Phase 0: Core route search engine — done

Everything downstream (baggage/filters, N-hop, the agent loop) dispatches
into this; it had to exist first; it does.

- [x] Google Flights scraper as collector, parsed into the local SQLite
      serving store — `94f259e`, `6ed63ed`
- [x] Cheap multi-leg (1-stop hub) route search: bounded, budgeted
      A*-style candidate search — DESIGN.md "Exploration algorithm"
      (`853adda`)
- [x] Direct A→B baseline included in the candidate rank list —
      `71cfd1a`
- [x] Round-trip (3-baseline) and flexible-date sweep search, plus
      stopover labeling — `f4ef9ed`
- [x] Eligibility constraints (`AvailableFrom`/`Until`,
      `ExcludeWeekdays`, `BlackoutDates`) on the flexible-date winner —
      `4109719`
- [x] Cache-first offers with `-force-refresh` and a dry-run preview;
      pace only live scrapes, not cache hits — `edca533`, `87b69de`,
      `c437cc6`
- [x] Force leg-level queries nonstop-only, never Google's own
      connections — DESIGN.md "Our hops vs. Google's hops" (`d7367aa`)
- [x] Fix `$0` fares poisoning the price cache/ranking — `01fe022`
- [x] Show the direct route alongside hub candidates in `-dry-run` —
      `1a62406`
- [x] Schema ownership handed to Flyway migrations; Go never manages
      DDL — DESIGN.md "Schema ownership" (`e0cbf12`, `0b6bc2a`,
      `34cf48b`)

## Phase 1: Agent-loop plumbing — done (prototype, ahead of schedule)

DESIGN.md "Agent loop" flags this as built *ahead* of the sequencing it
originally called for ("no LLM gets wired in until the deterministic Go
side is solid") — the loop's mechanics exist, but `DecideNextAction` is
still a deterministic stub, not a real LLM call. That gap is Phase 2
below, not this phase.

- [x] Store-backed agent loop, first draft (no Temporal) — `b3f4e51`
- [x] Chain agent-loop steps through Kafka (`agent-decisions` /
      `search-tasks`), split the loop out of `email-intake` — `cf1cbcb`
- [x] `agent_requests`/`agent_tasks` schema — Flyway `V003`–`V005`
- [x] `cmd/agent-worker`, `cmd/collector -worker`, and the one-shot
      `cmd/email-intake` CLI — same commits above
- [x] `flight_offers_cache` table; uniform `created_at`/`updated_at`
      naming — `7d8090d`

## Phase 2: Agent-loop decision core — LLM input/output, decide-next-step — done

DESIGN.md "Agent loop" steps 1-2 and "Spec's concrete fields." Moved
ahead of baggage/N-hop on purpose: those extend `routesearch.Params`,
which nothing downstream can actually exercise from an email until the
agent loop can turn a request into a `Spec` and act on it. Depends on
Phase 1 (the loop to plug into).

- [x] `agents.LLMClient`: adapter interface (one chat/structured-output
      method) so `DecideNextAction` and spec-formation below never call a
      specific provider's SDK directly — `internal/agents/llm.go`
- [x] Two backends behind it: OpenAI (API key from env) for prod
      (written, not exercised live — no key in this dev environment), and
      a local Ollama backend (`http://localhost:11434`, no key) for
      simulation/dev — selectable via `LLM_BACKEND` env var, not a
      compile-time branch — `internal/agents/ollama.go`,
      `internal/agents/openai.go`
- [x] `agents.Spec`: add `MinLayoverMinutes`/`MaxLayoverMinutes` (already
      on `routesearch.Params`, just missing from `Spec`) — and
      `MaxPrice`, done alongside it
- [x] `routesearch.Params`: add `MaxPrice int`, enforce in
      `pickCheapestFeasible`/`bestConnection` (`scoring.go`); wired to
      `googleflights.SearchParams` → `Query.MaxPrice` (already encoded)
      and up to `agents.Spec`
- [x] `agents.Spec`: add a date-range shape so date search becomes a
      dispatchable tool from the agent loop, not just
      `CollectRouteRequest` — superseded its own first cut (a single
      center date + `WindowDays`/`StepDays`) with independent
      `MinDepartDate`/`To` and `MinReturnDate`/`To` ranges plus
      `MinRoundTripDate`/`To` (an outer eligibility bound, e.g. limited paid
      leave — distinct from the ranges, which only control what gets
      *priced*) once a live run showed the coupled single-window model
      couldn't express "depart and return each genuinely independently
      flexible" or "make sure a hard leave-length constraint is never
      violated." `routesearch.SearchDateRange` (`daterange.go`) prices
      every combination in the ranges (a real depart x return grid —
      window² queries, an accepted cost tradeoff, not scaled back to
      window+length automatically: prefer the still-available
      `FlexibleParams.TripLengthDays` coupled case when the trip length
      is actually fixed, since that's only window queries).
      `dispatch.runDateRangeSearch` routes a request with a genuine
      range on either end to it and reports which combination won via
      `CollectRouteResult.ChosenDepartDate`/`ChosenReturnDate` — verified
      live end to end (`TestAgentLoop_FlexibleDates`,
      `TestAgentLoop_NextMonthCrossesYearBoundary`), a dispatch-level
      test for both trip types (`internal/dispatch/search_test.go`), and
      a routesearch-level test proving the eligibility bound actually
      excludes a cheaper-but-ineligible combination in favor of the
      cheapest eligible one (`internal/routesearch/daterange_test.go`)
- [x] Real spec formation: turn free-text (the `-start` request text,
      `-signal` follow-ups) into `Spec`'s concrete fields +
      `SoftConstraints` via `LLMClient` — `agents.FormSpec`
      (`internal/agents/formspec.go`), one function for both callers;
      replaced `cmd/email-intake` building `Spec` from CLI flags and the
      old `AppendSoftConstraint`/`NewRequest` (removed, both dead code
      once `FormSpec` took over)
- [x] `agents.Action`: add `ActionAskUser` (+ a `Question` field on
      `Decision`) — DESIGN.md step 2's fourth move, for a genuinely
      underspecified spec, distinct from `ActionDefer`
- [x] Replace the `DecideNextAction` stub with a real `LLMClient` call:
      given `Spec` + round history, choose dispatch (with what
      arguments) / ask-user / exclude-and-retry-with-new-filters /
      finalize — the judgment call DESIGN.md's loop step 4-5 describes.
      `ActionDefer` stays unproduced: the system prompt tells the model
      never to choose it, since its wake-sweep still isn't built
- [x] Verify live against the local Ollama backend (`qwen2.5:7b`): one
      deliberately underspecified request (asked "What is the departure
      airport?", parked in the new `awaiting_user` status), one complete
      request (dispatched immediately, finalized with a real scraped
      offer), one soft-constraint violation (self-transfer result
      correctly recognized as not "good enough" and retried rather than
      finalized). Real transcripts (prompt + raw reply) captured for all
      three — see conversation, not reproduced here

## Phase 3: Deeper itineraries — N-hop search + hop-country constraints — done

DESIGN.md "Deeper itineraries: N-hop search and hop-country
constraints" (designed: `ac24419`). Extends Phase 0's search directly.
Depends on Phase 0.

- [x] `routesearch.Params`: add `MaxLegs int`, default 1 (today's 1-stop
      behavior unchanged when unset) — `Search` dispatches to `searchNHop`
      (`nhop.go`) only when `MaxLegs > 1`; the original loop is untouched
      otherwise (verified: `TestSearch_MaxLegsOneUnchanged`)
- [x] Generalize `search.go`'s fixed `A→hub→B` loop into label-setting
      search: state `(node, price_so_far, duration_so_far, last offer)`,
      Pareto-dominance pruning per node (`dominated`/`keepNonDominated`),
      `legs_so_far` (`nhopLabel.legs()`) as a hard cutoff at `MaxLegs` —
      `nhop.go`. `openflights.Graph.Neighbors` added alongside
      `CandidateHubs`: an intermediate hop doesn't need a direct route to
      the final destination the way a 1-stop hub does, so N-hop candidate
      generation needed the plain adjacency, not the destination-filtered
      one. Turned out to need one more prune `CandidateHubs` gets for
      free: a hub-connectivity filter (`minHubOutDegree`) — without one,
      a well-connected origin's raw `Neighbors` fans out into dead-end
      regional airports that look deceptively cheap by raw distance
      alone and starve the flat query budget before a real path ever
      gets a second hop
- [x] `QUERY_BUDGET` stays flat, not scaled by `MaxLegs` — asked; a
      high-degree origin genuinely can starve a deep search under a flat
      budget (observed live: YVR's ~100+ major-hub neighbors alone ate a
      200-query budget without ever reaching hop 2), an accepted
      consequence of this choice, not a bug — a caller wanting deeper
      search from a well-connected origin raises `QueryBudget` itself
- [x] `routesearch.Params`: add `MaxCountries int` and
      `ExcludedCountries []string`
- [x] Wire both into `candidates.go`'s geometry-prune stage —
      `excludesCountry`/`countDistinctCountries`, applied in both
      `ResolveCandidates` (1-stop) and `nhop.go`'s `candidateEdges`
- [x] Extend the audit trail: `Plan.NHopRanked []NHopOutcome` — a sibling
      to `CandidatesRanked`, not a forced fit into its leg1/leg2 shape,
      since an N-hop edge is "from this partial path to this airport,"
      not a fixed two-leg row
- [x] Self-transfer risk reporting: `Result.TransferCount` added
      alongside the existing `SelfTransfer` bool (`len(Path)-2` —
      distinguishes one hub from a four-hop combo, same risk category,
      different magnitude) — `cmd/routesearch/print.go` surfaces it
- [x] `cmd/routesearch`: `-max-legs`, `-max-countries`,
      `-excluded-countries` flags; `printNHopCandidates` (print.go) as
      `printCandidates`'s `NHopRanked` counterpart
- [x] Verified: `internal/routesearch/nhop_test.go` — `MaxLegs` 0/1 take
      the untouched original path (`NHopRanked` stays empty); a
      synthetic-offer scenario (expensive baseline, cheap subsequent
      legs) proves the search actually chains hops into a genuine
      multi-leg result cheaper than the baseline, respects `MaxLegs` and
      `QueryBudget`, and never revisits an airport within one itinerary

## Phase 4: Baggage and other per-request search filters — done

DESIGN.md "Baggage cost is a query input, not a scoring adjustment"
(designed: `3d0cb65`). Pure plumbing — no scoring-logic changes anywhere
in this phase. Independent of Phase 3; both extend Phase 0's `Params`
surface.

- [x] `googleflights.SearchParams`: add `CheckedBags *int`, thread into
      `toQuery()` → `Query.CheckedBags` (already wired to the protobuf,
      `protobuf.go:126-127`) — `CarryOnBags` stayed unneeded, nothing
      asked for it
- [x] `routesearch.Params`: add `CheckedBags int` (`checkedBagsPtr`
      mirrors `maxPricePtr`'s "0 = not specified" convention), pass
      through on every `searchOffers` call in `search.go` (baseline,
      leg1, leg2), `roundtrip.go`'s bundled query, and `flexible.go`'s
      `scanPair` — also threaded into `nhop.go`'s two `searchOffers`
      calls, not explicitly named above but the same gap under N-hop
      search (Phase 3)
- [x] `agents.Spec`: add `CheckedBags int`, thread into
      `CollectRouteRequest` (`toCollectRouteRequest`, `task.go`,
      `fillDispatchDefaults` in `decide.go`) and both LLM prompts
      (`decideSystemPrompt`'s dispatch-argument contract,
      `formSpecSystemPromptTemplate`'s field list) so free text like "2
      checked bags" actually reaches a dispatched search, not just a
      CLI flag. Excluded from `normalizeDefaults`, same as `MaxPrice`: 0
      is CheckedBags' own legitimate value ("not mentioned"), never "not
      filled in yet"
- [x] `cmd/routesearch`: `-checked-bags` flag
- [x] `agents.bookingLink`: also carries the dispatched request's
      `CheckedBags` — found while wiring this phase, not in the original
      checklist: the booking link a traveler clicks priced the same
      itinerary Google returned, so a link that dropped the bag count
      could show a different (lower) price than what was just quoted
- [x] **Found while verifying live, fixed as part of this phase, not in
      the original checklist:** `flight_offers_cache`'s lookup key was
      (origin, destination, depart_date, return_date) only —
      `CheckedBags` didn't participate, so a `CheckedBags:2` search made
      within `offersCacheFreshness` (24h) of an existing `CheckedBags:0`
      scrape for the same route/date would silently reuse the wrong
      (bag-mismatched) prices. Flyway `V009` adds a `checked_bags` column
      (default 0, backward-compatible with existing rows) and folds it
      into the lookup index; `catalog.CachedOffers`/`SaveOffersCache`
      take it as a parameter now
- [x] Verify live: DEN→LAS 2026-10-15, `-checked-bags 0` vs. `2`,
      `-max-hours 5` (forces the nonstop over the cheaper-but-11h
      self-transfer, whose fare Google doesn't reprice for bags on this
      carrier — DESIGN.md's noted Google-side limitation, not a bug
      here): `$117` → `$145`, confirmed against real scraped
      `Offer.Price`, not just that the field exists. Also confirmed the
      wire-level encoding differs (`SearchURL` embeds a distinct `tfs`
      payload per bag count) and that a full `Search()` run picks up the
      new cache key correctly

## Phase 5: Wide fuzzy-range search, preference-aware pruning, trace files, a shared rate limiter — done

DESIGN.md "Wide fuzzy-range search, preference-aware pruning, a trace
file, and a shared rate limiter" — supersedes the old "Fixed-length
trip, wide-open window" backlog item below with the actual requirement
(any width, on any fuzzy dimension), not one example of it. Depends on
Phase 2 (`Spec`/`FormSpec`/`dispatch.runSearch`) and Phase 3
(`ExcludedCountries`/`MaxCountries`).

- [x] `routesearch.FlexibleParams`: `TripLengthDays` becomes a tolerance
      range (`TripLengthMaxDays`, `TripLengthStepDays` — 0 defaults to
      1, every day, never an error); add explicit `DepartFrom`/`DepartTo`
      as an alternative to center date + `WindowDays` — `27c866b`
- [x] Fix: `SearchFlexible`'s Phase A has no `QueryBudget` check at all
      today (unlike `SearchDateRange`) — add the same
      `withinBudget`/`queriesUsed` guard — `27c866b`
- [x] `routesearch.DateRangeParams`: add `BlackoutDates`, threaded into
      its `eligibility{...}` the same way `FlexibleParams` already does —
      `27c866b`
- [x] Pre-flight cost estimate for the new shapes: extend
      `cmd/routesearch/confirm.go`'s pattern (`FlexibleParams.
      EstimatedCombinations`, `-depart-from`/`-depart-to`/
      `-trip-length-max-days`/`-trip-length-step-days` flags — `61214d9`);
      extend `DecideNextAction`'s existing "disclose default assumptions
      on first `ask_user`" to also mechanically disclose a wide fuzzy
      range's estimated combination count — `ed61243`
- [x] Time-boxed spike: a Google Flights bulk price-calendar/graph
      endpoint (same reverse-engineering approach as
      `internal/googleflights/protobuf.go`) — found (`GetCalendarGraph`,
      confirmed via krisukox/google-flights-api's `GetPriceGraph`), not
      adopted: needs session cookies + a time-stamped anti-abuse token
      (this project's client is stateless today) and a second,
      independently-fragile reverse-engineered payload format pinned to
      a dated internal build id. Recorded in DESIGN.md; `scanPair`'s
      one-scrape-per-date loop stays the implementation — `ed61243`
- [x] `agents.Spec`/`CollectRouteRequest`: add `MinTripLengthDays`/
      `MaxTripLengthDays`/`TripLengthStepDays`, `ExcludedCountries`,
      `MaxCountries`, `BlackoutDates`; forward in
      `Spec.toCollectRouteRequest` — `ed61243`
- [x] `FormSpec`'s prompt: teach all six new fields, incl. trip-length
      tolerance as an alternative to `MinReturnDate`/`MaxReturnDate`
      (mutually exclusive — `validateDates` resolves a contradiction the
      same way it already resolves a backwards return window) — `ed61243`
- [x] `decide.go`: round-trip's required-field check accepts a trip-length
      range as an alternative to `MinReturnDate`; add new fields to
      `decideSystemPrompt`'s dispatch-argument contract and
      `fillDispatchDefaults` — `ed61243`
- [x] `dispatch.runSearch`: new `tripLengthFlex` branch →
      `runFlexibleTripLengthSearch` (factored the origin/destination
      candidate-merge loop `runDateRangeSearch` already had into the
      shared `runFlexibleAcrossCandidates`); thread
      `ExcludedCountries`/`MaxCountries`/`BlackoutDates` into every
      `Params`/`DateRangeParams`/`FlexibleParams` literal in the file via
      the new `baseParams` helper, not just the new shape — `ed61243`
- [x] `catalog.GetRouteSearchPlan` (read-side counterpart to
      `SaveRouteSearchPlan`); `internal/tracefile.Write` (mirrors
      `cmd/collector/main.go`'s `writeRaw` idiom); every routesearch
      entry point's `savePlan` helper writes its `Plan`/`FlexiblePlan` to
      a trace file alongside the existing DB save (once final, not on
      the initial "running" checkpoint); the agent loop's finalize step
      (`agents.writeCombinedTraceFile`) writes one combined file joining
      the conversation trail with each round's fetched
      `RouteSearchPlan` — `b8bab80`
- [x] `internal/ratelimit`: `Limiter` interface + `FixedWindow`
      (multi-granularity, mutex-guarded) implementation; wired into
      `googleflights.Client`'s one HTTP call site, one shared
      `processSharedLimiter` instance across all four `NewClient()` call
      sites — `d376415`
- [x] Tests: `flexible_test.go` (new — `27c866b`, covers the tolerance
      range/explicit window/budget guard) and a `daterange_test.go`
      `BlackoutDates` case (`27c866b`) were already done;
      `dispatch/search_test.go` new cases (`TestRunSearch_Flexible_TripLength`
      for the tripLengthFlex branch, `TestBaseParams_ThreadsHopCountryAndBlackout`
      and `TestRunDateRangeSearch_ThreadsBlackoutDates` for hop-country/
      blackout threading); new `decide_test.go` covering
      `missingRequiredFields`'s trip-length-as-return-date-alternative,
      `fillDispatchDefaults`'s forward-fill of all six new fields,
      `estimateDateCombinations`'s three shapes, `isFirstAskUser`,
      `joinMissing`; `formspec_test.go`'s `TestValidateDates` gained the
      return-date-vs-trip-length mutual-exclusivity case; new
      `tracefile_test.go` (round-trip, dir creation, overwrite-not-append,
      `Dir()` default) and `ratelimit_test.go` (window enforcement,
      independent multi-window caps, context cancellation, and a
      concurrent-goroutines case run under `-race`)

## Backlog — proposed, not yet a DESIGN.md decision

Not a phase: no DESIGN.md section has decided these yet, so there's
nothing dependency-ordered to schedule until one exists.

- [ ] **Infra — provisioning the agent-loop stack for production.**
      DESIGN.md "Infra" (top-level architecture) and "Schema ownership"
      target section — the only backlog item whose shape a DESIGN.md
      section *has* already decided (Terraform EC2 + Ansible-installed
      self-managed Kubernetes + Helm); deprioritized rather than
      undecided, moved here since there's nothing worth deploying
      continuously yet. `terraform/` is still an empty placeholder and
      `ansible/` only has the `mac_dev` dev-machine role today.
  - [ ] `terraform/`: EC2 instances + networking for the Kubernetes
        fleet
  - [ ] `ansible/`: a new role to install/join self-managed Kubernetes
        on that fleet (separate from `mac_dev`, which configures a
        developer's own Mac, not the fleet)
  - [ ] Docker image per component (`email-intake`, `agent-worker`,
        `collector`, `search-api`, serving-sync) — one per existing
        `cmd/` binary
  - [ ] Helm chart per component, plus the Postgres Flyway pre-install/
        pre-upgrade hook Job described under "Schema ownership"
  - [ ] Strimzi Kafka and Postgres as self-managed workloads on the
        same cluster
- [ ] Multi-airport origin/destination (e.g. treat PEK/PKX/NAY as
      interchangeable, price each, keep the cheapest) — needs a
      DESIGN.md write-up and an explicit decision first, not started
- [ ] Seat/cabin filters (legroom, seat class) as additional query
      inputs — same "query input, not a scoring adjustment" shape as
      Phase 4's baggage, but no DESIGN.md write-up exists yet
- Fixed-length trip, wide-open window as its own agent-loop request
  shape — superseded by Phase 5 above (generalized to any width, on any
  fuzzy dimension, not just this one example), not a backlog item anymore

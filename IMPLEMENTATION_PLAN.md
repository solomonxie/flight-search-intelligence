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
- [x] `agents.Spec`: add a date-window shape (reuse `FlexibleParams`'
      window/step fields) so `SearchFlexible` becomes a dispatchable tool
      from the agent loop, not just `CollectRouteRequest` — `WindowDays`/
      `StepDays` on `Spec` and `CollectRouteRequest`; `FormSpec` only sets
      `WindowDays` from an explicit flexibility signal ("give or take N
      days"), never from a vague date phrase alone (that stays
      `DepartDate`'s own earliest-date rule); `dispatch.runFlexibleSearch`
      routes a nonzero `WindowDays` to `SearchFlexible` and reports which
      date won via `CollectRouteResult.ChosenDepartDate`/`ChosenReturnDate`
      — verified live end to end (`TestAgentLoop_FlexibleDates`), plus a
      dispatch-level test for both one-way and round-trip
      (`internal/dispatch/search_test.go`)
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

## Phase 4: Baggage and other per-request search filters

DESIGN.md "Baggage cost is a query input, not a scoring adjustment"
(designed: `3d0cb65`, not yet built). Pure plumbing — no scoring-logic
changes anywhere in this phase. Independent of Phase 3; both extend
Phase 0's `Params` surface.

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

## Phase 5: Infra — provisioning the agent-loop stack for production

DESIGN.md "Infra" (top-level architecture) and "Schema ownership"
target section. Shape is already decided (Terraform EC2 + Ansible-
installed self-managed Kubernetes + Helm), just unbuilt — `terraform/`
is still an empty placeholder and `ansible/` only has the `mac_dev`
dev-machine role today. Depends on nothing above functionally (it's
deployment, not logic), placed last because there's nothing worth
deploying continuously until Phases 1-4 give the loop real judgment.

- [ ] `terraform/`: EC2 instances + networking for the Kubernetes fleet
- [ ] `ansible/`: a new role to install/join self-managed Kubernetes on
      that fleet (separate from `mac_dev`, which configures a
      developer's own Mac, not the fleet)
- [ ] Docker image per component (`email-intake`, `agent-worker`,
      `collector`, `search-api`, serving-sync) — one per existing
      `cmd/` binary
- [ ] Helm chart per component, plus the Postgres Flyway pre-install/
      pre-upgrade hook Job described under "Schema ownership"
- [ ] Strimzi Kafka and Postgres as self-managed workloads on the same
      cluster

## Backlog — proposed, not yet a DESIGN.md decision

Not a phase: no DESIGN.md section has decided these yet, so there's
nothing dependency-ordered to schedule until one exists.

- [ ] Multi-airport origin/destination (e.g. treat PEK/PKX/NAY as
      interchangeable, price each, keep the cheapest) — needs a
      DESIGN.md write-up and an explicit decision first, not started
- [ ] Seat/cabin filters (legroom, seat class) as additional query
      inputs — same "query input, not a scoring adjustment" shape as
      Phase 4's baggage, but no DESIGN.md write-up exists yet

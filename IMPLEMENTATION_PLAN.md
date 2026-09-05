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

## Phase 2: Agent-loop decision core — LLM input/output, decide-next-step

DESIGN.md "Agent loop" steps 1-2 and "Spec's concrete fields." Moved
ahead of baggage/N-hop on purpose: those extend `routesearch.Params`,
which nothing downstream can actually exercise from an email until the
agent loop can turn a request into a `Spec` and act on it. Depends on
Phase 1 (the loop to plug into).

- [ ] `agents.LLMClient`: adapter interface (one chat/structured-output
      method) so `DecideNextAction` and spec-formation below never call a
      specific provider's SDK directly
- [ ] Two backends behind it: OpenAI (API key from env) for prod, and a
      local Ollama backend (`http://localhost:11434`, no key) for
      simulation/dev — selectable via config/flag, not a compile-time
      branch. Ollama's already installed and running locally
      (`brew services start ollama`) with `llama3.1:8b`/`qwen2.5:7b`
      pulled, ready to point this at
- [ ] `agents.Spec`: add `MinLayoverMinutes`/`MaxLayoverMinutes` (already
      on `routesearch.Params`, just missing from `Spec`)
- [ ] `routesearch.Params`: add `MaxPrice int`, enforce in
      `pickCheapestFeasible`/`bestConnection` (`scoring.go`); then add to
      `googleflights.SearchParams` → `Query.MaxPrice` (already wired,
      `protobuf.go:125`) and up to `agents.Spec`
- [ ] `agents.Spec`: add a date-window shape (reuse `FlexibleParams`'
      window/step fields) so `SearchFlexible` becomes a dispatchable tool
      from the agent loop, not just `CollectRouteRequest`
- [ ] Real spec formation: turn free-text (the `-start` request text,
      `-signal` follow-ups) into `Spec`'s concrete fields +
      `SoftConstraints` via `LLMClient` — replaces `cmd/email-intake`
      building `Spec` straight from CLI flags and `AppendSoftConstraint`
      appending raw text unread
- [ ] `agents.Action`: add `ActionAskUser` (+ a `Question` field on
      `Decision`) — DESIGN.md step 2's fourth move, for a genuinely
      underspecified spec, distinct from `ActionDefer`
- [ ] Replace the `DecideNextAction` stub with a real `LLMClient` call:
      given `Spec` + round history, choose dispatch (with what
      arguments) / ask-user / exclude-and-retry-with-new-filters /
      finalize — the judgment call DESIGN.md's loop step 4-5 describes
- [ ] Verify live against the local Ollama backend: one deliberately
      underspecified request (asks a clarifying question), one complete
      request (dispatches straight away), one where round 1's result
      should be excluded and retried with tighter filters

## Phase 3: Deeper itineraries — N-hop search + hop-country constraints

DESIGN.md "Deeper itineraries: N-hop search and hop-country
constraints" (designed: `ac24419`, not yet built). Extends Phase 0's
search directly — do the state-model change once, carefully; everything
else in this phase builds on it. Depends on Phase 0.

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

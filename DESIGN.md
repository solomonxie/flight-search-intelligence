# Design

Target architecture. See `README.md` "Status" for what's actually built
vs. still scaffold.

Deployment: AWS EC2, self-managed (not managed services like EKS/RDS/
EMR/MSK) — Terraform provisions the EC2 instances and networking,
Ansible configures them (installs/joins Kubernetes, per the `ansible`
skill's role conventions), every component ships as a Docker image,
and Kubernetes (self-managed control plane on that EC2 fleet) runs
them via Helm charts. This replaces the AWS CDK stack previously under
`infra/`, which was removed by request. No `docker-compose.yml` exists
right now either — it described a standalone Postgres service nothing
actually depended on yet (local dev uses SQLite today — see "Local
development"); removed rather than leaving unused scaffold, revisit
once a service genuinely needs it locally.

## Data flow

Collection is **on-demand, not a mass crawl**: nothing gets scraped
until a user emails a request for a specific route/date. This bounds
scrape volume to actual requests (see "Collection scope" below).

```
 user email                search-api miss
(route+dates,             (route/date not yet
 +any follow-up email)      collected)
     │                            │
     ▼                            │
┌─────────────┐                   │
│ email intake │                  │
│ (SES inbound)│                  │
└──────┬───────┘                  │
       │ create a row, publish a  │ create a row, publish a
       │ DecisionTrigger (simple  │ DecisionTrigger (simple
       ▼ case: no LLM needed)     │ case, same as email)
      ┌──────────────────────────────┐
      │      cmd/agent-worker        │
      │  consumes agent-decisions;   │
      │  see "Agent loop: LLM        │
      │  drives the search" below.   │
      │  Decides: dispatch a tool    │
      │  call, defer, or finalize —  │
      │  finalize = publish nothing  │
      │  further, chain just ends.   │
      └───────────────┬───────────────┘
                       │ dispatch (a tool call) =
                       │ INSERT into agent_tasks +
                       │ publish {task_id} onto
                       │ search-tasks
                       ▼
      ┌──────────────────────────────┐
      │         cmd/collector        │
      │  -worker: consumes           │
      │  search-tasks, runs the      │
      │  fetch, writes the result    │
      └───────────────┬───────────────┘
                       │ publishes {request_id}
                       │ back onto agent-decisions —
                       │ wakes the next round; may
                       │ loop back to dispatch again
                       │ with different parameters,
                       │ or continue ▼
                       ▼
      ┌──────────────────────────────┐
      │        internal/routesearch  │
      │ fetches fares with its own   │
      │ retry/pacing; multi-leg      │
      │ ("complex") search runs      │
      │ in-process, one task, not a  │
      │ distributed fan-out (see     │
      │ "Cheap multi-leg route       │
      │ search")                     │
      └──────┬────────────────┬──────┘
             │ raw result     │ notify when done
             ▼                ▼
      ┌─────────────┐  ┌────────────────────┐
      │ S3 raw zone │  │ email reply AND     │
      └──────┬──────┘  │ search-api availab. │
             │          └────────────────────┘
             │ periodic batch
             ▼
      ┌────────────────────────────────────┐
      │ Delta Lake (bronze → silver, on S3) │
      │ accumulates requested-route history │
      └────────────────┬─────────────────────┘
                        │ dbt (Spark SQL)
                        ▼
      ┌────────────────────────────────────┐
      │ Delta Lake gold — fare trends per   │
      │ previously-requested route          │
      └────────────────┬─────────────────────┘
                        │ sync job
                        ▼
                 ┌───────────────┐
                 │   Postgres     │
                 │ (serving store)│
                 └───────┬────────┘
                         ▼
                 ┌───────────────┐
                 │ cmd/search-api │
                 └───────────────┘
```

## Agent loop: LLM drives the search, Go stays narrow

**Status: built and live-verified, including the decision core.** The
loop mechanics (`internal/agents`, `internal/kafka`, `cmd/agent-worker`,
`cmd/collector -worker`, `cmd/email-intake`) were built first, ahead of
the sequencing this section originally called for ("no LLM gets wired in
until the deterministic Go side is solid"), at explicit request, as a
prototype of the control flow — `DecideNextAction` was a deterministic
stub through that phase. `DecideNextAction` and spec formation
(`agents.FormSpec`) are now real `agents.LLMClient` calls (see
`internal/agents/decide.go`/`formspec.go`), verified live against a
local Ollama backend across an underspecified request (asked a
clarifying question), a complete request (dispatched and finalized on
one round), and a soft-constraint violation (a self-transfer result
correctly judged as not "good enough" and retried rather than
finalized) — IMPLEMENTATION_PLAN.md Phase 2. Still open: `SearchFlexible`
as a second dispatchable tool (`Spec`'s date-window field), and
`ActionDefer`'s wake-sweep (the decision prompt tells the model never to
choose `defer`, so this stays a dead path on purpose for now).

**The reframing**: the backend is not "deterministic pipeline with an
LLM bolted on at the email boundary" — it's an **agent loop**. An LLM
takes the user's request (and any later follow-up email), turns it into
concrete search parameters, decides what to run, and — this is the part
a fixed pipeline can't do — **examines the result and decides whether
it's actually good enough**, against constraints that were never going
to survive being turned into typed Go struct fields. Two examples,
verbatim the kind of judgment call this is for:

- The algorithm reports Jan 7–30 as the cheapest window. The agent
  knows (from the original email) the trip is over Christmas and that
  matters to the user — cheapest-but-misses-Christmas isn't a candidate
  at all, not a worse-ranked one. No `Params` field says "don't skip the
  holiday the user is traveling for"; encoding every such thing as a
  field is the trap this design explicitly avoids.
- A follow-up email mentions a 1-year-old traveling with them. A 30-hour
  itinerary that was perfectly fine as a number now needs to be
  reconsidered — not because `MaxHours` should have been smaller from
  the start (it was a fine constraint for the request as first
  understood), but because new context changed what "good enough" means
  mid-search.

**Go code's job shrinks to match, deliberately.** It is not the job of
`routesearch` to grow a field for every soft preference someone might
email in — that path never ends and produces an ever-larger, still
incomplete parameter list. Go code stays exactly what it is today: a
small, fixed set of mechanical, typed-parameter primitives —
`Search` (one-way + hub search), `SearchRoundTrip`, `SearchFlexible` —
plus the not-yet-built booking-horizon defer. Each is a **tool** in the
LLM-agent sense: a fixed name, a typed argument list, a structured
result (`Plan` / `RoundTripPlan` / `FlexiblePlan`), nothing fuzzy in or
out. The agent's job is picking *which* tool, with *what* arguments,
given the current understanding of the request — and, after seeing the
result, deciding whether to call a tool again with different arguments
or to stop.

**The loop itself**:
1. **Form/update the spec.** Turn the email thread so far (initial
   request + every follow-up) into a structured spec: the concrete,
   machine-checkable part *and* a running list of soft constraints in
   plain language (e.g. "must be there for Christmas," "traveling with
   an infant, avoid very long single itinerary") — the second list
   doesn't become new Go fields; it stays something only the agent reads.
   See "Spec's concrete fields" below for exactly which of the two lists
   each constraint type belongs in.
2. **Decide the next action**: call one tool with a chosen set of
   concrete arguments; defer (booking horizon, or "wait, the user might
   still be adding context"); **ask the user a clarifying question** (the
   spec is genuinely underspecified — e.g. no return date and no
   indication it's one-way — not just "could be narrower"); **ask the
   user to confirm the cost** of a search whose estimate crosses some
   threshold before dispatching it (see "Confirming search cost" below —
   this is a deferred idea, not built); or finalize.
3. **Dispatch and await.** A tool call is one row inserted into
   `agent_tasks`, plus a tiny Kafka message ("this task is ready to run")
   — the agent doesn't run `routesearch` itself, it hands the request off
   and gets woken up again by another message once a result exists,
   exactly as the rest of this document describes ("Collector task
   dispatch" below).
4. **Examine the result against the full spec** — concrete constraints
   already got enforced by the Go side (a result violating `MaxHours`
   simply isn't in `Plan.FinalResult`); the agent's own job here is
   specifically the soft-constraint half: does the cheapest option
   returned actually satisfy "must be there for Christmas," etc.? If
   not, that's not "no results" — the deterministic search worked fine;
   this is the layer above it disqualifying an option a fixed pipeline
   would have shipped as "the answer."
5. **Loop or finalize.** Not good enough → go to 2 with adjusted
   arguments (e.g. exclude a date range, tighten `MaxHours`, rerun with
   a return date now that one's known). Good enough, or the loop's own
   budget below is exhausted → finalize: draft the results email (same
   LLM-drafting idea already in "Components") and, for a not-fully-solved
   case, say so honestly rather than presenting a partial answer as
   final.

**Confirming search cost (deferred idea, not built).** `routesearch`'s
default is now unlimited/exhaustive (see "Query budget: unlimited by
default, an optional cap" under "Cheap multi-leg route search" below) —
right for a person watching `cmd/routesearch` run in a terminal, who
sees the estimate and decides live, but the email path has no live
terminal: a request that looks small can still turn into a long,
possibly surprising run with nobody watching. The fix mirrors
`cmd/routesearch`'s own pre-search prompt (`cmd/routesearch/confirm.go`)
rather than reinventing it: before the first dispatch of a request whose
estimate crosses some threshold, park it in `awaiting_user` (the same
state `ask_user`/`ActionAskClarification` already uses) with a
confirmation email instead of dispatching straight away.

**The confirmation message itself is a fixed Go template filled with
numbers, never LLM-drafted text** — the same principle "Go stays narrow"
already establishes for `routesearch` throughout this document, applied
to this action too. `DecideNextAction`'s only job here is emitting the
lightweight signal ("this request needs a cost confirmation, here are
the dispatch arguments it would use"); the actual candidate-count/
query-count/time-estimate numbers come from the same deterministic
estimator `cmd/routesearch/confirm.go` already has (`ResolveCandidates`
+ `hubEstimate`'s arithmetic — no scraping, no LLM call), rendered into
one fixed template. Two reasons this isn't the LLM's job: the numbers
have to be trustworthy and reproducible (an LLM paraphrasing "up to 113
scrapes" risks quietly getting the arithmetic wrong), and it's the exact
same estimate either surface would show, so computing it twice in two
different ways (one deterministic, one LLM-drafted) would be pure
duplication for a worse result. A user's reply ("yes, go ahead" / a
changed constraint) reaches `Decide` the same way any other follow-up
does ("Continuous email / mid-flight interruption" above) — no new
mechanism, just a new reason a request can be sitting in
`awaiting_user`.

**Open**: the threshold itself (flat query-count cutoff, or scaled by
how open-ended the request's `MaxLegs`/date-window is), and whether
`Spec` needs its own field to skip confirmation for a user who's already
said "just find me the best price, however long it takes."

**Spec's concrete fields, precisely — and the gap between this and what's
built.** The rule for which list a constraint goes in isn't "how
important is it," it's "can Go check it mechanically": anything a
`routesearch.Params`-shaped struct can express as a typed field belongs
in the concrete half, regardless of how it entered the spec (a first
email might state a hard price ceiling as plainly as the origin
airport). Everything else — a judgment call needing context, not a
threshold — stays in `SoftConstraints`.

- **Concrete today, and already built**: `Origin`, `Destination`,
  `DepartDate`, `ReturnDate`, `MaxHours`.
- **Concrete, plumbing exists lower down but nothing above it sets it
  yet**: `MaxPrice` and a checked/carry-on bag count — both are already
  full protobuf fields on `googleflights.Query` (`MaxPrice`,
  `CarryOnBags`, `CheckedBags`; see "Baggage cost is a query input, not a
  scoring adjustment" below), just never threaded up through
  `SearchParams` → `routesearch.Params` → `agents.Spec`.
- **Concrete, built in `routesearch.Params` but missing from
  `agents.Spec`**: `MinLayoverMinutes`/`MaxLayoverMinutes` — a real gap
  independent of the LLM being stubbed: today's agent loop can't set
  these even in principle, because `Spec` doesn't carry them.
  `QueryBudget` is the mirror case the other way: on `Spec` already, so
  the agent *can* set it, but nowhere does an email's "how thorough a
  search do you want" map to it today.
- **Concrete, designed but not built anywhere**: `MaxLegs`,
  `MaxCountries`, `ExcludedCountries` (see "Deeper itineraries" above) —
  a date *window* as a first-class request shape rather than one exact
  date is `FlexibleParams`, already built, just not yet a field
  `SearchFlexible`'s caller can set from an email-derived spec either.
- **Soft, and staying that way — DESIGN.md already names why**: holiday/
  occasion timing judgment calls, "avoid a very long single itinerary"-
  style vague comfort preferences, anything where the *email's wording*
  carries meaning a numeric field can't — this is what `SoftConstraints`
  is for, not a to-do list of fields to eventually add.

None of the "concrete, not built" rows above are hard to add — they're
mechanical plumbing once the LLM call itself exists (still deferred, see
"Open decisions") — flagged here so "the agent will handle price/layover/
country limits" isn't assumed true of the current build.

**Baggage cost is a query input, not a scoring adjustment.** A cheap
fare with 2 checked bags priced separately can lose to a pricier one
that includes them — but the fix isn't a bag-fee cost model bolted onto
`routesearch`'s scoring. Google Flights' own query already accepts a bag
count (`Query.CarryOnBags`/`CheckedBags`, wired into the protobuf today)
and returns `Offer.Price` **already inclusive** of bag fees for carriers
that price them separately. So once a bag count reaches `SearchParams`,
every downstream consumer — `pickCheapestFeasible`, the Pareto set,
`bestConnection` — needs *zero* changes: they already just compare
`Offer.Price`, and that price becomes bag-inclusive for free. The only
work is the same mechanical plumbing named above, not new comparison
logic. (Known limitation, not fixable here: Google's own bag-fee
display isn't perfectly complete for every low-cost carrier — inherited,
not something this codebase can correct.)

**The loop degenerates gracefully for the simple case.** A precise,
unambiguous request (most `search-api` misses, and plenty of plain
emails) needs none of this back-and-forth — step 1 produces a spec with
no soft constraints, step 2 picks the obvious tool and arguments, step 4
has nothing extra to check, and it finalizes after one round. The agent
loop is a superset of "just run the search," not a mandatory detour.

**Continuous email / mid-flight interruption.** This is the part a
one-shot "parse email, run search, send reply" design structurally can't
do. Each request's agent loop is durable state — one `agent_requests`
row, keyed by `request_id` so email intake can find it again — not a
running process. A reply to an existing thread doesn't start a new
request, it's an `UPDATE agent_requests SET spec_json = ...` against the
row already tracking that thread (`cmd/email-intake -signal` today; a
real SES reply handler later) — no message needed for this one, since
whatever step runs next for that request reads the spec fresh from the
row anyway.

**No workflow engine backs this, on purpose — see `internal/agents`' and
`internal/kafka`'s package docs for the reasoning.** Durability comes
from the row itself, not a replay log: every step (a task dispatched, a
result examined, a decision made) commits to
`agent_requests`/`agent_tasks` before the function that made it returns.
A crash mid-step loses nothing, because nothing was ever held only in
memory. "Async" here means two things at once: dispatching a search
never blocks (it's an `INSERT` plus a published message, both of which
return immediately), and no process sits there checking a clock to see
if it's time to do something — a step only runs because a Kafka message
said so, pushed by whatever just happened (a search finished, an email
arrived). An earlier version of this design used a poll loop (a
`cmd/email-intake -worker` checking the database every few seconds)
instead of Kafka messages — replaced because polling only notices a
change up to one interval late and has to keep checking even when
nothing's happening; a pushed message reacts immediately and costs
nothing between messages.

**Reacting to a follow-up mid-dispatch** doesn't need anything like a
language-level "wait on two things at once": a follow-up updates
`spec_json` regardless of what the request's `status` currently is, so a
request sitting in `dispatched` (a task already in flight) picks up the
new constraint the moment `RecordTaskResult` hands it back to
`awaiting_decision` and `Decide` reads the row fresh. There's also no
need to cancel an in-flight task when a follow-up lands — same reasoning
as before: let it keep running and decide, once its result is in,
whether the new context changes anything.

**Termination is still bounded, same principle as the query budget, one
level up.** An agent that can always decide "let's try one more idea"
needs its own ceiling or it never stops: a cap on redispatch rounds and
an overall query-budget-across-the-whole-loop (not per tool call) are
both required, and hitting either forces finalize-with-what-you-have —
same anytime-algorithm honesty the query budget already established for
a single search, just scoped to the whole conversation instead of one
call.

**Audit trail gets a layer above `Plan`, not a replacement for it.**
Every agent decision — the spec at that point, which tool was called
with what arguments, and *why* (the stated reasoning for accepting or
rejecting a result) — is its own logged entry, parallel to
`candidates_ranked`. If anything this matters more than the mechanical
audit trail: the deterministic part is checkable by rerunning it: the
judgment calls are not, so the record of *why* the agent made one is the
only way to catch it inferring something the email never actually said.

**Where this lives in the repo — resolved, built**: `internal/agents`
holds the loop's logic — `DecideNextAction` and `FormSpec` (the real
`LLMClient` calls; `llm.go`/`ollama.go`/`openai.go` are the adapter and
its two backends), `DraftFinalEmail`, `Decide` (reads an `agent_requests`
row and either dispatches a new task, parks it in `awaiting_user`, or
finalizes), and `RecordTaskResult` (folds a finished task's outcome back
into its request and hands it back to
"awaiting_decision") — following the same `cmd/`-is-thin/
`internal/`-has-the-logic split used everywhere else in this repo. It has
no Kafka dependency at all; `internal/kafka` is the separate, thin
messaging layer both consumers below import.

`cmd/agent-worker` is a new, persistent background process — the only
one whose whole job is running the decision loop: it consumes
`internal/kafka`'s `agent-decisions` topic and calls `Decide` for each
message. `cmd/collector -worker` is the other persistent background
process, consuming `search-tasks` and calling `RecordTaskResult` once a
fetch finishes, then publishing the next `agent-decisions` message
itself. `cmd/email-intake` is deliberately *not* persistent — one-shot
only (`-start`, `-signal`), the same way one incoming email triggers one
action rather than needing its own always-running process; splitting it
this way (an earlier version bundled the persistent loop into
`cmd/email-intake -worker`) keeps a name meaning one thing.

**Open decisions**:
- **Tool contract**: not asked — the four tools named above
  (`Search`/`SearchRoundTrip`/`SearchFlexible`/defer), each taking
  exactly the typed params already designed for it, nothing added. Say
  so if a fifth tool turns out to be needed.
- **Redispatch cap**: not asked — defaulting to **3 rounds** before
  forced finalization. Say so if that's too tight or too loose once this
  is actually built.
- **LLM choice / call shape**: resolved and built — `agents.LLMClient`
  (`internal/agents/llm.go`) with two backends, `OllamaClient` (dev,
  `qwen2.5:7b`, live-verified) and `OpenAIClient` (prod shape written,
  not exercised live — no key in this dev environment), picked by
  `LLM_BACKEND` env var. Still open: whether prod ever runs against
  Ollama or it stays dev-only, and whether `qwen2.5:7b` is good enough
  for prod judgment calls — the live runs surfaced real reasoning
  mistakes (e.g. once mis-read two offers as violating `MaxPrice` when
  neither did, burning a redispatch round on nothing), which argues for
  a stronger model in front of anything real users see.
- **`Spec`'s field gap**: `MaxPrice`, `MinLayoverMinutes`/
  `MaxLayoverMinutes` built (`agents.FormSpec` fills them from free text,
  `DecideNextAction` can set them on a retry). Still open: bag count, a
  date window (so `SearchFlexible` becomes a second dispatchable tool),
  `MaxLegs`/`MaxCountries`/`ExcludedCountries` — see "Spec's concrete
  fields" above for which layer each is missing from.

## Collection scope

- The collector never crawls broadly or on a fixed schedule across all
  routes — it fetches **one route because one user asked for it**.
  This is what keeps scraping load low enough to avoid needing the
  proxy-pool/anti-bot-evasion infrastructure that large-scale crawling
  would require (see prior discussion): request volume is bounded by
  inbound email volume, not by route/date combinatorics.
- Repeat value comes from accumulation, not breadth: every fulfilled
  request adds one data point for that route into Delta Lake, so a
  route asked about repeatedly (by the same or different users) builds
  a real price-history trend over time; `search-api` then serves
  instantly from whatever's already been collected, and only a
  genuinely new route/date triggers a fresh scrape.
- Resolved: `search-api` is **not** read-only. A search that misses
  (no data for that route/date yet) also creates an on-demand collection
  request — a row in `agent_requests` with an empty soft-constraint list,
  the same shape `cmd/email-intake -start` creates, plus the same
  `DecisionTrigger` publish onto `internal/kafka`'s `agent-decisions`
  topic that gets the loop moving — so email and search-api are two
  producers into the same table and the same topic, no separate queue
  service needed for either one. `search-api` must therefore return a
  "pending, check back" response on a miss rather than just an empty
  result.

## Components

- **Email intake / `cmd/email-intake`** (SES-inbound handler not yet in
  the repo; the one-shot dev CLI is) — in prod, SES inbound receives the
  request email (initial or a follow-up on an existing thread) and either
  creates a new `agent_requests` row + publishes its first
  `DecisionTrigger`, or updates an existing row's spec. Superseded by
  "Agent loop: LLM drives the search" below, which is the fuller version
  of this: the LLM isn't just parsing the request and drafting the reply
  at the two edges, it's the thing deciding what to search and whether
  the result is actually good enough, for the whole request, not only at
  the boundary. Deliberately *not* where the persistent loop runs — see
  `cmd/agent-worker` below and "Where this lives in the repo" under that
  section.

- **`cmd/agent-worker` (Go)** — the persistent process running the agent
  loop's decision step: consumes `internal/kafka`'s `agent-decisions`
  topic and calls `internal/agents.Decide` for each request that's ready.
  New; see "Agent loop" above.

- **`cmd/search-api` (Go)** — normally read-only against the serving
  store (Postgres in prod, SQLite locally — see "Local development"),
  but on a miss (route/date not yet collected) it also creates an
  `agent_requests` row and publishes its `DecisionTrigger` directly — the
  second of two producers into the loop.

- **`cmd/collector` (Go)** — on-demand only, and thin: its `-worker` mode
  consumes `internal/kafka`'s `search-tasks` topic, runs the fetch, and
  publishes the resulting `DecisionTrigger` back onto `agent-decisions`
  (see "Collector task dispatch" below). No scheduled/broad scraping
  mode.

- **Spark** (`etl/spark/clean_raw_flights.py`) — periodic batch job
  (not continuous streaming — there's no continuous fare feed to
  stream from anymore) that cleans/dedupes whatever raw drops have
  accumulated since the last run and merges them into Delta Lake.
  Delta Lake (not plain Parquet) because each run needs to upsert into
  shared tables without clobbering prior requested-route history.

- **dbt (`etl/dbt`)** — models the Delta silver layer into a gold,
  analytics-ready layer (fare trends per requested route) via Spark
  SQL.

- **Airflow (`etl/airflow`)** — orchestrates the periodic chain: Spark
  clean → dbt build → sync to serving store. (Email intake → collector
  is event-driven, not Airflow-scheduled.)

- **Serving sync** — a job (Spark write or small Go/CDC consumer) that
  publishes the gold Delta tables into the serving store (Postgres in
  prod, SQLite locally — see "Local development"), so `search-api`
  reads (the hit path, above) never query Delta Lake directly and stay
  low-latency — not from the same raw table the collector writes, and
  not from Delta Lake directly.

- **Infra** — Terraform provisions an EC2 fleet on AWS; Ansible
  configures each instance and installs/joins a self-managed
  Kubernetes cluster on top; every component (email intake, agent-worker,
  collector, Spark, serving sync, search-api) ships as a Docker image and
  deploys onto that cluster via Helm charts. Postgres (the serving store
  and the agent loop's durable state, both rows in it — see "Agent loop")
  and Kafka (Strimzi — the messaging between `cmd/agent-worker` and
  `cmd/collector`, see "Collector task dispatch") both run as self-managed
  workloads on that same footprint. S3 and SES inbound are the two
  deliberate managed-AWS exceptions (mail receiving and object-storage
  durability aren't worth self-hosting) — everything else is
  self-managed. `terraform/` is an empty placeholder — that provisioning
  doesn't exist yet. `ansible/` does have one real role today, `mac_dev`
  — but it manages a developer's own Mac (Flyway, Go, sqlite3 for local
  dev), not the EC2 fleet above; see "Local development". Both live as
  separate top-level directories, not nested under a shared `infra/` (the
  older AWS CDK stack that used to live there was removed by request —
  see the top of this doc).

## Schema ownership

**Resolved, applies to every database this project ever has (SQLite
today, Postgres in prod later): Go code never creates or alters
schema.** It inserts, updates, and selects — nothing that touches DDL.
Schema is DBA/ops tooling's job, kept structurally separate from
application code, the same way Terraform provisions the database
*server* but never reaches into table-level DDL — a different layer,
different tooling, different (and often more cautious) change process.

**Why this is the standard, not just a preference here**: an app
process that can alter its own schema has no boundary between "my code
changed" and "my data's shape changed" — two risk profiles that
production practice keeps apart on purpose (schema changes are less
reversible, more often reviewed separately, sometimes run under a
different, more-privileged DB credential than the app's own runtime
user gets). Conflating them into one Go binary's startup path is
exactly the anti-pattern this avoids.

**Resolved: Flyway, versioned migrations from day one, folder pattern
`databases/<db>/`.** Not a diff-based/declarative tool (Liquibase-style
auto-generated diffs, or Atlas) — explicit, hand-written, versioned SQL
files applied in strict order is the whole point: less surface for a
tool's own dialect-translation logic to get a migration subtly wrong
across versions. `flyway_schema_history` (a table Flyway itself creates
in the target database) tracks exactly which migrations have run.

```
databases/
  sqlite/
    flyway.toml          # [environments.default].url + [flyway].locations
    migrations/
      V001__create_flight_prices.sql
      V002__create_route_search_plans.sql
  postgres/               # doesn't exist yet — same pattern, when prod needs it
    flyway.toml
    migrations/
```

Naming is Flyway's own required format, not a style choice:
`V<version>__<description>.sql` — the double underscore is the
separator Flyway parses on; a single underscore fails to parse.

**Today (SQLite, local dev)**: `make db-init` runs
`flyway -configFiles=databases/sqlite/flyway.toml migrate`. Verified
live: creates `flyway_schema_history`, applies both migrations in
order, `flyway info` shows both as `Success`, and `cmd/collector`/
`cmd/routesearch` read/write the result normally afterward.
`internal/catalog.Open` checks both tables exist and fails fast with a
pointer back to `make db-init` if not — a read (`SELECT ... FROM
sqlite_master`), not schema management, so it doesn't cross the line
above; it just refuses to silently proceed against a database that was
never set up.

**Target (Postgres in prod, on the Helm/Kubernetes stack already
decided)**: the same principle, realized as a **Helm pre-install/
pre-upgrade hook Job** — a one-shot Kubernetes Job, gated to complete
before the application Deployment rolls out, running the official
`flyway/flyway` image against `databases/postgres/migrations/`. Schema
application stays an infra artifact (a Job spec + migration files),
never application runtime code, in prod exactly as in local dev.

**Provisioning Flyway itself** is a dev-machine/CI setup step, not
something this repo's own tooling installs — consistent with the rest
of "Schema ownership": the migration tool is infra/ops-provisioned, not
something Go (or any app-side script) pulls in for itself. Locally,
that's `ansible/roles/mac_dev` (`ansible-playbook
ansible/playbooks/mac_dev.yml`, or `brew install flyway go sqlite3` by
hand — see `ansible/manual_mac_dev.sh`); in CI/prod, the `flyway/flyway`
Docker image.

**Open decisions**:
- **Least-privilege DB credentials**: not asked — a hardened setup
  gives the app's own runtime Postgres user no DDL rights at all,
  separate from Flyway's own credential. Doesn't apply to SQLite (no
  user/permission model) — a Postgres-in-prod item, flagged for when
  that exists.

## Collector task dispatch: Kafka-chained steps, no workflow engine

How a task actually moves from "an email/search-api miss arrived" to
"fetched, stored, and the requester notified" — including the case
where fulfilling one request means fetching several fares (a
multi-city itinerary, or a worker deciding mid-flight that a request
needs several sub-fetches).

**Resolved: Kafka, but no workflow engine, and no message that carries
full state.** Temporal was considered and dropped — see `internal/agents`'
package doc for the full reasoning (its core value, high throughput and
complex sagas, isn't what this project's email-bounded, days-SLA volume
needs; its cost, a second Postgres-backed cluster, isn't earned back).
Kafka was *also* dropped for one round of this design (when it would have
sat in front of Temporal's own task queue — pure duplication) and then
brought back on its own merits once Temporal was gone: something has to
turn "a search finished" into "go decide the next step" the instant it
happens, and a poll loop checking the database on a timer (the version
that briefly replaced both) only notices up to one interval late and has
to keep checking even when nothing's happening. Kafka's messages are the
push-based fix for exactly that, without needing a workflow engine's
replay machinery to get it: each message is a small, disposable trigger
("go check request X" / "go run task Y"), never the request/task's actual
state — the database (`agent_requests`/`agent_tasks`, unchanged from the
poll-loop version) stays the one place that state lives, so a message
being lost, redelivered, or arriving twice is harmless by construction.

**Two topics, chained** (`internal/kafka`):

```
agent-decisions {request_id}  --cmd/agent-worker-->  search-tasks {task_id}  --cmd/collector-->  agent-decisions {request_id}  --> ...
```

- `agent-decisions` carries a `DecisionTrigger{request_id}`. Consumed by
  `cmd/agent-worker`, which calls `internal/agents.Decide`: loads the row,
  asks `DecideNextAction`, and either inserts a new `agent_tasks` row +
  publishes its id onto `search-tasks`, or finalizes and publishes
  nothing further — that silence is the entire "stop" signal, no separate
  mechanism needed.
- `search-tasks` carries a `SearchTaskTrigger{task_id}`. Consumed by
  `cmd/collector -worker`, which loads the task, runs the real fetch
  (`internal/routesearch.Search`), saves the result, calls
  `internal/agents.RecordTaskResult` (folds the outcome into the
  request's round history, hands it back to `awaiting_decision`), and
  publishes a fresh `DecisionTrigger` for that request — waking the next
  round.

Both topics are keyed by the id in the message (`request_id` /
`task_id`), so every step in one request's chain lands on the same
partition and processes in order.

**Commit-after-work, not commit-on-read**: both consumers use
`internal/kafka.Consumer.Next`, which fetches a message but does *not*
mark it handled until the caller explicitly commits — done only after
the triggered work (a `Decide` call, a fetch) actually finishes. A worker
that crashes mid-step leaves its message uncommitted, so it's redelivered
to the consumer group after a restart/rebalance rather than silently
dropped — every step this project chains through Kafka is a
database-backed, redo-safe check, never a one-shot side effect, so
redelivery is always safe to retry.

`agent_tasks` table (`internal/catalog`, schema at
`databases/sqlite/migrations/V004__create_agent_tasks.sql`) — unchanged
in shape from the poll-loop version, just no longer polled:

```sql
CREATE TABLE agent_tasks (
  task_id      TEXT PRIMARY KEY,
  request_id   TEXT NOT NULL REFERENCES agent_requests(request_id),
  round        INTEGER NOT NULL,
  params_json  TEXT NOT NULL,      -- origin, destination, dates, max_hours, budget
  status       TEXT NOT NULL,      -- pending | done | failed
  result_json  TEXT,
  error        TEXT,
  created_at   TEXT NOT NULL, updated_at TEXT NOT NULL
);
```

**Multi-leg ("complex") requests need no fan-out at the task layer at
all**: `internal/routesearch.Search` already runs the whole hub-search
algorithm (below) as one in-process Go function with its own paced,
sequential scrapes — it was never a distributed fan-out to begin with, so
one `agent_tasks` row per request (not per leg) is already the right
granularity. Durable checkpointing mid-search isn't needed either:
routesearch's own audit trail (`route_search_plans`, "Audit trail" below)
is written incrementally, and a crash mid-search just means the task is
retried from scratch on the next round, an acceptable cost at this
project's query-budget scale (a handful to dozens of scrapes per request,
not hundreds).

**Notification on completion**: still open (unchanged from before) —
`search-api` querying `agent_requests.status`/`finalized_by` by
`request_id` vs. some other "ready" signal; not blocking.

## Cheap multi-leg route search: how "complex" requests decide what to fetch

Elaborates the "complex" case named in "Collector task dispatch" above:
when a plain A→B search isn't enough, and specifically *which*
candidate hubs the single in-process search (below) spends its query
budget scraping — "try every possible intermediate airport" is
combinatorially impossible and would itself become the mass-crawl
"Collection scope" rules out.

**Reframing the problem**: this is not a shortest-path search over a graph
already in hand — it's a resource-constrained shortest path (RCSP) problem
over a graph that must be *discovered by scraping, one edge at a time,
under a hard query budget*. Every edge (a leg's price) costs one real
scrape (latency, scrape-load, block risk), so "explore" and "query" are
the same action: the algorithm has to decide which edges are worth paying
for, not just how to search a graph it already has.

- **Nodes**: (airport, time), not just airport — a connection is only
  feasible if arrival + minimum connection time ≤ the next leg's
  departure, and infeasible past a max-layover cutoff. A time-expanded
  graph, not a plain airport graph.
- **Edges**: one scraped leg (origin, destination, date) → price +
  duration + concrete departure/arrival times (`googleflights.Offer.
  Segments`, already returned today). **Nonstop only** — see "Our hops
  vs. Google's hops" below; a leg query is never allowed to be itself a
  connection.
- **Weight to minimize**: price.
- **Hard constraints** (feasibility, not objectives, unlike price): total
  elapsed time (departure to final arrival, layovers included) ≤ the
  user's tolerable-hours cap; each connection's layover within [min, max]
  minutes; optionally max stops.
- **Baseline**: a plain A→B Google Flights search already finds Google's
  own best *single-ticket* connections — the only value this feature adds
  is trying **split-ticket / hidden-city combos** (separate one-way legs
  through a hub) that price differently than a through-fare. Step 0 is
  always the plain A→B search (1 scrape): both the answer-of-last-resort
  and the price/duration bound everything else prunes against.

**Our hops vs. Google's hops — two different meanings of "connection" that
must never nest.** `routesearch` decides its own topology (which hub(s)
to force a self-transfer through), chosen from OpenFlights schedule data
specifically because a **nonstop** `A→hub`/`hub→B` route exists
(`Graph.CandidateHubs`). Separately, Google Flights decides its own
connections for whatever (origin, destination, date) it's asked to price
— it is always free to answer a plain query with a multi-stop itinerary
through a third airport `routesearch` never chose or saw. Left alone,
these two notions of "hop" nest silently: a leg query for `A→hub` can
come back as an itinerary that itself connects through some airport `X`,
so a candidate the algorithm thinks is "2 legs via hub" is actually a
4+ segment real itinerary, and every downstream thing built on "one hop =
one flight" breaks along with it — the geometry-prune bound (assumed
roughly-direct flight time per hop), the self-transfer layover-risk count
(one flagged layover per hub *we* chose; Google's hidden connection adds
an unflagged one), and `MaxLegs`/`MaxCountries` (Deeper itineraries",
above) as real caps on transfers.

**Resolved: every leg query `routesearch` issues itself is nonstop-only**
(`googleflights.SearchParams.MaxStops` set via `NonstopOnly()`, value `1`
— this protobuf's stops field is 1-based like `Seat`/`Trip`, so `1` means
nonstop, not `0`) — `A→hub`, `hub→B`, and any future N-hop leg. This
makes "our hop" and "Google's hop" the same thing by construction; they
can no longer nest. If a candidate hub has no live
nonstop fare that day (OpenFlights schedule existence isn't a same-day
fare guarantee), the leg comes back infeasible and the candidate is
pruned — correct, not a bug. The **Step 0 baseline** (plain A→B) stays
deliberately unrestricted: its whole job is to capture Google's own best
full itinerary, stops and all, as the price/duration floor the hub search
has to beat.

**Bounding the search** (the actual "smart" part — otherwise this is an
unbounded fan-out over every airport on Earth):

1. **Geometry prunes before any scrape.** Estimate each candidate hub's
   minimum possible detour from great-circle distance (haversine) and a
   fixed cruise-speed assumption; discard any hub whose `A→hub` +
   `hub→B` minimum flight time already exceeds the tolerable-hours cap
   with zero layover. Pure arithmetic, no scraping — cuts "every airport
   on Earth" to a short list before spending a single scrape.
2. **Prior ranking, not exhaustive order.** Rank surviving candidates —
   known major hubs near the great-circle path first, then airports
   already seen for this region in the local store — and explore
   best-first. A good split-ticket price found early tightens the
   pruning bound for everything explored after it.
3. **Cache before scrape.** Check the local store for a recent-enough row
   for the exact (origin, destination, date) before fetching — the
   "repeat value comes from accumulation" property already claimed in
   "Collection scope," made literal.
4. **A\*-style admissible pruning.** Only fetch `hub→B` once `A→hub`'s
   cheapest fare is in hand: if that price plus a cheap lower-bound
   estimate for `hub→B` (last-seen price for that pair, or a
   price-per-mile prior) is already ≥ the current best full price, skip
   the second scrape entirely — half the candidate's cost avoided
   without ever fetching it.
5. **Query budget: unlimited by default, an optional cap.** `QUERY_BUDGET
   <= 0` (the default) means run every surviving candidate to the (*)
   frontier-cutoff below — exhaustive, provably-optimal-within-the-
   candidate-set search is the actual selling point over a plain flight
   search (root `README.md` "Why this beats a plain flight search"), and
   the project's SLA is days, not seconds, so there's no reason to settle
   for less. A positive `QUERY_BUDGET` reintroduces the earlier anytime
   behavior — stop early, return the best feasible combo found so far —
   for a caller that explicitly wants a bounded, faster/cheaper run
   instead (e.g. the email agent loop's default cap; see "Agent loop"
   above). Either way, "Collection scope" is unaffected: per-request cost
   still can't exceed the candidate list a real route graph produces, and
   collection is still triggered only by an actual request, never a
   broad crawl.
6. **Depth cap.** 1-stop split-ticket combos by default — cost grows
   multiplicatively per extra hop, and so does self-transfer risk (below).
   Raisable via `MaxLegs`; see "Deeper itineraries" below.

### Exploration algorithm

The six techniques above are the intuition; this is the algorithm itself,
stated precisely enough to implement without re-deriving it.

**Class**: best-first branch-and-bound with lazy, budgeted edge
evaluation — A* where "expanding a node" costs a scrape instead of being
free, so the frontier order also decides *which edges get paid for at
all*, not just the order results come back in.

**State**: for the 1-stop case, a state is just a candidate hub `h`
(the path is fixed: `A → h → B`). Generalizing to 2+ stops, a state
becomes a label `(node, arrival_time, price_so_far, duration_so_far)` —
see "Generalizing beyond 1-stop" below.

**Bound functions** (per candidate hub `h`):
- `g(h) = ` actual price of `A→h`, once scraped — before that, undefined.
- `hEst(h) = ` an *admissible* (never-overestimating) lower bound on the
  cheapest possible `h→B`: a cached recent price for that exact leg if
  the local store has one, else `great_circle_miles(h, B) × min_price_per_mile`,
  where `min_price_per_mile` is a deliberately low global constant (or the
  cheapest $/mile actually observed so far, whichever is lower) — it must
  underestimate, or every pruning step below becomes unsound.
- `f(h) = g(h) + hEst(h)` once `g(h)` is known; before `A→h` is scraped,
  use `LB(h) = est(A→h) + hEst(h)` (both sides estimated) to rank
  candidates that haven't been queried yet at all.

**Main loop**:

```
best          ← baseline direct A→B search              // 1 query, seeds the bound
frontier      ← candidate hubs passing the geometry prune  // §1 above
frontier      ← sort ascending by LB(h)                    // §2 above
queries_used  ← 1

while frontier not empty and queries_used < QUERY_BUDGET:
    h ← frontier.pop_min()

    if LB(h) ≥ best.price:
        break                          // (*) — see optimality note below

    leg1 ← scrape(A, h, date)          // query #1 for this candidate
    queries_used ← queries_used + 1
    if leg1 has no offer feasible within the elapsed-time budget so far:
        continue                       // dead end, cost one query, not two

    g1 ← cheapest feasible price in leg1
    if g1 + hEst(h) ≥ best.price:
        continue                      // leg 1 alone already forecloses winning; leg 2 skipped

    if queries_used ≥ QUERY_BUDGET:
        break

    leg2 ← scrape(h, B, date + layover window)   // query #2, only spent when leg 1 leaves room to win
    queries_used ← queries_used + 1

    for (o1, o2) in feasible pairs from (leg1, leg2):   // layover ∈ [min, max], total time ≤ cap
        candidate ← {price: o1.price + o2.price, duration: total(o1, o2), path: [o1, o2]}
        if candidate is not dominated by any result already kept:
            best ← best ∪ {candidate}, minus anything candidate now dominates   // Pareto update, §"Output"

return best
```

**Why the `(*)` break is a full stop, not just a `continue`**: `frontier`
is sorted ascending by an *admissible* bound, so the moment the best
remaining `LB(h)` is no better than the current best price, every hub
still in the frontier is provably no better either — this is the same
argument that makes A* optimal. It is optimal **within the candidate set
the geometry prune let through**, not a proof about every conceivable
routing on Earth — a cheap fare through a hub the geometry prune excluded
is a false negative this algorithm accepts by construction (see "Hub
candidate source" below).

**Two independent stopping conditions, doing different jobs — one always
on, one optional**: the `(*)` bound-crossing break is what makes results
*provably good* (given the candidate set), and by default it's the only
thing that stops the loop, which is what makes the result the actual
best option rather than a good-enough one found fast. `QUERY_BUDGET`,
when a caller sets it above 0, is what makes the algorithm *provably
terminate quickly* instead — a deliberate trade of that optimality
guarantee for a bounded run. Worth knowing if you do set it: early on,
with no cached prices yet, `hEst` is loose and `(*)` may rarely trigger,
so a low budget is a real, frequently-hit backstop, not just a formality.

**Anytime property, when a budget is set**: because the frontier is
processed best-first, `best` after any prefix of the loop is a
reasonable answer — running out of a positive `QUERY_BUDGET` mid-loop
degrades result quality gracefully (it just means fewer,
less-likely-to-win candidates went unexplored) rather than failing
outright. With the default unlimited budget this property is moot: the
loop simply runs to the `(*)` cutoff or the frontier's end, whichever
comes first.

**Generalizing beyond 1-stop**: 2-stop search reuses the identical loop,
but a hub can now be reached via more than one first leg with different
(price, duration) trade-offs, so "state" must become the full label
`(node, arrival_time, price_so_far, duration_so_far)`, and a label is
discarded the moment another label at the same node **dominates** it
(≤ price, ≤ duration, compatible-or-earlier arrival) — standard
multi-criteria label-setting (the same idea Dijkstra's relaxation step
uses, extended from one scalar cost to a Pareto pair). 1-stop search
above is the degenerate case of this where every path has exactly one
intermediate node, so dominance checking collapses to plain Pareto
membership on the final `(price, duration)` pair — which is exactly what
the loop above already does.

**Output**: a small ranked (price, duration) Pareto set, not one "best"
answer — cheapest and fastest usually disagree. Flag any combo assembled
from separate tickets through a hub as a **separate-ticket / self-transfer
itinerary**: no through checked bags, no airline rebooking if the first
leg is delayed — real risk the plain single-ticket baseline doesn't carry,
never presented as equivalent to a through-fare without that flag.

**Fits the existing design, no new infra**: this is exactly the "complex"
case named in "Collector task dispatch" — and, per that section, it needs
no fan-out at the task layer: `internal/routesearch.Search` runs the
whole loop below (candidate generation, geometry pruning, the leg
scrapes, the A\*-style bound) as one in-process function under one
`agent_tasks` row, the same per-provider concurrency throttle already
decided there applying uniformly to every task regardless of how many
legs it scrapes internally. What's new here is entirely `routesearch`
*logic*, not new components.

### Deeper itineraries: N-hop search and hop-country constraints

Extends "Generalizing beyond 1-stop" above from "2-stop behind a flag" to
an arbitrary depth, for a traveler type the design so far didn't have:
price beats everything else including comfort — many legs, a long total
elapsed time, several transfers, all acceptable if the itinerary is
cheaper. `MaxLegs` and `MaxHours` become **tunable fields on the existing
`Params`**, not a separate request type — same `Search` function, same
audit trail shape, just a caller (person or agent) free to set e.g.
`MaxLegs: 5, MaxHours: 48` instead of leaving the (unchanged) defaults.

**`MaxLegs` raises the depth cap, it doesn't remove it.** The
label-setting algorithm from "Generalizing beyond 1-stop" runs unchanged
past 2 hops — a state is still `(node, arrival_time, price_so_far,
duration_so_far, legs_so_far)`, discarded the moment another label at the
same node dominates it. `legs_so_far` is now also a hard cutoff: no label
expands past `MaxLegs`. Cost is genuinely multiplicative per extra hop —
candidate hubs at hop 2 branch into candidates at hop 3 branch into hop
4 — so unlimited `QUERY_BUDGET` (the default, same semantics as 1-stop)
means a high `MaxLegs` really does explore exhaustively, which is the
point: real-world runs have seen 100+ scrapes from a single
well-connected origin before even reaching hop 2 (IMPLEMENTATION_PLAN.md
Phase 3) — expected under this design, not a bug, and exactly why
`cmd/routesearch` confirms the estimated cost before running (see
`cmd/README.md`). Set `QUERY_BUDGET` explicitly for a bounded run
instead.

**Country/region hop constraints — a new edge filter, cheap to apply.**
OpenFlights' airport table already carries `Country` per airport
(`internal/openflights`), so this is a filter on the existing
geometry-prune step (before any scrape), not a new data source:

- **`MaxCountries`** (or `MaxRegions`, once a country→region mapping
  exists — see open decision below): caps how many distinct countries a
  candidate path may transit, independent of `MaxLegs` — a 4-leg
  itinerary that never leaves one country's domestic network is a
  different risk profile from one hopping 4 countries.
- **`ExcludedCountries`** (a hard prune, not a soft preference): any
  candidate hub in an excluded country is dropped at the geometry-prune
  stage, before it ever becomes a scrape. This is where a visa
  ineligibility, a conflict-zone/sanctions concern, or an
  identity-based travel restriction lives — a real constraint the search
  must never route through, not a preference a Pareto ranking merely
  deprioritizes.
- **The exclusion list's source is the agent loop, not `routesearch`.**
  Per "Agent loop: LLM drives the search, Go stays narrow" above: whether
  a user's stated passport/visa situation or a mentioned conflict zone
  should become an `ExcludedCountries` entry is exactly the judgment call
  that section already reserves for the LLM — not a Go struct field's
  job to infer. `routesearch` only ever receives the already-decided
  list; it never derives one itself from, say, a passport mentioned in
  an email.

**Other per-hop conditions**: raised as a category, not yet enumerated —
e.g. a minimum layover that scales with `MaxLegs` (a 6-hour minimum
connection makes sense at 1 hop, less so budgeted 4 times over a 48-hour
trip), overnight-layover handling, or a max single-leg duration
independent of the total cap. Flagged here as real and likely, deferred
until a concrete one is actually needed rather than guessed at now.

**Open decisions**:
- **Max depth**: was "1-stop only, 2-stop behind an opt-in flag" — now
  **`MaxLegs`, a tunable `Params` field, default unchanged** (still
  1-stop). Say so if you want a different default, or a hard ceiling
  above which even an explicit request is rejected.
- **Country/region exclusion**: not asked — no `ExcludedCountries` /
  `MaxCountries` field exists yet. Say so if you want it added to
  `Params` now, and whether country-only is enough to start or region
  (needing a country→region mapping, which doesn't exist yet) is needed
  too.
- **Resolved: no scaling needed.** `QUERY_BUDGET`'s default is now
  unlimited (see "Bounding the search" above), so "a 5-leg search
  plausibly needs a higher budget" no longer applies — there's no cap to
  raise. Still open if a caller does set an explicit `QUERY_BUDGET`: say
  so if you want it to scale with `MaxLegs` automatically rather than
  stay a single manually-set number.

### Round trips and flexible dates

Two gaps in scope, both real: everything above only ever searches one
exact one-way date pair. Neither is a small tweak — both change what
"a candidate" even means — so both get their own phase rather than being
folded into the existing loop.

**Round trips.** A round-trip fare is not always "outbound price + return
price" — airlines/GDSs often bundle a round trip at a price different
from (usually, but not always, cheaper than) the sum of two one-ways, the
same way a hub connection can price differently than its two legs summed.
So a round-trip request gets **three baselines** compared up front, all
cheap (no hub search yet):

1. **Bundled round-trip** — one query, both dates, Google's own
   round-trip fare.
2. **Sum of two one-ways** — one query per direction, priced
   independently — sometimes cheaper, for the same reason a hub split can
   beat a through-fare: the two directions aren't always priced by the
   same inventory/carrier.
3. Whichever of (1)/(2) wins becomes `best`, seeding the hub search
   exactly like the one-way case's single baseline did.

**Hub search runs per-direction, not combined.** Outbound and return each
get their own independent 1-stop hub search (the existing algorithm,
unmodified) rather than searching outbound-hub × return-hub jointly — the
combined version is a real algorithmic generalization (a hub choice for
one direction doesn't constrain the other, so it's just two independent
instances of the existing search, not a harder search) but multiplies
candidate count for a savings case that's already the thinner one (a
per-direction hub beating a per-direction through-fare is rarer than a
bundled round trip beating two one-ways). Deferred, not designed away —
revisit if per-direction hub search alone doesn't earn its query budget.

**Flexible dates.** The request's date(s) are a target, not an exact
requirement — and per the days-not-minutes SLA already decided, there's
time to spend confirming that rather than assuming it. This is a
**two-phase search, not one bigger loop**, because the two things being
explored (which dates, which hubs) have very different costs:

- **Phase A — date sweep.** Query just the cheap baseline (direct, or
  the 3-baseline round-trip comparison above) across a bounded date grid
  around the requested date(s) — e.g. ±3 days each way. No hub search
  yet: this phase is only trying to answer "which date(s) in this window
  are actually cheap," and every query in it is exactly as cheap as the
  single baseline query the one-way case already spends. A 7×7 grid
  (±3 days outbound × ±3 days return) is 49 queries, worth it given a
  days-scale SLA and the total absence of per-candidate hub-query cost in
  this phase.
- **Phase B — hub search anchored on the winner.** Take the best (or top
  few) date combination(s) Phase A found and run the existing per-date
  hub search (this section, above) *only* on those — not on every date in
  the grid, which is what keeps this from multiplying hub-candidate count
  by grid size. Hub search is expensive (multiple scrapes per candidate);
  date search is one query per date; spending the budget on more dates
  cheaply before spending it on more hubs expensively is the same
  admissible-bound-before-you-pay principle as "Exploration algorithm"
  above, just applied one level up.

**Audit trail gets a sibling, not a replacement**: Phase A's date grid
becomes a `date_sweep` array in `Plan` — same shape idea as
`candidates_ranked` (which date, what it cost, which won) — sitting
alongside it, since "why this date" and "why this hub" are both
questions the audit trail exists to answer.

**Open decisions** (same convention as elsewhere in this doc):
- **Date window**: not asked — defaulting to **±3 days** each end that
  was given. Say so if you want it wider/narrower, or asymmetric.
- **Sweep budget**: unaffected by "Query budget: unlimited by default"
  above — `QUERY_BUDGET <= 0` still means unlimited for Phase A's date
  grid too, so by default the whole window gets priced. Still not asked:
  whether Phase A and Phase B should draw from separate counters once a
  caller *does* set a positive `QUERY_BUDGET` (date-sweep queries are
  cheap, one per date combo; hub queries are expensive, multiple per
  candidate) — today they're both just `Base.QueryBudget`, checked
  independently per phase rather than a single shared running count. Say
  so if you want that changed.
- **Top-K dates into Phase B**: not asked — defaulting to **1** (just the
  outright winner) rather than running hub search on several near-tied
  dates. Say so if you want hub search hedged across, e.g., the top 3.

**Eligibility constraints on the Phase A winner** (`FlexibleParams`):
`AvailableFrom`/`AvailableUntil` (a real-world travel window, e.g.
limited PTO — narrower than the scan window, which only controls what
gets *priced*), `ExcludeWeekdays`, `BlackoutDates` (holidays, etc). Every
scanned date is still priced and shown; these only narrow which priced
date `cheapestDateScanEntry` may pick — pure price-argmin otherwise.
LLM-assisted re-ranking of near-tied candidates (day-of-week/holiday
judgment calls a hard constraint can't express) is a deferred idea, not
built.

**Deferred idea, not designed yet: land near the destination, cover the
last leg by ground.** E.g. flying into Tianjin (TSN) and taking the
intercity train into Beijing, rather than flying all the way into PEK.
This is a genuinely different edge type from everything above — not
another OpenFlights flight route, but a ground-transport hop with its
own (currently nonexistent) distance/time/cost data source — so it needs
its own candidate-generation approach (nearby-airport-by-radius, not
route-existence) and its own cost model before it fits this algorithm.
Flagged here rather than folded into "hub candidates," which it isn't.

**Already covered, no new design needed**: a domestic first hop (e.g.
Vancouver → Calgary/Toronto before the long-haul leg) is not a new case —
it's exactly what the existing hub search already searches for. Any
airport with an OpenFlights route both from the origin and to the
destination is already a hub candidate today, domestic or not.

### Booking horizon: dates too far out to price yet

Observed directly, not theoretical: a flexible-date sweep for a date
~15 months out came back **empty on every single date in the window** —
not one route having no service, but nothing anywhere having fares yet.
Airlines/GDSs publish schedules and fares roughly 10–12 months ahead,
not indefinitely; past that horizon, "no offers" doesn't mean "no such
flight," it means "ask again later." Today's code can't tell those two
apart — it just returns empty either way, which is a wrong answer
dressed as a right one for the too-early case, and (for a flexible
sweep) burns a full window's worth of queries to learn nothing.

**Detection is a date check, not a response-content guess.** Trying to
infer "too early" from Google's response (which says nothing explicit
either way) is unreliable and, worse, only knowable *after* spending the
query. Checking `request_date − today > BOOKING_HORIZON_DAYS` first is
cheap, reliable enough, and — critically — answerable before scraping
anything: exactly the same "cheap arithmetic before an expensive query"
shape as the geometry prune. `BOOKING_HORIZON_DAYS` is a deliberately
approximate constant (carriers vary — full-service international
carriers tend toward the long end, LCCs often load less far out), not a
fact about any specific route.

**What happens instead of searching**: rather than run (and get nothing
from) a request that fails this check, the request **defers itself** —
no durable timer primitive needed for this, just a row:
1. Compute a wake time — `request_date − BOOKING_HORIZON_DAYS` plus a
   small safety buffer (fares aren't always loaded exactly on schedule).
2. Set `agent_requests.status = 'deferred'`, `deferred_until` = that wake
   time, and stop — same "state lives in the row" property as everything
   else in "Agent loop"/"Collector task dispatch": nothing needs to stay
   running for the months in between. This is the one genuinely
   time-based check in the whole design — "has enough calendar time
   passed" isn't something a Kafka message can trigger, since nothing
   *happens* to publish one. A periodic sweep (`deferred_until <= now` →
   back to `awaiting_decision` + publish a fresh `DecisionTrigger`) is
   what "waking" actually is; a daily Airflow-scheduled task (`etl/airflow`
   already runs on a schedule for unrelated reasons) is a natural fit
   rather than a bespoke long-lived process, since checking a date needs
   nothing faster than daily.
3. On waking, run the search normally. If it's still empty (a carrier
   loaded a little later than the horizon constant assumed), retry on a
   short backoff (e.g. daily) up to a capped number of attempts or until
   the requested date itself has passed — same "give an honest answer
   eventually, don't loop forever" principle as the query budget.
4. Whichever response channel is waiting (email, or search-api's
   "pending" state) gets notified once real results land — reusing the
   existing "notify both response channels" completion step, not a new
   one.

**The user finds out immediately, not after a long silence**: the
*first* response — email reply or search-api's pending state — says
outright that the date is beyond the fare-publishing horizon and roughly
when to expect a real answer, rather than either an empty result or no
response at all until the wake sweep fires. This is the email-drafting
LLM call from "Components" being told about this specific case, not a
new mechanism.

**Audit trail**: a deferred request's `agent_requests.status` is
literally `'deferred'` with `deferred_until` set — so the audit trail
correctly shows "hasn't actually searched yet, waiting," not a
completed-but-empty search, no separate status string needed the way a
`Plan`'s own `status` field would.

**Not yet implemented**: `DecideNextAction` (`internal/agents/decide.go`)
is a real LLM call now (see "Agent loop"), but its system prompt tells
the model never to choose `ActionDefer` — the wake sweep this section
describes isn't written yet, so there's nothing for a `defer` decision to
do. This section describes the target behavior, which the
row-plus-Kafka design already supports without needing anything new
(unlike the Temporal-dependent version this replaced), but the code path
itself is still open work.

**Open decisions**:
- **`BOOKING_HORIZON_DAYS`**: not asked — defaulting to **330 days**
  (~11 months), a common denominator across full-service carriers. Say
  so if you want it per-carrier, configurable, or a different default.
- **Safety buffer past the horizon**: not asked — defaulting to
  **+14 days** past the raw horizon before the first wake-up attempt,
  since fare loading isn't always exactly on schedule.
- **Retry cap once within horizon**: not asked — defaulting to
  **daily retries for up to 14 days**, then notify "still not found,
  may need to check back yourself" rather than retrying indefinitely.
- **Partial-window flexible requests**: not asked — a date-sweep window
  straddling the horizon (some dates reachable now, some not) defaults
  to **deferring the whole request** rather than splitting it into an
  immediate sweep over the reachable dates plus a separate deferred
  sweep over the rest. Simpler, at the cost of not getting the
  already-reachable dates' prices right away. Say so if you'd rather it
  split.

### Pacing, audit trail, and observability

**Resolved: this is a days-SLA async workflow, not a minutes-SLA one.**
People plan a trip like this months out; the response channel is email,
sent once when the search finishes, not a live wait. That removes the
latency pressure that would otherwise push toward parallel/burst
dispatch (a real alternative considered and rejected — see below) and
argues for the opposite: deliberately **space scrapes out** — a plain
in-process delay between each query (minutes, not seconds; already
`routesearch.Params.Delay` / a `time.Sleep`, since the whole search runs
inside one `agent_tasks` task's goroutine — see "Collector task
dispatch") — which is strictly safer against anti-bot detection than
firing legs back to back, and costs nothing extra since there's no
deadline to race against. No durable timer needed here, unlike the
booking horizon's month-scale wait: a single goroutine sleeping for
however long a bounded query budget's worth of spacing adds up to (worst
case, hours) is not the "outlive the process" problem a months-long wait is.
`QUERY_BUDGET` (see "Exploration algorithm" above) keeps its job of
capping total scrape *volume* per request, but its reason for existing
here shifts from "bound latency" to "bound how much of this one user's
search gets spread across how much of the provider's attention" —
spacing solves politeness; the budget solves cost/scope. This is also
why the email/agent path sets an explicit, positive `QUERY_BUDGET`
(default 20, below) rather than leaving it unset: `routesearch`'s own
package default is now unlimited/exhaustive (the CLI's selling point —
root `README.md`), which is the right default for a person watching one
request run in a terminal, wrong for an inbound-email pipeline whose
whole reason for existing is bounding scrape volume per stranger's
request ("Collection scope").

(A parallel-wave version of the same algorithm — precompute several
candidates, dispatch their leg-queries concurrently — was considered and
set aside: it trades query-count discipline for latency, and latency
isn't scarce here. Worth revisiting only if the SLA ever tightens.)

**Resolved: route-existence graph = OpenFlights, not a hardcoded hub
list.** `airports.dat` (coordinates, for the geometry prune) and
`routes.dat` (which airport pairs are actually flown) are static,
bundled data — a one-time snapshot shipped with the repo, not scraped —
and turn "every airport on Earth" into "airport pairs someone actually
flies," a real graph instead of an arbitrary curated list. Distance from
this graph stays a *feasibility filter* only (per "Exploration
algorithm"'s bound functions) — never the price-ranking signal — for the
reason already discussed: fare price doesn't track distance, and ranking
by distance would bury exactly the anomalous-but-cheap routes this
feature exists to find.

**Audit trail**: every request's full candidate plan and outcome is
recorded, not just the final answer — needed for "why didn't it find X"
debugging, and cheap to keep given the low request volume ("Collection
scope"). One `route_search_plans` row per request, written once as the
plan and updated as results land:

```json
{
  "request_id": "uuid",
  "input": {"origin": "SFO", "destination": "NRT", "date": "2026-12-20",
            "max_hours": 30, "max_stops": 1, "budget_usd": 800},
  "candidates_considered": 4312,
  "candidates_after_geometry_prune": 5187,
  "candidates_ranked": [
    {"hub": "ANC", "lb_usd": 410, "rank": 1,
     "leg1": {"queried": true, "price_usd": 180, "queried_at": "..."},
     "leg2": {"queried": true, "price_usd": 250},
     "outcome": "kept", "combined_usd": 430},
    {"hub": "SEA", "lb_usd": 460, "rank": 2,
     "leg1": {"queried": true, "price_usd": 205},
     "leg2": {"queried": false, "reason": "g1 + hEst >= best.price"},
     "outcome": "pruned"},
    {"hub": "PDX", "lb_usd": 610, "rank": 8,
     "leg1": {"queried": false}, "leg2": {"queried": false},
     "outcome": "frontier_cutoff", "reason": "LB >= best.price"}
  ],
  "final_result": ["...Pareto set..."],
  "queries_used": 7,
  "status": "done"
}
```

Same JSON is also emitted as structured log lines (one per row-level
event above — candidate generated, leg queried, candidate pruned, result
kept), each tagged with the `agent_tasks.task_id` (and, one level up,
the `agent_requests.request_id` — see "Agent loop"), so a
`route_search_plans` row and its log lines cross-reference each other.

**Step-level visibility**: no Temporal Web UI to lean on here — this is
the one real observability cost of dropping it (see "Collector task
dispatch"). What replaces it: `route_search_plans.plan_json`'s
per-candidate table above, updated incrementally as `routesearch.Search`
runs, plus the structured JSON log lines tagged the same way, are the
whole story — `kubectl logs`/journald + `jq` over the tagged lines, or a
query against `plan_json`, in place of a workflow-history browser.
Sufficient at this project's request volume ("Collection scope"); revisit
if debugging a specific request's step-by-step history from logs alone
ever proves too slow.

**Open decisions** (defaults chosen the same way as elsewhere in this
doc — say so if you'd rather change them):
- **Query budget**: not asked — defaulting to **20 scrapes/request**,
  now read as a cost/scope cap rather than a latency cap (see above), and
  now an explicit override of `routesearch`'s own unlimited package
  default rather than "leave it at the default" (see above). Say so if
  you want it higher/lower, unlimited here too, or configurable per
  request.
- **Max depth**: see "Deeper itineraries: N-hop search and hop-country
  constraints" above — `MaxLegs`, a tunable `Params` field, default
  still 1-stop.
- **Query spacing**: not asked — defaulting to a **random 5–30 minute
  in-process delay between scrapes**. Say so if you want it tighter/looser.
- **Self-transfer risk disclosure**: not asked — defaulting to **always
  labeling** any multi-ticket combo as such in the response, never
  silently mixing it into the same list as single-ticket results.
- **Log aggregation**: not asked — no new component yet; JSON-to-stdout
  (readable via `kubectl logs` / journald + `jq`, or a laptop's terminal
  in local dev) until request volume actually justifies a Loki/ELK-style
  aggregator. Say so if you want one now instead of deferred.

### Wide fuzzy-range search, preference-aware pruning, a trace file, and a shared rate limiter

Extends "Round trips and flexible dates," "Deeper itineraries" (hop-
country constraints), and this section's own audit-trail/pacing above —
raised in discussion once a real request looked like "cheapest trip,
sometime in the next N months/years, roughly M days long (give or take),
avoid country X as a layover, not around a given holiday." Generalizes
IMPLEMENTATION_PLAN.md's former "Fixed-length trip, wide-open window"
backlog item rather than building it exactly as first scoped — the
original framing was one example (a specific window/length), not the
actual requirement (*any* width, on *any* of these dimensions).

**Resolved: every fuzzy dimension is enumerated exhaustively by default,
at any width — cost made visible, never silently narrowed.**
`FlexibleParams.TripLengthDays` (a single fixed length) becomes a
tolerance range (`TripLengthDays`..`TripLengthMaxDays`, sampled every
`TripLengthStepDays`, defaulting to 1 = every day — same convention every
other `StepDays` field in this codebase already uses); `FlexibleParams`
also gains an explicit `DepartFrom`/`DepartTo` window as an alternative
to today's center-date-plus-`WindowDays` shape. No dimension gets a
forced coarse default the way an earlier draft of this plan proposed
(erroring unless the caller picked a step) — that would trade away the
"true global optimum" guarantee the hub search already gives for hops,
for a heuristic approximation, which is the wrong tradeoff here. Instead:
before spending a real scrape, the true combination count and time
estimate is shown (`cmd/routesearch/confirm.go`'s existing pre-flight
pattern, extended to this shape; the agent/email path's own "disclose
default assumptions on the first `ask_user` round" mechanism, extended to
disclose this too), and `QueryBudget` (unlimited by default) is the one
explicit opt-in cap. Coarser sampling stays available, but only as an
explicit request ("check every few days"), never a silent default.

**Spiked, not adopted: a Google Flights bulk price-calendar/graph
endpoint.** It exists — `GetCalendarGraph`
(`https://www.google.com/_/FlightsFrontendUi/data/travel.frontend.flights.FlightsFrontendService/GetCalendarGraph`),
confirmed via a real working client
([krisukox/google-flights-api](https://github.com/krisukox/google-flights-api)'s
`GetPriceGraph`) — one request returns every (start date, return date,
price) triple for a start-date range at one fixed trip length, which
would turn a wide window's N `scanPair` scrapes into one call. Not worth
adopting here, for two reasons found in that client's source, not
theoretical:
  - **It's a stateful RPC, not a stateless query string.** Unlike our
    protobuf `tfs` param (one deterministic GET, no cookies), this
    endpoint needs a prior page load to harvest session cookies and a
    time-stamped anti-abuse token (`at=...`) sent with every call — real
    session-lifecycle code our `Client` (one `http.Client`, no state
    between calls) doesn't have anywhere today, for every call site
    (`cmd/routesearch`, `cmd/collector -worker`, `cmd/email-intake`,
    Kafka-dispatched agent tasks) to now share and keep alive.
  - **The request body pins a dated internal build id**
    (`bl=boq_travel-frontend-ui_20230627.07_p1` in the reference
    client, already ~3 years stale) inside a bespoke nested-array
    ("batchexecute"-style) payload — a second, differently-fragile
    reverse-engineered format alongside the protobuf one, needing its
    own independent upkeep as Google's frontend build rolls forward,
    for a fixed trip length per call rather than the full independent
    depart×return grid `SearchDateRange` supports (it would still need
    one call per trip length, or per return-range point, to cover that
    case).
  - Given "resolved: every fuzzy dimension is enumerated exhaustively by
    default" above already accepts N (or N×M) real scrapes as the
    honest cost, and this project's low request volume ("Collection
    scope") doesn't make that cost pressing, the added client-lifecycle
    complexity and a second fragile reverse-engineered surface aren't
    worth it for this project's stated risk tolerance. `scanPair`'s
    one-scrape-per-date loop stays the implementation; revisit if a
    live wide-range search's latency ever actually becomes the
    bottleneck (unlikely at today's "days-SLA async" volume).

**Resolved: hop-country and blackout-date pruning reach the agent loop,
not just `cmd/routesearch`.** `routesearch.Params.ExcludedCountries`/
`MaxCountries` and `FlexibleParams`/`DateRangeParams`' `BlackoutDates`
already exist and are already enforced by the search itself — but
`agents.Spec`/`CollectRouteRequest` never carried them, so a plain-English
"avoid Russia as a layover" or "not around Christmas" had no path into
either mechanism, in any search shape. `Spec` gains matching fields;
`FormSpec`'s prompt learns when to set them; `dispatch.runSearch` threads
them into every `routesearch.Params`/`DateRangeParams`/`FlexibleParams`
literal it builds, not just the new fuzzy-range shape. General
preferences ("I prefer an aisle seat," "I'd rather fly Star Alliance")
stay on `Spec.SoftConstraints`, already judged per-result by
`DecideNextAction` — no new mechanism needed there.

**Resolved: one combined trace file per request, not just a
`route_search_plans` row.** The per-candidate audit trail this section
already documents above (`plan_json`'s per-hub/hop/date table, `outcome`
+ `reason` on every entry) stays exactly as-is and exactly as valuable —
this adds a plain-file export of the *same* data, joined with the
conversation-level trail (`agents.Outcome`/`RoundRecord`: each round's
`Spec` snapshot, `Decision`+`Reasoning`, and `Result`) that today only
exists in memory during one loop run. One JSON file per finalized
request: every round, what was decided and why, and — nested under each
round — every route/date/hop combination that round's search actually
tried and whether it was kept or rejected and why. Mechanically this is
almost entirely wiring, not new tracking: the data was already fully
modeled, just never joined or written past the database.

**Resolved: a shared, process-wide rate limiter underneath the existing
per-request spacing, not a replacement for it.** "Query spacing," above,
already resolves *how far apart one request's own scrapes* are spaced
(`Params.Delay`/`sleepPacing`) — what it doesn't cover is `cmd/collector
-worker`'s worker pool (`concurrency` goroutines, each running a
*different* request's search independently): every goroutine paces
*itself* with no shared state, so real scrape throughput scales with
`concurrency`, not with `Delay` — the exact shape "Collector task
dispatch"'s live-run note about tripping Google's rate limiting from
near-simultaneous scrapes already describes. `internal/ratelimit.Limiter`
(`Wait(ctx) error`) is the fix: a `FixedWindow` implementation combining
multiple granularities (second/minute/hour/day, each independently
capped) behind one mutex-guarded counter set, shared by every
`googleflights.Client` in a process — wired at the client's one real HTTP
call site, so it throttles every concurrent goroutine together rather
than each independently. No token-bucket/leaky-bucket implementation for
now; the `Limiter` interface leaves room for one, and for a Redis-backed
implementation of the same interface once this goes distributed across
the (backlogged) Infra section's Kubernetes fleet, without touching any
call site again.

## Open decisions

**Resolved:** cloud = AWS EC2, self-managed compute; IaC = Terraform
(provision) + Ansible (configure) + Docker + Kubernetes + Helm
(deploy); object storage = S3; inbound email = SES; response channel =
both email reply and search-api; search-api triggers collection on a
miss, not just email; durability for the agent loop and fetch retries =
the serving store's `agent_requests`/`agent_tasks` tables (state) plus
Kafka/Strimzi (the trigger between steps — see "Collector task
dispatch"), not Temporal — considered and dropped as more infrastructure
than this project's scale justifies (see `internal/agents`' package
doc), while Kafka earns its place once it's the thing actually pushing
work instead of sitting redundantly in front of a workflow engine's own
task queue.

- **Kubernetes distro**: not asked — defaulting to **k3s** (lighter
  control-plane footprint than kubeadm/RKE2, well-suited to a
  self-managed EC2 cluster at this project's scale). Say so if you'd
  rather use kubeadm or RKE2.
- **Spark runtime**: not asked — defaulting to a **Spark-on-Kubernetes
  operator** on the same EC2/k3s cluster rather than a separate Spark
  cluster, since everything else already runs on that cluster and a
  second cluster to operate isn't justified by this project's volume
  (bounded by email/search requests, per "Collection scope"). Say so
  if you want Spark kept separate.
- **Serving sync mechanism**: not asked — defaulting to a **batch
  Spark write on the same Airflow-orchestrated schedule** as the rest
  of the ETL chain, rather than building CDC infrastructure. There's
  no real-time freshness requirement left to justify CDC: results only
  become available once collection + cleaning + dbt have all finished
  anyway, so the sync step isn't the bottleneck. Say so if you
  disagree.
- **EC2 architecture**: not asked — worth flagging: if prod EC2 is
  arm64 (Graviton) rather than x86_64, it's the same architecture as
  local M1 builds, not just "also native" — removes a whole class of
  arch-mismatch bugs, and Graviton is generally cheaper too. Nothing
  in the stack (Kafka, Spark, Delta Lake, dbt, Go, Postgres) blocks arm64.
  Defaulting to **arm64/Graviton** for this reason; say so if
  x86_64 is actually needed for something not yet in the design.

## Local development

Goal: the whole pipeline runs on a MacBook M1, fully testable end to
end, no AWS account required. Same Helm charts as prod throughout —
local dev is a smaller, substituted deployment of the identical
manifests, not a separate setup, so what works locally is evidence
about what works in prod.

- **Cluster**: k3d (k3s-in-Docker) — same distro as the prod default
  (k3s), arm64-native on M1, fast to create/destroy. `k3d cluster
  create` stands in for "Terraform + Ansible provisioning EC2"; the
  same Helm charts deploy into it via a `values-local.yaml` override.
- **Object storage**: MinIO (S3-compatible) replaces S3 — same API,
  so Delta Lake/Spark code doesn't change, just the endpoint/creds.
- **Serving store**: **SQLite, not Postgres** (resolved — keeping
  local dev simple was explicitly chosen over cluster/prod parity
  here). `search-api` and the serving-sync job need a small
  storage-driver abstraction (SQLite locally, Postgres in prod) behind
  one interface — the schema is simple enough that this shouldn't
  need much divergence. This is the one deliberate non-parity point;
  everything else below aims for real parity.
- **Spark**: same as prod default, no substitution — the
  Spark-on-Kubernetes operator runs inside the local cluster too
  (not `local[*]` mode), affordable here since data volumes are tiny
  (bounded by test-fixture requests, per "Collection scope").
- **Kafka**: same as prod, no substitution — Strimzi, single-broker
  KRaft mode (no ZooKeeper), deployed into the local cluster with lower
  resource requests/limits in `values-local.yaml`. Outside the cluster
  (a bare Go dev loop, not k3d), a plain local Kafka install works too —
  verified directly: `brew install kafka` (Kafka 4.x ships KRaft mode
  built in, `kafka-server-start`) plus `make kafka-topics` to create
  `agent-decisions`/`search-tasks` once.
- **Email intake**: SES inbound can't run locally — it's an
  AWS-managed delivery hop, not something to emulate. Local dev uses
  `cmd/email-intake -start`/`-signal` (built) as the fixture-driven
  stand-in — same effect a raw-email-fixture HTTP endpoint would have,
  writing the same `agent_requests` row (and publishing the same
  `DecisionTrigger`) real SES-triggered parsing code would. This tests
  everything downstream of "an email arrived"; it doesn't test AWS's
  delivery of the email itself, which isn't testable locally regardless.
- **Collector — provider**: resolved — a mock/stub provider for local
  dev and any automated tests, returning canned fares for known test
  routes, selected via an env var (e.g. `PROVIDER=mock`). Keeps
  repeated local/test runs from generating real scraping traffic
  against actual providers. The same collector image runs the real
  provider client in prod via the same env var.
- **Images**: build natively for arm64 (M1 needs no cross-compilation/
  emulation, and if prod EC2 is also arm64/Graviton per the item
  above, local and prod images are literally the same architecture)
  and load into k3d via `k3d image import` — no registry needed
  locally.
- **End-to-end test flow** (should be one scriptable command, not just
  manual steps, so it also runs in CI):
  1. Bring up the cluster + stack: Kafka, MinIO, SQLite-backed
     search-api/collector/agent-worker/email-intake, Spark operator
     (Helm). No workflow engine to bring up — `cmd/agent-worker` and
     `cmd/collector -worker` just need the same SQLite file the rest of
     local dev already uses, plus Kafka reachable.
  2. Run `cmd/email-intake -start` with a sample request (exercises the
     email path), or query `search-api` for a route that's a known miss
     (exercises the search-triggered path).
  3. Watch it flow: `agent_requests` row + `DecisionTrigger` →
     `cmd/agent-worker` dispatches a task → `cmd/collector -worker` runs
     it (mock provider) → MinIO raw zone → Spark batch → Delta Lake (on
     MinIO) → dbt gold → sync → SQLite; meanwhile the result also flows
     back through `agent-decisions` to finalize the request.
  4. Query `search-api` for that route/date and assert the result.
- **Sizing**: a single k3d node running Kafka (1 broker) + the Spark
  operator + MinIO + a handful of small Go services should fit an M1
  MacBook's unified memory at low resource requests — lighter than the
  Kafka+Temporal version considered earlier (no second Postgres-backed
  cluster for Temporal's own persistence) — flagging as an assumption to
  revisit if it turns out too heavy, not a blocker.

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
       │ start, or signal an      │ start (simple case:
       │ already-running agent    │ no LLM round-trip
       ▼                          │ needed — see "Agent loop")
      ┌──────────────────────────────┐
      │   Agent loop (row in the     │
      │   agent_requests table) —    │
      │   see "Agent loop: LLM       │
      │   drives the search" below.  │
      │   Decides: dispatch a tool   │
      │   call, defer, or finalize.  │
      └───────────────┬───────────────┘
                       │ dispatch (a tool call) = one
                       │ INSERT into agent_tasks
                       ▼
      ┌──────────────────────────────┐
      │   agent_tasks table          │
      │   (status: pending)          │
      └───────────────┬───────────────┘
                       │ polled + claimed
                       ▼
      ┌──────────────────────────────┐
      │         cmd/collector        │
      │  -worker: claims a pending   │
      │  task, runs the fetch, and   │
      │  writes the result back      │
      └───────────────┬───────────────┘
                       │ result picked up on the
                       │ agent loop's next poll tick —
                       │ may loop back to dispatch
                       │ again with different
                       │ parameters, or continue ▼
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

**Status: a first draft of the loop mechanics is built (`internal/agents`,
`cmd/collector -worker`, `cmd/email-intake`) — ahead of the sequencing
this section originally called for ("no LLM gets wired in until the
deterministic Go side is solid"), at explicit request, as a prototype of
the control flow. The decision step (`DecideNextAction`) is still a
deterministic stub, not a real LLM call** — see `internal/agents/decide.go`
— so what's actually proven out so far is the *loop*, not the judgment
calls it exists to make. Treat everything below as the built shape, with
the LLM call itself still an open decision (see "Open decisions").

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
   machine-checkable part (origin, destination, date window, max price,
   max hours, max stops...) *and* a running list of soft constraints in
   plain language (e.g. "must be there for Christmas," "traveling with
   an infant, avoid very long single itinerary") — the second list
   doesn't become new Go fields; it stays something only the agent reads.
2. **Decide the next action**: call one tool with a chosen set of
   concrete arguments; defer (booking horizon, or "wait, the user might
   still be adding context"); or finalize.
3. **Dispatch and await.** A tool call is one row inserted into
   `agent_tasks` — the agent doesn't run `routesearch` itself, it hands
   off the request and waits for the row's result, exactly as the rest of
   this document describes ("Collector task dispatch" below).
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
real SES reply handler later).

**No workflow engine backs this, on purpose — see `internal/agents`'
package doc for the reasoning.** Durability comes from the row itself,
not a replay log: every step (a task dispatched, a result examined, a
decision made) commits to `agent_requests`/`agent_tasks` before the
function that made it returns. A crash between polls loses nothing,
because nothing was ever held only in memory. "Async" here means
dispatching a search never blocks — it's an `INSERT` that returns
immediately; `cmd/collector -worker` claims and runs it on its own
schedule, in its own goroutine.

**Reacting to a follow-up mid-dispatch, not just between rounds**, is
handled by the same mechanism, just looser than a language-level
`select`: a follow-up updates `spec_json` regardless of what the request's
`status` currently is, so a request sitting in `dispatched` (a task
already in flight) picks up the new constraint the moment it's back to
`awaiting_decision` — no explicit "wait on two things at once" code
needed, at the cost of reacting on the next poll tick rather than
instantly. At this project's days-scale SLA (see "Pacing"), a poll
interval measured in seconds is not a real cost. There's also no need to
cancel an in-flight task when a follow-up lands — same as the original
reasoning: let it keep running and decide, once its result is in, whether
the new context changes anything.

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
holds the loop's logic — `DecideNextAction` (the stub LLM-call
stand-in), `DraftFinalEmail`, and `AdvanceRequest` (the state-machine step
function that reads an `agent_requests` row and does exactly one thing:
examine a finished task, dispatch a new one, or finalize) — following the
same `cmd/`-is-thin/`internal/`-has-the-logic split used everywhere else
in this repo. `cmd/email-intake` runs the poll loop that calls
`AdvanceRequest` for every non-finalized request (`-worker`), plus the
dev CLI standing in for SES inbound (`-start`, `-signal`); it is the SES
inbound handler in prod, same as before. `cmd/collector -worker` is the
separate poll loop that claims and runs `agent_tasks` rows — kept as its
own process from `cmd/email-intake`, since "decide what to search" and
"do one search" stay different concerns even without Temporal's
task-queue mechanism drawing that line for us.

**Open decisions**:
- **Tool contract**: not asked — the four tools named above
  (`Search`/`SearchRoundTrip`/`SearchFlexible`/defer), each taking
  exactly the typed params already designed for it, nothing added. Say
  so if a fifth tool turns out to be needed.
- **Redispatch cap**: not asked — defaulting to **3 rounds** before
  forced finalization. Say so if that's too tight or too loose once this
  is actually built.
- **LLM choice / call shape**: not asked — deferred entirely; not worth
  deciding until the deterministic side this depends on is done.

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
  the same shape `cmd/email-intake -start` creates, just written directly
  by `search-api` since both share the one serving store — so email and
  search-api are two producers into the same table, no separate queue
  service needed. `search-api` must therefore return a "pending, check
  back" response on a miss rather than just an empty result.

## Components

- **Email intake / `cmd/email-intake`** (SES-inbound handler not yet in
  the repo; the reconciler and dev CLI are) — in prod, SES inbound
  receives the request email (initial or a follow-up on an existing
  thread) and either creates or updates the request's `agent_requests`
  row. Superseded by "Agent loop: LLM drives the search" below, which is
  the fuller version of this: the LLM isn't just parsing the request and
  drafting the reply at the two edges, it's the thing deciding what to
  search and whether the result is actually good enough, for the whole
  request, not only at the boundary. Also runs the poll loop that
  advances every non-finalized request — see "Where this lives in the
  repo" under that section.

- **`cmd/search-api` (Go)** — normally read-only against the serving
  store (Postgres in prod, SQLite locally — see "Local development"),
  but on a miss (route/date not yet collected) it also creates an
  `agent_requests` row directly and returns a "pending" response rather
  than an empty one — the second of two producers into that table.

- **`cmd/collector` (Go)** — on-demand only, and thin: its `-worker` mode
  polls `agent_tasks` for pending rows, claims one, and runs the fetch
  (see "Collector task dispatch" below). No scheduled/broad scraping
  mode, no separate queue service — producer (the agent loop) and
  consumer (`cmd/collector`) meet in the same table, in the same store
  `search-api` already reads.

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
  Kubernetes cluster on top; every component (email intake, collector,
  Spark, serving sync, search-api) ships as a Docker image and deploys
  onto that cluster via Helm charts. Postgres runs as a self-managed
  workload on that same footprint — the only stateful service this
  project's request queue and workflow durability need, now that both
  are rows in it rather than separate Kafka/Temporal clusters (see
  "Agent loop" and "Collector task dispatch"). S3 and SES inbound
  are the two deliberate managed-AWS exceptions (mail receiving and
  object-storage durability aren't worth self-hosting) — everything
  else is self-managed. `terraform/` is an empty placeholder — that
  provisioning doesn't exist yet. `ansible/` does have one real role
  today, `mac_dev` — but it manages a developer's own Mac (Flyway, Go,
  sqlite3 for local dev), not the EC2 fleet above; see "Local
  development". Both live as separate top-level directories, not
  nested under a shared `infra/` (the older AWS CDK stack that used to
  live there was removed by request — see the top of this doc).

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

## Collector task dispatch: store-backed poll queue

How a task actually moves from "an email/search-api miss arrived" to
"fetched, stored, and the requester notified" — including the case
where fulfilling one request means fetching several fares (a
multi-city itinerary, or a worker deciding mid-flight that a request
needs several sub-fetches).

**Resolved: no message broker, no workflow engine.** Both were
considered (Kafka/Strimzi + Temporal) and dropped — see `internal/agents`'
package doc for the full reasoning. Short version: Temporal's task queue
already gives durable, multi-producer, deduplicated dispatch on its own,
so a Kafka layer in front of it was duplicate infrastructure; and once
that's gone, Temporal itself is solving a smaller problem than its own
operational cost (a second Postgres-backed cluster) justifies at this
project's scale (bounded by email volume, a days-not-minutes SLA, no
complex distributed sagas — see "Collection scope" and "Pacing"). What's
left does the same job with one already-present dependency: the serving
store.

**`agent_tasks` table** (`internal/catalog`, schema at
`databases/sqlite/migrations/V004__create_agent_tasks.sql`) is the queue:

```sql
CREATE TABLE agent_tasks (
  task_id      TEXT PRIMARY KEY,
  request_id   TEXT NOT NULL REFERENCES agent_requests(request_id),
  round        INTEGER NOT NULL,
  params_json  TEXT NOT NULL,      -- origin, destination, dates, max_hours, budget
  status       TEXT NOT NULL,      -- pending | claimed | done | failed
  claimed_by   TEXT, claimed_at TEXT,  -- lease, for crash-safe re-claim
  attempt      INTEGER NOT NULL DEFAULT 0,
  result_json  TEXT,
  error        TEXT,
  created_at   TEXT NOT NULL, updated_at TEXT NOT NULL
);
```

**Producers**: the agent loop (`internal/agents.AdvanceRequest`, run by
`cmd/email-intake -worker`) inserts a row each time it decides to
dispatch — see "Agent loop" above. `search-api` producing directly into
`agent_requests` on a miss (see "Collection scope") is the same
mechanism one layer up, not a second queue.

**`cmd/collector -worker`'s claim loop** (`cmd/collector/worker.go`) polls
for `status = 'pending'` rows, claims one via a conditional `UPDATE ...
WHERE task_id = ? AND status = 'pending'` (the affected-row count tells
it whether it won the race against another claimer — SQLite/Postgres both
support this without a broker's own dedup), and runs the fetch in its own
goroutine, capped by a `-concurrency` limit — the store-backed stand-in
for a Kafka partition's per-consumer throttle and Temporal's per-task-
queue concurrency cap alike (see "Collection scope"'s per-route
discussion for the trade-off this accepts: no per-route ordering
guarantee, just a global concurrency cap plus a cache check before
scraping).

**Retry / crash recovery**: a task that fails is marked `failed` with the
error recorded — `internal/agents`' decision step treats that the same as
"no offers" and retries with widened parameters (bounded by the
redispatch cap, same as any other round). A task claimed but never
finished (worker crashed mid-fetch) is caught by a stale-claim sweep
(`claimed_at` older than a lease timeout → back to `pending`) run at the
top of every poll tick — the hand-rolled equivalent of a Temporal
Activity's timeout+retry policy.

**Multi-leg ("complex") requests need no fan-out at the task layer at
all**: `internal/routesearch.Search` already runs the whole hub-search
algorithm (below) as one in-process Go function with its own paced,
sequential scrapes — it was never a distributed fan-out to begin with, so
one `agent_tasks` row per request (not per leg) is already the right
granularity. What Temporal would have added here — durable checkpointing
mid-search — isn't needed either: routesearch's own audit trail
(`route_search_plans`, "Audit trail" below) is written incrementally, and
a crash mid-search just means the task is retried from scratch on the
next round, an acceptable cost at this project's query-budget scale (a
handful to dozens of scrapes per request, not hundreds).

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
  Segments`, already returned today).
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
5. **Hard query budget.** A fixed cap on scrapes per user request
   (default: 20) regardless of candidate-list size. An anytime algorithm:
   if the budget runs out, return the best feasible combo found so far,
   never search exhaustively — the cap is what keeps this from becoming
   the mass-crawl "Collection scope" rules out.
6. **Depth cap.** 1-stop split-ticket combos by default; 2-stop only
   behind an explicit "aggressive search" flag — cost grows
   multiplicatively per extra hop, and so does self-transfer risk (below),
   for typically thin additional savings.

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

**Two independent stopping conditions, doing different jobs**: the `(*)`
bound-crossing break is what makes results *provably good* (given the
candidate set); `QUERY_BUDGET` is what makes the algorithm *provably
terminate quickly* regardless of how good the bounds turn out to be —
early on, with no cached prices yet, `hEst` is loose and `(*)` may rarely
trigger, so the budget is the real backstop, not a formality.

**Anytime property**: because the frontier is processed best-first,
`best` after any prefix of the loop is a reasonable answer — running out
of `QUERY_BUDGET` mid-loop degrades result quality gracefully (it just
means fewer, less-likely-to-win candidates went unexplored) rather than
failing outright.

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
- **Sweep budget**: not asked — defaulting to a **separate budget from
  the hub QUERY_BUDGET** (not shared) — date-sweep queries are cheap
  (one per date combo) and hub queries are expensive (multiple per
  candidate), so they shouldn't compete for the same cap. Say so if you'd
  rather they share one pool.
- **Top-K dates into Phase B**: not asked — defaulting to **1** (just the
  outright winner) rather than running hub search on several near-tied
  dates. Say so if you want hub search hedged across, e.g., the top 3.

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
   running for the months in between. A sweep at the top of
   `cmd/email-intake -worker`'s poll tick (`deferred_until <= now` → back
   to `awaiting_decision`) is all "waking" is; a poll interval measured in
   seconds costs nothing extra spent checking a timestamp that won't be
   true for months.
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
is still the deterministic stub from "Agent loop" and never returns
`ActionDefer`, and the wake sweep described above (`requeueDeferredRequests`)
isn't written yet either — this section describes the target behavior,
which the store-backed design already supports without needing anything
new (unlike the Temporal-dependent version this replaced), but the code
path itself is still open work.

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
shifts from "bound latency" to "bound how much of this one user's search
gets spread across how much of the provider's attention" — spacing
solves politeness; the budget solves cost/scope.

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
  now read as a cost/scope cap rather than a latency cap (see above).
  Say so if you want it higher/lower, or configurable per request.
- **Max depth**: not asked — defaulting to **1-stop only**, with 2-stop
  behind an opt-in flag.
- **Query spacing**: not asked — defaulting to a **random 5–30 minute
  in-process delay between scrapes**. Say so if you want it tighter/looser.
- **Self-transfer risk disclosure**: not asked — defaulting to **always
  labeling** any multi-ticket combo as such in the response, never
  silently mixing it into the same list as single-ticket results.
- **Log aggregation**: not asked — no new component yet; JSON-to-stdout
  (readable via `kubectl logs` / journald + `jq`, or a laptop's terminal
  in local dev) until request volume actually justifies a Loki/ELK-style
  aggregator. Say so if you want one now instead of deferred.

## Open decisions

**Resolved:** cloud = AWS EC2, self-managed compute; IaC = Terraform
(provision) + Ansible (configure) + Docker + Kubernetes + Helm
(deploy); object storage = S3; inbound email = SES; request queue =
the serving store's own `agent_requests`/`agent_tasks` tables, not a
separate Kafka/Strimzi cluster; response channel = both email reply and
search-api; search-api triggers collection on a miss, not just email;
durability for the agent loop and fetch retries = a store-backed poll
loop (see "Collector task dispatch"), not Temporal or a hand-rolled
Kafka-based tracking table — both were considered and dropped as more
infrastructure than this project's scale justifies.

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
  in the stack (Spark, Delta Lake, dbt, Go, Postgres) blocks arm64.
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
- **Email intake**: SES inbound can't run locally — it's an
  AWS-managed delivery hop, not something to emulate. Local dev uses
  `cmd/email-intake -start`/`-signal` (built) as the fixture-driven
  stand-in — same effect a raw-email-fixture HTTP endpoint would have,
  writing the same `agent_requests` row real SES-triggered parsing code
  would. This tests everything downstream of "an email arrived"; it
  doesn't test AWS's delivery of the email itself, which isn't testable
  locally regardless.
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
  1. Bring up the cluster + stack: MinIO, SQLite-backed
     search-api/collector/email-intake, Spark operator (Helm). No
     separate queue/workflow-engine component to bring up — `cmd/collector
     -worker` and `cmd/email-intake -worker` just need the same SQLite
     file the rest of local dev already uses.
  2. Run `cmd/email-intake -start` with a sample request (exercises the
     email path), or query `search-api` for a route that's a known miss
     (exercises the search-triggered path).
  3. Watch it flow: `agent_requests`/`agent_tasks` rows → `cmd/collector
     -worker` claims and runs the task (mock provider) → MinIO raw zone →
     Spark batch → Delta Lake (on MinIO) → dbt gold → sync → SQLite.
  4. Query `search-api` for that route/date and assert the result.
- **Sizing**: a single k3d node running the Spark operator + MinIO + a
  handful of small Go services should fit an M1 MacBook's unified memory
  at low resource requests, comfortably lighter than the Kafka+Temporal
  version this replaced — flagging as an assumption to revisit if it
  turns out too heavy, not a blocker.

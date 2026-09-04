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
      │   Agent loop (LLM control    │
      │   loop) — see "Agent loop:   │
      │   LLM drives the search"     │
      │   below. Decides: dispatch a │
      │   tool call, defer, or       │
      │   finalize.                  │
      └───────────────┬───────────────┘
                       │ dispatch (a tool call)
                       ▼
      ┌──────────────────────────────┐
      │   Kafka topic (Strimzi),     │
      │   key = route, msg = task    │
      └───────────────┬───────────────┘
                       │ consume
                       ▼
      ┌──────────────────────────────┐
      │         cmd/collector        │
      │  starts a Temporal workflow  │
      │  execution per task          │
      └───────────────┬───────────────┘
                       │ result examined by the
                       │ agent loop above — may
                       │ loop back to dispatch
                       │ again with different
                       │ parameters, or continue ▼
                       ▼
      ┌──────────────────────────────┐
      │            Temporal          │
      │ activities fetch fares with  │
      │ built-in retry; complex      │
      │ tasks fan out into child     │
      │ workflows, fan back in       │
      │ before completing            │
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

**Sequencing, stated up front so it isn't lost: this whole section is
design-only, not a build order.** Right now, and until the deterministic
Go side (`googleflights`, `openflights`, `routesearch`, `store`) is
solid, no LLM gets wired in — the priority is polishing the free, local,
deterministic primitives first. This section exists so that work isn't
accidentally shaped in a way this target architecture can't absorb
later — it changes *where the line is drawn*, not what to build next.

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
3. **Dispatch and await.** A tool call is a Kafka message /
   Temporal child workflow exactly as already designed — the agent
   doesn't run `routesearch` itself, it starts the same
   `CollectRouteWorkflow`-shaped task the rest of this document already
   describes, and waits for its structured result.
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
do. Each request's agent loop is itself one long-running Temporal
workflow (call it `TravelRequestAgentWorkflow`), keyed so email intake
can find it again — a reply to an existing thread doesn't start a new
request, it delivers a **Signal** into the workflow already running for
that thread.

**None of this needs the agent to sit there occupying anything while it
waits, distributed or not.** A Temporal workflow "awaiting" a child
workflow, an Activity (an LLM call included — has to be an Activity, not
inline workflow code, since an LLM call is non-deterministic and
workflow code must be deterministic except via Activities/child
workflows), or a signal is durably checkpointed and costs nothing while
it waits — a worker can hold thousands of such waits, for seconds or for
months, without a blocked thread or a busy-poll anywhere. This is true
of every wait in this design already (the dispatched search task, the
booking-horizon timer, waiting on a signal) — it doesn't need separate
handling to be "async."

**Reacting the instant a signal arrives, not just between rounds**, is
the one place the design above was too weak: "wait for the current tool
call to finish, then look at the new context" only checks signals
between loop iterations. The Go SDK's `workflow.Selector` is the actual
mechanism for better than that: it waits on *multiple* futures at once —
the in-flight tool call's completion **and** the signal channel — and
resumes on whichever fires first. So the default is a workflow that's
simultaneously awaiting its dispatched task and listening for a signal,
and reacts to new context immediately rather than only after the current
task happens to finish. A signal arriving mid-dispatch still doesn't need
to hard-cancel the in-flight call (Temporal supports that too, via the
child workflow's cancellation handle, if a case ever needs it) — the
agent can let it keep running and simply decide, right away, whether
it's still worth waiting for once it sees the new context, rather than
finding out only when it completes.

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
  (no data for that route/date yet) also enqueues an on-demand
  collection job, onto the same Kafka topic email intake uses — so
  email and search-api are two producers into one collector queue.
  `search-api` must therefore return a "pending, check back" response
  on a miss rather than just an empty result.

## Components

- **Email intake** (new, not yet in the repo) — SES inbound receives
  the request email (initial or a follow-up on an existing thread) and
  either starts or signals the request's agent workflow. Superseded by
  "Agent loop: LLM drives the search" below, which is the fuller
  version of this: the LLM isn't just parsing the request and drafting
  the reply at the two edges, it's the thing deciding what to search and
  whether the result is actually good enough, for the whole request, not
  only at the boundary. Still not wired in yet — see that section's
  "sequencing" note.

- **`cmd/search-api` (Go)** — normally read-only against the serving
  store (Postgres in prod, SQLite locally — see "Local development"),
  but on a miss (route/date not yet collected) it also publishes onto
  the same Kafka request topic and returns a "pending" response rather
  than an empty one — the second of two producers into that topic.

- **`cmd/collector` (Go)** — on-demand only, and thin: consumes the
  Kafka request topic (self-hosted via Strimzi) and starts one
  Temporal workflow execution per task. All the actual fetch/retry/
  fan-out logic lives in Temporal workflow and activity code (see
  "Collector task queue" below), not in the Kafka-consuming loop
  itself. No scheduled/broad scraping mode.

- **Temporal** (new, not yet in the repo) — self-managed workflow
  engine: runs the collector's workflow/activity code, gives fetch
  retries, per-route/provider rate limiting, and complex-search
  fan-out/fan-in correctness for free instead of hand-rolled Kafka
  message juggling. Needs its own Postgres-backed persistence store
  (separate from the serving store, per Temporal's own recommendation)
  and, optionally, its Web UI for observability. See "Collector task
  queue" below for how it fits with Kafka.

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
  onto that cluster via Helm charts. Kafka (Strimzi) and Postgres run
  as self-managed workloads on that same footprint. S3 and SES inbound
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

## Collector task queue: Kafka → Temporal

How a task actually moves from "an email/search-api miss arrived" to
"fetched, stored, and the requester notified" — including the case
where fulfilling one request means fetching several fares (a
multi-city itinerary, or a worker deciding mid-flight that a request
needs several sub-fetches).

**Message schema** (JSON on the Kafka request topic):

```
{
  "task_id":        "uuid",
  "origin":         "SFO",
  "destination":    "JFK",
  "depart_date":    "2026-12-05",
  "return_date":    null,
  "requester": {
    "channel":  "email" | "search-api",
    "email":    "...",        // present when channel == email
    "trace_id": "..."         // present when channel == search-api
  },
  "created_at": "..."
}
```

**Producers**: `email intake` and `cmd/search-api` (on a miss) both
publish this same message shape onto one Kafka topic — **keyed by
route** (`origin-destination`, e.g. `SFO-JFK`), not by `task_id`. That
puts every request for the same route on one partition, so per-route/
per-provider politeness (rate limiting) is a local property of one
partition's consumer rather than something requiring cross-worker
coordination. Trade-off, accepted: a single very busy route can
bottleneck one partition/worker; not a concern at this project's
request-bounded volume (see "Collection scope").

**Kafka client**: `confluent-kafka-go` (wraps librdkafka via cgo) —
most battle-tested Go Kafka client. Implementation note: this means
`cmd/collector`, `cmd/search-api`, and email intake all need cgo
enabled and librdkafka available at build *and* run time, so their
Dockerfiles need a glibc base with `librdkafka` installed (e.g.
Debian-slim + `apt-get install librdkafka-dev`) rather than a minimal
`scratch`/distroless final stage — a real (small) cost of this choice
over a pure-Go client, worth remembering when writing those
Dockerfiles.

**`cmd/collector`'s consumer loop** is intentionally thin: for each
Kafka message, it starts one Temporal workflow execution
(`CollectRouteWorkflow`), keyed by `task_id` as the Temporal workflow
ID (so a redelivered Kafka message that tries to start the same
workflow ID again is a no-op — Temporal's own dedup, not a hand-rolled
one), then commits the Kafka offset. Everything past that point is
Temporal's problem, not the consumer loop's.

**`CollectRouteWorkflow`** (runs in Temporal, code lives in
`cmd/collector` as the Temporal worker):
1. Decide simple vs. complex. A simple request (one O/D/date pair) is
   the common case; complex covers things like a multi-city itinerary
   or a workflow that, having started, determines it needs several
   date/leg variants to answer the original request.
2. **Simple**: run one `FetchFareActivity(origin, destination, date)`
   — Temporal retries it automatically per a configured retry policy
   (backoff, max attempts) if the provider call fails, no hand-rolled
   retry-topic/DLQ needed. On success, write the raw result to the S3
   raw zone and finish.
3. **Complex**: the workflow itself is the "worker that produces more
   tasks" — it starts N child workflow executions (one
   `CollectRouteWorkflow` per leg/variant, same code, recursively
   simple at that level), runs them concurrently, and `Get()`s every
   child's result before proceeding — Temporal's native fan-out/
   fan-in, not a hand-rolled `expected_children`/`completed_children`
   tracking table. If a child ultimately fails after its retries are
   exhausted, the parent decides whether that's a whole-request
   failure or a partial result, and reports accordingly — this
   decision point is itself part of what a workflow gives you for
   free (a durable place to make that call) that a bag of Kafka
   messages doesn't.
4. On completion (success or terminal failure), the workflow's last
   step notifies both response channels: sends the email reply and
   makes the result available to `cmd/search-api` (exact mechanism —
   e.g. `search-api` querying Temporal workflow status by `task_id`
   vs. the workflow writing a "ready" marker somewhere `search-api`
   already reads — still open, not blocking).

**Rate limiting / politeness toward providers**: Temporal worker
options support capping concurrent activity execution per task queue,
which doubles as the per-provider throttle this project cares about
(see the earlier scraping-safety discussion) — another thing that
doesn't need separate hand-rolled machinery now.

**Local dev fit**: Temporal ships a built-in dev server
(`temporal server start-dev`) backed by SQLite — a direct match for
the "SQLite locally, Postgres in prod" split already decided for the
serving store, so local dev doesn't need a second Postgres instance
just for Temporal's own persistence.

## Cheap multi-leg route search: how "complex" requests decide what to fetch

Elaborates the "Complex" branch of `CollectRouteWorkflow` above: when and
how it decides a plain A→B search isn't enough, and specifically *which*
child leg-searches to fan out into — "try every possible intermediate
airport" is combinatorially impossible and would itself become the
mass-crawl "Collection scope" rules out.

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

**Fits the existing design, no new infra**: this is exactly the "Complex"
branch already sketched in `CollectRouteWorkflow` — it fans out into child
`CollectRouteWorkflow` executions (one per leg), the same fan-out/fan-in
and per-provider concurrency throttle already decided there. What's new
is entirely workflow *logic* (candidate generation, geometry pruning, the
A\*-style bound, the query budget), not new components.

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
from) a request that fails this check, the workflow **defers itself**:
1. Compute a wake time — `request_date − BOOKING_HORIZON_DAYS` plus a
   small safety buffer (fares aren't always loaded exactly on schedule).
2. Sleep until then via a **Temporal durable timer** (`workflow.Sleep` /
   a timer future) — the same mechanism "Pacing" above uses for
   between-scrape delays, just at a scale of months instead of minutes.
   This is exactly what makes it tractable at all: a durable timer
   survives worker restarts and costs nothing while waiting, unlike a
   process that would need to stay alive (or a cron job re-deriving
   "is it time yet" on every tick) for the better part of a year.
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
response at all until the timer fires. This is the email-drafting LLM
call from "Components" being told about this specific case, not a new
mechanism.

**Audit trail**: a deferred request's `Plan`/`FlexiblePlan` gets a
status like `"deferred_until_2027-10-15"` instead of `"done"` — so the
audit trail correctly shows "hasn't actually searched yet, waiting," not
a completed-but-empty search. In Temporal's Web UI the same thing is
visible natively: a workflow sitting in a multi-month sleep is exactly
as inspectable as one mid-scrape (see "Step-level visibility") — the
long durable timer *is* the audit trail's runtime counterpart.

**This depends on Temporal actually existing, unlike everything else
implemented so far.** `cmd/routesearch`'s direct-run CLI has no way to
"sleep for months" durably — a plain process can't outlive itself. Until
Temporal is wired in (see "Collector task queue"), the honest interim
behavior is just detecting the condition and saying so up front (rather
than silently burning the query budget the way the Dec 2027 demo just
did) — not attempting the defer/wake mechanism itself.

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
argues for the opposite: deliberately **space scrapes out** — a Temporal
durable timer between each query (minutes to hours, not seconds) — which
is strictly safer against anti-bot detection than firing legs back to
back, and costs nothing extra since there's no deadline to race against.
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
kept), each tagged with the Temporal workflow/run ID, so a `route_search_
plans` row and its log lines cross-reference each other.

**Step-level visibility**: each row-level event above (generate
candidates, query one leg, prune decision, Pareto update) is its own
Temporal Activity, not buried inside one opaque "search" call — Temporal
already records every activity's input/output/timing/retries in its Web
UI (already planned in "Components"/"Local dev fit"), so this gets "the
whole search visible step by step" for free from modeling the workflow
this way, no new component required. The structured JSON logs above are
the log-search half of the same requirement, tagged so they line up with
that same Temporal history.

**Open decisions** (defaults chosen the same way as elsewhere in this
doc — say so if you'd rather change them):
- **Query budget**: not asked — defaulting to **20 scrapes/request**,
  now read as a cost/scope cap rather than a latency cap (see above).
  Say so if you want it higher/lower, or configurable per request.
- **Max depth**: not asked — defaulting to **1-stop only**, with 2-stop
  behind an opt-in flag.
- **Query spacing**: not asked — defaulting to a **random 5–30 minute
  Temporal timer between scrapes**. Say so if you want it tighter/looser.
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
Kafka/Strimzi, keyed by route; Kafka client = `confluent-kafka-go`;
response channel = both email reply and search-api; search-api
triggers collection on a miss, not just email; fan-out/fan-in for
complex requests and fetch retries = Temporal (see "Collector task
queue"), not a hand-rolled tracking table.

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
  in the stack (Kafka, Spark, Delta Lake, dbt, Go, Postgres) blocks
  arm64. Defaulting to **arm64/Graviton** for this reason; say so if
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
- **Kafka**: same as prod, no substitution — Strimzi, single-broker
  KRaft mode (no ZooKeeper), deployed into the local cluster with
  lower resource requests/limits in `values-local.yaml`.
- **Temporal**: `temporal server start-dev` (its own built-in
  SQLite-backed dev server) instead of a Helm-deployed Temporal +
  Postgres — matches the SQLite-for-simplicity choice already made
  for the serving store, and needs no extra cluster resources.
- **Spark**: same as prod default, no substitution — the
  Spark-on-Kubernetes operator runs inside the local cluster too
  (not `local[*]` mode), affordable here since data volumes are tiny
  (bounded by test-fixture requests, per "Collection scope").
- **Email intake**: SES inbound can't run locally — it's an
  AWS-managed delivery hop, not something to emulate. Local dev
  exposes a small HTTP endpoint/CLI that accepts a raw email fixture
  file and runs the same parsing code SES would trigger, publishing
  onto the local Kafka topic exactly like prod. This tests everything
  downstream of "an email arrived"; it doesn't test AWS's delivery of
  the email itself, which isn't testable locally regardless.
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
     search-api/collector/email-intake, Spark operator (Helm), plus
     the Temporal dev server (not Helm — see above).
  2. POST a sample raw-email fixture to the local email-intake
     endpoint (exercises the email path), or query `search-api` for a
     route that's a known miss (exercises the search-triggered path).
  3. Watch it flow: Kafka → collector starts a Temporal workflow
     (mock provider activity) → MinIO raw zone → Spark batch → Delta
     Lake (on MinIO) → dbt gold → sync → SQLite.
  4. Query `search-api` for that route/date and assert the result.
- **Sizing**: a single k3d node running Kafka (1 broker) + the Spark
  operator + MinIO + a handful of small Go services + the lightweight
  Temporal dev server should fit an M1 MacBook's unified memory at low
  resource requests — flagging as an assumption to revisit if it turns
  out too heavy, not a blocker.

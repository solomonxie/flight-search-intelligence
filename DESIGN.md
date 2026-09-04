# Design

Target architecture. See `README.md` "Status" for what's actually built
vs. still scaffold.

Deployment: AWS EC2, self-managed (not managed services like EKS/RDS/
EMR/MSK) — Terraform provisions the EC2 instances and networking,
Ansible configures them (installs/joins Kubernetes, per the `ansible`
skill's role conventions), every component ships as a Docker image,
and Kubernetes (self-managed control plane on that EC2 fleet) runs
them via Helm charts. This replaces the AWS CDK stack previously under
`infra/`, which was removed by request. `docker-compose.yml` is local
dev only.

## Data flow

Collection is **on-demand, not a mass crawl**: nothing gets scraped
until a user emails a request for a specific route/date. This bounds
scrape volume to actual requests (see "Collection scope" below).

```
 user email                search-api miss
(route+dates)            (route/date not yet
     │                       collected)
     ▼                            │
┌─────────────┐                   │
│ email intake │                  │
│ (SES inbound)│                  │
└──────┬───────┘                  │
       │ parse + publish          │ publish
       ▼                          ▼
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
  the request email, parses route/dates/requester from it, and
  publishes a collection job onto the Kafka request topic.

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
  else is self-managed. Nothing under `infra/` exists yet — this is
  target state, not current.

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

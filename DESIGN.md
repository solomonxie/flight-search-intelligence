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

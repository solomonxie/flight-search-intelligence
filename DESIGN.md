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
   user email                                     search-api miss
 (route + dates)                                  (route/date not
      │                                            yet collected)
      ▼                                                  │
┌───────────────┐  parse request      ┌──────────────┐   │
│ email intake   │────────────────────▶│ Kafka topic  │◀──┘
│ (SES inbound)  │                     │ (Strimzi,    │
└───────────────┘                     │  route+dates+│
                                        │  requester)  │
                                        └──────┬───────┘
                                               │ dequeue
                                               ▼
                                        ┌──────────────┐  raw result   ┌──────────────┐
                                        │ cmd/collector │──────────────▶│  S3 raw zone │
                                        │ (Go) — fetches│                └──────┬───────┘
                                        │ ONLY the      │                       │
                                        │ requested     │                       │ periodic batch
                                        │ route on-     │                       ▼
                                        │ demand        │                ┌──────────────┐
                                        └──────┬────────┘                │ Spark batch  │
                                               │ notify when ready       │  job         │
                                               ▼                         └──────┬───────┘
                                        ┌──────────────┐                        │
                                        │ email reply + │                        ▼
                                        │ search-api    │         ┌────────────────────────────────────┐
                                        │ (both)        │         │ Delta Lake (bronze → silver, on S3) │
                                        └──────────────┘         │ accumulates requested-route history │
                                                                   └───────────────┬──────────────────────┘
                                                                                    │ dbt (Spark SQL)
                                                                                    ▼
                                                                    ┌────────────────────────────────────┐
                                                                    │ Delta Lake gold — fare trends per   │
                                                                    │ previously-requested route          │
                                                                    └───────────────┬──────────────────────┘
                                                                                    │ sync job
                                                                                    ▼
                                                                             ┌──────────────┐
                                                                             │  Postgres     │
                                                                             │ (serving store)│
                                                                             └──────┬────────┘
                                                                                    ▼
                                                                             ┌──────────────┐
                                                                             │cmd/search-api │
                                                                             └──────────────┘
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

- **`cmd/search-api` (Go)** — normally read-only against the Postgres
  serving store, but on a miss (route/date not yet collected) it also
  publishes onto the same Kafka request topic and returns a "pending"
  response rather than an empty one — the second of two producers into
  that topic.

- **`cmd/collector` (Go)** — on-demand only: consumes the Kafka request
  topic (self-hosted via Strimzi — one job type, no fan-out/replay
  need), fetches that single route/date from the provider, writes the
  raw result to the S3 raw zone, and signals completion on both the
  email-reply and search-api-availability paths (both are wired, per
  the resolved response-channel decision). No scheduled/broad scraping
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
  publishes the gold Delta tables into Postgres, so `search-api` reads
  (the hit path, above) never query Delta Lake directly and stay
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

## Open decisions

**Resolved:** cloud = AWS EC2, self-managed compute; IaC = Terraform
(provision) + Ansible (configure) + Docker + Kubernetes + Helm
(deploy); object storage = S3; inbound email = SES; request queue =
Kafka/Strimzi; response channel = both email reply and search-api;
search-api triggers collection on a miss, not just email.

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

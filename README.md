# Flight Search Intelligence

A distributed flight price intelligence platform: users email a route
request, which triggers a Go collector to fetch just that route
on-demand (no broad/scheduled scraping); Spark processes the
accumulating results into Delta Lake, dbt/Airflow turn that into an
analytics-ready layer, and a Go API serves low-latency search against
a synced serving store built from every route ever requested.

See `DESIGN.md` for the full data-flow diagram and open decisions.

## How it works

- **Email intake** — SES receives an inbound route-request email
  (origin, destination, dates), parses it, and publishes an on-demand
  collection job onto a Kafka topic.
- **Collector** (`cmd/collector`) — consumes that topic and starts a
  Temporal workflow per task, which fetches *only* the requested route
  from providers, on-demand (no scheduled crawl of routes nobody asked
  about), with built-in retry and, for multi-leg/complex requests,
  fan-out into child workflows; drops the raw result in the S3 raw
  zone and replies by email + makes it available via search-api once
  done.
- **ETL** (`etl/`) — a periodic Spark batch job cleans/dedupes
  whatever's accumulated since the last run and merges it into Delta
  Lake; dbt models the silver layer into an analytics-ready gold layer
  (fare trends per requested route); Airflow orchestrates that chain
  and the gold → serving-store sync.
- **Search API** (`cmd/search-api`) — serves flexible, low-latency
  search (route, date range, airline, price, stops...) against the
  serving store synced from the gold layer (Postgres in prod, SQLite
  for local dev); a miss also publishes a collection job onto the same
  Kafka topic email intake uses, so browsing can pull in new routes
  too, not just email.
- **Infra** — self-managed on AWS EC2: Terraform provisions the
  instances, Ansible installs a self-managed Kubernetes cluster on
  them, and every service deploys as a Docker image via Helm charts
  (no `infra/` directory exists yet — see `DESIGN.md`).

## Layout

| Path | What |
|---|---|
| `cmd/collector` | on-demand, per-route fare collection |
| `cmd/search-api` | flight search service (also triggers collection on a miss) |
| `etl/spark` | periodic cleaning job → Delta Lake |
| `etl/dbt` | Delta Lake silver → gold analytics models |
| `etl/airflow/dags` | periodic ETL + serving-sync orchestration |

## Status

Early scaffold — see `DESIGN.md` for the target architecture
(email/search-triggered on-demand collection via Kafka/Temporal, Delta
Lake, serving sync) this doesn't yet implement. The collector
(`cmd/collector`) is the one piece with real logic: run directly, it
fetches real fare offers from the Amadeus test API for one route/date
and writes the raw JSON locally (see "Local dev" below) — no Kafka, no
Temporal, no S3 yet, just the provider fetch proven out end to end.
Everything else is still scaffold: `search-api` is unimplemented, the
Spark job writes plain files (no Delta Lake), dbt/search-api still
assume a Postgres table pair (no gold layer, no sync, no
miss-triggers-collection behavior), and Airflow only runs a nightly
batch chain. There is no infra/deployment code at all right now — no
Terraform, no Ansible roles, no Helm charts, no Dockerfiles — and no
email intake, Kafka, or Temporal exist yet either. See the `TODO`s
throughout.

## Local dev

Target: the full stack runs locally on a MacBook M1 in a local
Kubernetes cluster (k3d), fully testable end to end — no AWS account
needed. MinIO substitutes for S3, SQLite for Postgres, and the
collector runs against a mock fare provider instead of real ones. See
`DESIGN.md`'s "Local development" section for the full plan; none of
this tooling exists in the repo yet.

Until then, the collector runs directly (no Docker, no cluster) against
the real Amadeus Self-Service test API:

```sh
cp .env.example .env          # fill in AMADEUS_CLIENT_ID/SECRET
                               # (free: https://developers.amadeus.com/my-apps)
go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05
```

This fetches real (test-environment) fare offers, prints a summary, and
writes the raw JSON response to `data/raw/`. It bypasses Kafka/Temporal/S3
for now — see `DESIGN.md` "Collector task queue" for the queue-driven
version this will grow into.

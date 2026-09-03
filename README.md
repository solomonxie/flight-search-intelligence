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
- **Collector** (`cmd/collector`) — consumes that topic and fetches
  *only* the requested route from providers, on-demand (no scheduled
  crawl of routes nobody asked about), dropping the raw result in the
  S3 raw zone; replies by email and makes the result available via
  search-api once done.
- **ETL** (`etl/`) — a periodic Spark batch job cleans/dedupes
  whatever's accumulated since the last run and merges it into Delta
  Lake; dbt models the silver layer into an analytics-ready gold layer
  (fare trends per requested route); Airflow orchestrates that chain
  and the gold → serving-store sync.
- **Search API** (`cmd/search-api`) — serves flexible, low-latency
  search (route, date range, airline, price, stops...) against the
  Postgres serving store synced from the gold layer; a miss also
  publishes a collection job onto the same Kafka topic email intake
  uses, so browsing can pull in new routes too, not just email.
- **Infra** — self-managed on AWS EC2: Terraform provisions the
  instances, Ansible installs a self-managed Kubernetes cluster on
  them, and every service deploys as a Docker image via Helm charts
  (no `infra/` directory exists yet — see `DESIGN.md`).

## Layout

| Path | What |
|---|---|
| `cmd/collector` | on-demand, per-route fare collection |
| `cmd/search-api` | flight search service (also triggers collection on a miss) |
| `internal/db/migrations` | Postgres (serving store) schema |
| `etl/spark` | periodic cleaning job → Delta Lake |
| `etl/dbt` | Delta Lake silver → gold analytics models |
| `etl/airflow/dags` | periodic ETL + serving-sync orchestration |

## Status

Early scaffold, and currently batch-only/Postgres-only end to end —
see `DESIGN.md` for the target architecture (email/search-triggered
on-demand collection, Delta Lake, serving sync) this doesn't yet
implement. Service entrypoints, one migration, and one file per ETL
tool are in place; the collector only does scheduled batch scrape of
whatever it's pointed at (no on-demand/queue-driven mode yet), the
Spark job writes plain files (no Delta Lake), dbt/search-api still
read/write the same Postgres table pair directly (no gold layer, no
sync, no miss-triggers-collection behavior), and Airflow only runs the
nightly batch chain. There is no infra/deployment code at all right
now — no Terraform, no Ansible roles, no Helm charts, no Dockerfiles —
and no email intake or Kafka exist yet either. See the `TODO`s
throughout.

## Local dev

```sh
docker compose up -d          # Postgres
go run ./cmd/collector
go run ./cmd/search-api
```

Production is meant to run each service as a Docker image on
Kubernetes rather than via `go run`/`docker compose`, once that
infra exists — see `DESIGN.md`.

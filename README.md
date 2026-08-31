# Flight Search Intelligence

Search flights fast and flexibly by continuously collecting fare data
ourselves instead of hitting a provider's API on every user query.

## How it works

- **Collector** (`cmd/collector`) — scrapes flight fares from providers
  on a schedule and writes them to Postgres.
- **Search API** (`cmd/search-api`) — serves flexible, low-latency
  search against that same database (route, date range, airline, price,
  stops...) for the frontend.
- **ETL** (`etl/`) — Spark cleans/dedupes raw scraped rows, dbt models
  them into a queryable warehouse layer, and Airflow orchestrates the
  nightly run of both.
- **Infra** (`infra/cdk`) — AWS CDK (Go) for deployment.

## Layout

| Path | What |
|---|---|
| `cmd/collector` | scraping / data-collection service |
| `cmd/search-api` | flight search service |
| `internal/db/migrations` | Postgres schema |
| `etl/spark` | raw-data cleaning job |
| `etl/dbt` | warehouse transformation models |
| `etl/airflow/dags` | pipeline orchestration |
| `infra/cdk` | AWS deployment (Go CDK) |

## Status

Scaffold only — service entrypoints, one migration, and one file per ETL
tool are in place; scraping/search/transformation logic is not yet
implemented (see the `TODO`s throughout).

## Local dev

```sh
docker compose up -d          # Postgres
go run ./cmd/collector
go run ./cmd/search-api
```

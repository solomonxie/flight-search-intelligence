# ETL Pipeline

The periodic batch chain DESIGN.md's data-flow diagram puts *after*
collection: raw scraped fares land in the S3/MinIO raw zone (written by
`cmd/collector`, see its README), then get cleaned into Delta Lake
(Spark), modeled into a gold analytics layer (dbt), and synced into the
serving store `cmd/search-api` reads — orchestrated on a schedule by
Airflow. This is event-driven *up to* collection (an email or a
`search-api` miss triggers a scrape — see DESIGN.md "Collection scope")
and schedule-driven *after* it — nothing in this folder decides what to
scrape, only what to do with what's already been scraped.

**Status: every file below is a scaffold** — real task bodies, real
Spark logic, and real dbt tests are all still `TODO`. What exists is the
shape of the pipeline, wired together, not the pipeline itself.

## `airflow/dags/flight_pipeline_dag.py`

The orchestration: one DAG (`flight_pipeline`, daily schedule) chaining
three tasks — `run_collector` → `spark_clean` → `dbt_build`. Each is
currently a `BashOperator` stub that just echoes what it's meant to run
(`spark-submit etl/spark/clean_raw_flights.py`, `dbt build --project-dir
etl/dbt`) — not real triggers yet. Per DESIGN.md "Components," this is
the periodic half of the pipeline only; the collector itself is
triggered by email/search-api demand, not by this schedule.

## `spark/clean_raw_flights.py`

The batch job that dedupes/cleans whatever raw drops have accumulated
since the last run and merges them into Delta Lake (not plain Parquet —
each run needs to upsert into shared tables without clobbering prior
requested-route history, per DESIGN.md). Currently just opens and closes
a `SparkSession`; the actual cleaning (dedupe by `(origin, destination,
airline, depart_date, source)`, drop rows with a missing price,
normalize currency) is a `TODO` in the file's own docstring.

## `dbt/`

Models the Delta silver layer into a gold, analytics-ready layer (fare
trends per requested route) via Spark SQL.

- **`dbt_project.yml`** — project config: profile name
  `flight_search_intelligence`, staging models materialized as views.
- **`models/staging/sources.yml`** — declares the `raw.flight_prices`
  source dbt reads from. Table shape lives in
  `databases/sqlite/migrations/` (see `databases/README.md`), not here —
  this file only names what dbt points at.
- **`models/staging/stg_flights.sql`** — the one staging model so far: a
  passthrough select over `raw.flight_prices`' columns. `TODO`, per its
  own comment: dedup logic and tests, once the real source table's shape
  (post-Spark-cleaning) is settled.

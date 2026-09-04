# The Nightly Cleanup & Reporting Job (Not Built Yet)

This folder is about what happens to flight-price data *after* it's
already been collected: cleaning it up and turning it into useful
trends/reports, on a schedule (like once a day) — separate from "someone
just asked about a flight, go check it right now," which is handled
elsewhere (see `cmd/README.md`).

**Nothing in this folder actually works yet.** It's a sketch of the
shape the pipeline will take, with placeholder steps that don't do
anything real yet — think of it as labeled empty boxes, not a working
machine.

## `airflow/dags/flight_pipeline_dag.py`

The daily schedule: "run the collector, then clean the data, then build
the reports" — in that order, once a day. Right now each of those three
steps is just a placeholder that prints a message instead of actually
doing anything.

## `spark/clean_raw_flights.py`

Meant to remove duplicate or bad price entries before they're used for
reporting (e.g. the same flight price accidentally saved twice). Right
now it just starts up and immediately shuts back down — no real cleanup
logic yet.

## `dbt/`

Turns cleaned data into named, reusable tables/reports.

- **`dbt_project.yml`** — basic project settings.
- **`models/staging/sources.yml`** — says where the raw data comes from.
- **`models/staging/stg_flights.sql`** — a first, very basic step that
  just copies the data over as-is — no real cleanup logic added yet.

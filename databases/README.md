# How the Database Is Set Up

This project needs a database to store things like flight prices found
and the progress of each search. This page is about how the *shape* of
that database gets created — which tables exist, what columns they
have — not about the data inside it.

**The one rule that matters: the application itself is never allowed to
create or change tables.** Table changes are made by a separate tool, run
on purpose by a person (or a deployment step) — never automatically,
just because the app happened to start up. This keeps "the app changed my
table by accident" from ever being possible.

## How it actually works

Every table change is written as its own small, numbered file, e.g.
`V001__create_flight_prices.sql`. A tool called
[Flyway](https://flywaydb.org) reads these files in order and applies
each one exactly once, keeping track of which ones it's already run so
it never re-applies the same change twice.

To set up your local database, run:

```sh
make db-init
```

That's it — this creates the database file (if it doesn't already exist)
and brings it up to date.

## What's in this folder

- **`sqlite/`** — what's actually used today, for local testing.
  Contains the numbered change files (`migrations/`) and a small config
  file telling Flyway where the database lives.
- **`postgres/`** — an empty placeholder for the real database the
  live/production version of this project will eventually use. Nothing
  here yet.
- **`clickhouse/`** — just an empty leftover folder sitting on disk. It's
  not part of the plan anywhere else in this project, and it isn't even
  saved in the project's history — noting it here so it isn't a mystery,
  not because it means anything yet.

# Cheap Flight Finder

> 🚧 Work in progress. The search engine itself is real and runs today
> (see "Try it now" below) — the email-based product around it doesn't
> exist yet.

Finds flights cheaper than a plain search shows you, by trying things a
single query misses: splitting a trip across two separate tickets
through a connecting city, checking nearby dates for a better price,
and comparing a bundled round-trip fare against buying both directions
on their own. Eventually: email in a request in plain language —
"sometime in December, budget around $800, traveling with an infant so
nothing too long" — and get real options back, with room to add more
context while it's still searching.

## Features

- **Real fares, no paid API** — pulled live, directly from Google
  Flights.
- **Finds routes a plain search misses** — splitting a trip across two
  separate one-way tickets through a connecting city sometimes beats the
  official fare through that same city.
- **Round-trip check** — compares a bundled round-trip fare against
  buying both directions separately, keeps whichever actually works out
  cheaper.
- **Flexible dates** — sweeps a window of nearby dates first (real
  swings of hundreds of dollars a few days apart are common) before
  spending the more expensive route-hunting effort on the best one.
- **Full transparency** — every route it considered, and why it was kept
  or ruled out, is on the record — not just the final answer.
- **Long layovers are flagged, not hidden** — in case a long layover is
  something you'd plan around (a stopover) rather than pay to avoid.
- **Planned**: email in a request in plain language, including the kind
  of preference a form can't capture ("must be there for a specific
  date," "traveling with a baby"), with the ability to add more context
  while it's still working on it.

## Try it now

No account, no API key, nothing to configure beyond having Go and
[Flyway](https://flywaydb.org) installed — `brew install flyway`, or
`ansible-playbook ansible/playbooks/mac_dev.yml` to provision everything
this needs at once. First time only, set up the local database (Flyway
applies the versioned migrations under `databases/sqlite/migrations/` —
schema is DBA/ops tooling's job, not Go's; see `DESIGN.md` "Schema
ownership"):

```sh
make db-init
```

Then:

```sh
go run ./cmd/routesearch -origin SFO -destination JFK -date 2026-12-05
```

Add a return date for the round-trip comparison, and/or
`-date-window-days` to sweep nearby dates too:

```sh
go run ./cmd/routesearch -origin YVR -destination PEK \
  -date 2026-12-22 -return-date 2027-01-05 -date-window-days 15
```

See `DESIGN.md` for how it works under the hood and what's still ahead.

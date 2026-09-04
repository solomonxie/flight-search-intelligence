# Database Schema Management

**Resolved rule, applies to every database this project ever has: Go code
never creates or alters schema.** No `CREATE TABLE`, no `ALTER TABLE`,
nowhere in any `cmd/`/`internal/` package — those packages only insert,
update, and select. Schema is DBA/ops tooling's job, kept structurally
separate from application code: the same way Terraform provisions a
database *server* but never reaches into table-level DDL, a different
layer, different tooling, a more cautious change process. See DESIGN.md
"Schema ownership" for the full reasoning (in short: an app process that
can alter its own schema has no boundary between "my code changed" and
"my data's shape changed," two risk profiles production practice keeps
apart on purpose).

## How it's managed

**Flyway**, versioned migrations, one folder per database:

```
databases/
  sqlite/
    flyway.toml          # [environments.default].url + [flyway].locations
    migrations/
      V001__create_flight_prices.sql
      V002__create_route_search_plans.sql
      V003__create_agent_requests.sql
      V004__create_agent_tasks.sql
  postgres/               # placeholder — same pattern, once prod needs it
  clickhouse/             # placeholder — not yet part of the documented
                           # design (not mentioned in DESIGN.md); flagging
                           # rather than inventing a purpose for it
```

Each migration is hand-written, explicit SQL — not a diff-based/
declarative tool (Liquibase-style auto-generated diffs, or Atlas). Less
surface for a tool's own dialect-translation logic to get a migration
subtly wrong across versions. Naming (`V<version>__<description>.sql`,
double underscore) is Flyway's own required format, not a style choice —
a single underscore fails to parse. `flyway_schema_history`, a table
Flyway creates in the target database itself, tracks exactly which
migrations have run.

## How it works today (SQLite, local dev)

```sh
make db-init
# -> flyway -configFiles=databases/sqlite/flyway.toml migrate
```

This creates `flyway_schema_history` and applies every migration in
`sqlite/migrations/` in order. `internal/catalog.Open` then checks (a
read, not schema management) that every table it expects actually
exists, and fails fast with a pointer back to `make db-init` if this step
was skipped — rather than letting the first real query fail with a
confusing "no such table."

## Target (Postgres in prod)

Same principle, realized as a Helm pre-install/pre-upgrade hook Job — a
one-shot Kubernetes Job, gated to complete before the application
Deployment rolls out, running the official `flyway/flyway` image against
`databases/postgres/migrations/`. Schema application stays an infra
artifact (a Job spec + migration files), never application runtime code,
in prod exactly as in local dev. Not built yet — `databases/postgres/` is
still an empty placeholder.

## Provisioning Flyway itself

A dev-machine/CI setup step, not something this repo's own tooling
installs — consistent with the rule above: the migration tool is
infra/ops-provisioned, not something Go (or any app-side script) pulls in
for itself. Locally: `ansible-playbook ansible/playbooks/mac_dev.yml`, or
`brew install flyway` by hand. In CI/prod: the `flyway/flyway` Docker
image.

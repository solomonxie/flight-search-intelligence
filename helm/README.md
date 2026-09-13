# Helm charts

One chart per `cmd/` binary (DESIGN.md "Infra"), plus the self-managed
Postgres/Kafka workloads and the shared plumbing they need. **Not
installed anywhere** — these are reviewable manifests, not a running
deployment (no cluster exists to install them into yet — see
`terraform/README.md` and `ansible/roles/k8s_fleet`). `helm lint`/
`helm template` weren't run either — Helm isn't available in this
environment; verified by hand instead.

## Charts

| Chart | Kind | Depends on |
|---|---|---|
| `sqlite-data` | PVC | — |
| `agent-worker` | Deployment | `sqlite-data`, `kafka` |
| `collector` | Deployment | `sqlite-data`, `kafka` |
| `search-api` | Deployment + Service | — (scaffold, see `cmd/README.md`) |
| `email-intake` | Job (per invocation) | `sqlite-data`, `kafka` |
| `kafka` | Strimzi `Kafka`/`KafkaNodePool`/`KafkaTopic` | Strimzi operator (external, see below) |
| `postgres` | StatefulSet + Service + Secret | — |
| `flyway-migrate` | library (shared Job template) | — (not yet included by any chart) |

## Install order, once a cluster exists

```
helm repo add strimzi https://strimzi.io/charts/
helm install strimzi-operator strimzi/strimzi-kafka-operator -n kafka --create-namespace

helm install kafka ./helm/kafka -n kafka
helm install sqlite-data ./helm/sqlite-data
helm install agent-worker ./helm/agent-worker
helm install collector ./helm/collector
helm install search-api ./helm/search-api

# per request/follow-up, not a standing release:
helm install email-intake-<id> ./helm/email-intake --set mode=start --set text="..."

# standalone — see "Known gap" below before relying on this for anything:
helm install postgres ./helm/postgres
```

## Known gap: SQLite-in-Kubernetes is a stopgap, Postgres isn't wired in yet

`internal/catalog` only has a SQLite backend today (`sql.Open("sqlite",
path)`) — DESIGN.md's "Local development" section already flags this:
a storage-driver abstraction (SQLite locally, Postgres in prod) doesn't
exist in Go yet. That's a real gap in the *application*, not something
this Helm layer can paper over, so it isn't pretended away here:

- `agent-worker`, `collector`, `email-intake`, and (once it's real)
  `search-api` all need to see the *same* SQLite file — the whole point
  of a shared serving store. A `ReadWriteOnce` EBS volume can only
  attach to one node at a time, but Kubernetes *does* allow multiple
  pods on that same node to mount it concurrently. So `sqlite-data`
  provisions one shared PVC, and every SQLite-touching chart's
  `nodeSelector` (`sqlite-host: "true"`) pins its pod to whichever node
  an operator has labeled to match — an explicit, load-bearing manual
  step, not automatic. This only works at `replicas: 1` per component,
  and only as long as SQLite's known weakness under concurrent
  cross-process writes doesn't get exercised harder than local dev
  already exercises it.
- `helm/postgres` and `helm/flyway-migrate` build the actual target
  DESIGN.md commits to — a real, standalone, self-managed Postgres plus
  the shared pre-install/pre-upgrade migration hook — but **no chart
  currently depends on either one**. Wiring `agent-worker`/`collector`/
  `search-api` to Postgres instead of the `sqlite-data` stopgap needs
  `internal/catalog` to grow a Postgres driver first (a Go-side change,
  not a Helm one) — not attempted here; flagged in
  `IMPLEMENTATION_PLAN.md`'s backlog as a follow-up this work surfaced.
- Once that driver lands: drop `sqlite-data`/`nodeSelector` from the
  three app charts, add `flywayMigrate: {enabled: true, ...}` to each
  (wiring in the library chart's `flyway-migrate.job` template), and
  point `DB_DSN`/whatever the new driver expects at `helm/postgres`'s
  `postgres` Service instead of a mounted file.

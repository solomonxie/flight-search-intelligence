# Agent Loop Reconciler

The reconciler for `internal/agents`' agent loop, plus a dev-only CLI
standing in for SES inbound until that's actually built. See DESIGN.md
"Agent loop" and "Where this lives in the repo" — in prod, this is what
SES inbound hands a request email to; today it's three CLI modes in one
`main.go`, all against the local SQLite serving store.

### `main.go`

Three modes, chosen by flag:

- **`-worker`** (`runWorker`) — the actual reconciler: on each poll
  tick, lists every non-finalized `agent_requests` row
  (`ListActiveAgentRequests`) and calls `agents.AdvanceRequest` on each
  one. This is the process that has to be running for a created request
  to ever move — `cmd/collector -worker` alone only runs tasks that
  already exist, it doesn't decide to create them.
- **`-start`** (`startRequest`) — creates a new request: builds a `Spec`
  from the `-origin`/`-destination`/`-date`/`-max-hours`/`-budget`/
  `-soft-constraints` flags (`agents.NewRequest`) and inserts it
  (`CreateAgentRequest`). Stands in for "an initial request email
  arrived." `-wait` is a dev convenience that then polls the row until
  `-worker` finalizes it and prints the drafted email — real SES intake
  would never block like this, it would just create the row and return.
- **`-signal`** (`sendFollowUp`) — appends a plain-language constraint to
  an existing request's `Spec` (`agents.AppendSoftConstraint` +
  `UpdateAgentRequestSpec`). Stands in for "a reply arrived on an
  existing thread." No signal/notification mechanism needed on the
  receiving end — the next time `-worker` reads that row, the updated
  spec is just what's there.

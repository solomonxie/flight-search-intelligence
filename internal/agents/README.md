# Agent Loop

The state machine behind DESIGN.md's "Agent loop: LLM drives the search,
Go stays narrow." Deliberately not a workflow engine — see the package
doc at the top of `reconcile.go` for the full reasoning: durability comes
from committing state to `agent_requests`/`agent_tasks` rows
(`internal/catalog`) before returning, not from a replay log, and "async"
means dispatching a search is an `INSERT` that returns immediately, never
a blocked wait.

This package holds only the *logic*. The two processes that actually run
it live in `cmd/email-intake` (the poll loop that calls `AdvanceRequest`
on every non-finalized request) and `cmd/collector` (the poll loop that
claims and runs the tasks this package dispatches).

### `spec.go`

The shared types every other file works with: `Spec` (a request's
structured understanding — concrete fields plus a plain-language
`SoftConstraints` list that deliberately never becomes typed fields, per
DESIGN.md's "Agent loop"), `Decision`/`Action` (what the decision step
returns — dispatch, defer, or finalize), `RoundRecord` (one loop
iteration's audit-trail entry, including the `TaskID` it dispatched and
the `Result` once known), and `Outcome` (the final drafted email plus why
it stopped).

### `task.go`

`CollectRouteRequest`/`CollectRouteResult`/`CollectRouteOffer` — the
payload shape written into `agent_tasks.params_json` and read back from
`result_json`. This is the whole contract between this package and
`cmd/collector`; neither imports the other's package, just this one.

### `decide.go`

`DecideNextAction` and `DraftFinalEmail` — currently **deterministic
stubs standing in for a real LLM call** (DESIGN.md "LLM choice / call
shape: deferred entirely"). `DecideNextAction`'s stub policy: dispatch
the spec as given on round 0; after a result, finalize if it has any
offer, otherwise widen `MaxHours`/`QueryBudget` and retry. It does *not*
evaluate soft constraints — that's flagged explicitly in `Reasoning`
rather than silently skipped, since judging "does this actually satisfy
'must be there for Christmas'" is exactly the part a real LLM call is
for. Both functions take `ctx context.Context` even though the stub
doesn't need it, so swapping in a real call later touches only this file.

### `reconcile.go`

The state machine itself. `AdvanceRequest` is one poll tick's worth of
work for one request: read its row, and if (and only if) it's actually
ready to move, do exactly one step —

- **`awaiting_decision`** → `advanceAwaitingDecision`: call
  `DecideNextAction`; if it says dispatch and the redispatch cap
  (`RedispatchCap = 3`, DESIGN.md's resolved default) isn't hit, insert a
  new `agent_tasks` row and move the request to `dispatched`; otherwise
  call `DraftFinalEmail` and move to `finalized`.
- **`dispatched`** → `advanceDispatched`: check the current round's task;
  still `pending`/`claimed` → do nothing this tick; `done` → fold the
  result into the round history and move back to `awaiting_decision`;
  `failed` → treated the same as "no offers" (the decide stub already
  knows how to retry that).

`NewRequest` and `AppendSoftConstraint` are the two entry points callers
(`cmd/email-intake -start`/`-signal`) use to create a request and land a
follow-up without either one needing to know `Spec`'s JSON encoding.

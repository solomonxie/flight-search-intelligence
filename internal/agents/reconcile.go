// Package agents holds the agent loop DESIGN.md's "Agent loop: LLM drives
// the search, Go stays narrow" describes — and, deliberately, no workflow
// engine. Durable state lives in agent_requests/agent_tasks
// (internal/catalog), not in a long-lived process or a replay log: each
// call to AdvanceRequest reads a row, does at most one step, and writes
// the result back before returning. A crash between ticks loses nothing,
// because nothing was ever held only in memory — the same property
// Temporal's durable execution gives, bought here with a state table and
// a poll loop instead of a workflow engine (see DESIGN.md "Agent loop"
// for why: this project's days-scale SLA and low, email-bounded request
// volume don't need Temporal's core value — high throughput, complex
// sagas, millisecond reactivity — so the row-plus-poller version trades a
// little latency between ticks for one fewer stateful service to run).
//
// "Async" here means: dispatching a search never blocks — it writes an
// agent_tasks row and returns; cmd/collector -worker claims and runs it
// on its own schedule, in its own goroutine. The reconciler
// (cmd/email-intake -worker) only ever touches requests that are actually
// ready to move, so one poll tick over thousands of "waiting" requests
// costs one query each, not a blocked goroutine each.
package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"flight-search-intelligence/internal/catalog"
)

// RedispatchCap is DESIGN.md's resolved default: "3 rounds before forced
// finalization."
const RedispatchCap = 3

// Request status values — agent_requests.status.
const (
	StatusAwaitingDecision = "awaiting_decision"
	StatusDispatched       = "dispatched"
	StatusDeferred         = "deferred" // not produced yet — DecideNextAction never returns ActionDefer; see DESIGN.md "Booking horizon"
	StatusFinalized        = "finalized"
)

// NewRequest builds a fresh Spec + its initial JSON encoding, ready for
// catalog.CreateAgentRequest — the entry point DESIGN.md's email intake
// (or, for now, cmd/email-intake -start) calls for a new request.
func NewRequest(origin, destination, departDate, returnDate string, maxHours float64, queryBudget int, softConstraints []string) (Spec, []byte, error) {
	spec := Spec{
		Origin: origin, Destination: destination, DepartDate: departDate, ReturnDate: returnDate,
		MaxHours: maxHours, QueryBudget: queryBudget, SoftConstraints: softConstraints,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return Spec{}, nil, fmt.Errorf("agents: marshaling new spec: %w", err)
	}
	return spec, b, nil
}

// AppendSoftConstraint decodes specJSON, appends text, and re-encodes —
// how a follow-up email (cmd/email-intake -signal) lands into a request's
// spec (DESIGN.md "Continuous email / mid-flight interruption") without
// the reconciler needing any separate signal mechanism: the next tick
// that reads this row just sees the updated spec.
func AppendSoftConstraint(specJSON []byte, text string) ([]byte, error) {
	var spec Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, fmt.Errorf("agents: decoding spec: %w", err)
	}
	spec.SoftConstraints = append(spec.SoftConstraints, text)
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("agents: encoding spec: %w", err)
	}
	return b, nil
}

// AdvanceRequest is one poll tick's worth of work for one request: read
// its row, and — only if it's actually ready to move — do exactly one
// step (examine a finished task, decide the next action, dispatch a new
// task, or finalize). A request whose dispatched task hasn't finished yet
// is left untouched; the caller (cmd/email-intake -worker) just calls this
// again next tick.
func AdvanceRequest(ctx context.Context, db *catalog.SQLite, row catalog.AgentRequestRow) error {
	var spec Spec
	if err := json.Unmarshal([]byte(row.SpecJSON), &spec); err != nil {
		return fmt.Errorf("agents: decoding spec for %s: %w", row.RequestID, err)
	}
	var rounds []RoundRecord
	if err := json.Unmarshal([]byte(row.RoundsJSON), &rounds); err != nil {
		return fmt.Errorf("agents: decoding rounds for %s: %w", row.RequestID, err)
	}

	switch row.Status {
	case StatusDispatched:
		return advanceDispatched(ctx, db, row.RequestID, rounds)
	case StatusAwaitingDecision:
		return advanceAwaitingDecision(ctx, db, row.RequestID, spec, rounds)
	default:
		return nil // deferred (unimplemented) or finalized: nothing to do
	}
}

// advanceDispatched checks whether the current round's task has finished
// and, if so, folds its result into the round history and hands the
// request back to "awaiting_decision" for the next tick to examine.
func advanceDispatched(ctx context.Context, db *catalog.SQLite, requestID string, rounds []RoundRecord) error {
	if len(rounds) == 0 {
		return fmt.Errorf("agents: %s is 'dispatched' with no rounds recorded", requestID)
	}
	last := &rounds[len(rounds)-1]
	task, err := db.GetAgentTask(ctx, last.TaskID)
	if err != nil {
		return err
	}

	switch task.Status {
	case "pending", "claimed":
		return nil // still waiting; try again next tick
	case "done":
		var result CollectRouteResult
		if task.ResultJSON.Valid {
			if err := json.Unmarshal([]byte(task.ResultJSON.String), &result); err != nil {
				return fmt.Errorf("agents: decoding task result for %s: %w", task.TaskID, err)
			}
		}
		last.Result = &result
	case "failed":
		// Treated the same as "no offers": DecideNextAction's stub policy
		// already widens and retries on that signal. The actual error is
		// still on the task row (task.Error) for debugging.
		last.Result = &CollectRouteResult{}
	default:
		return fmt.Errorf("agents: task %s has unknown status %q", task.TaskID, task.Status)
	}

	roundsJSON, err := json.Marshal(rounds)
	if err != nil {
		return fmt.Errorf("agents: encoding rounds for %s: %w", requestID, err)
	}
	return db.SaveAgentRequestState(ctx, requestID, StatusAwaitingDecision, roundsJSON, nil, "", "")
}

// advanceAwaitingDecision calls the (stub, for now) decision function and
// either dispatches a new task or finalizes the request.
func advanceAwaitingDecision(ctx context.Context, db *catalog.SQLite, requestID string, spec Spec, rounds []RoundRecord) error {
	decision, err := DecideNextAction(ctx, spec, rounds)
	if err != nil {
		return fmt.Errorf("agents: DecideNextAction for %s: %w", requestID, err)
	}

	if decision.Action == ActionDispatch && len(rounds) < RedispatchCap {
		round := len(rounds) + 1
		taskID := fmt.Sprintf("%s-round-%d", requestID, round)
		paramsJSON, err := json.Marshal(decision.Request)
		if err != nil {
			return fmt.Errorf("agents: encoding task params for %s: %w", taskID, err)
		}
		if err := db.CreateAgentTask(ctx, taskID, requestID, round, paramsJSON); err != nil {
			return err
		}

		rounds = append(rounds, RoundRecord{Round: round, Spec: spec, Decision: decision, TaskID: taskID})
		roundsJSON, err := json.Marshal(rounds)
		if err != nil {
			return fmt.Errorf("agents: encoding rounds for %s: %w", requestID, err)
		}
		return db.SaveAgentRequestState(ctx, requestID, StatusDispatched, roundsJSON, nil, "", "")
	}

	// Finalize: either the decision said so, or the redispatch cap forced
	// it (DESIGN.md: "hitting either forces finalize-with-what-you-have").
	finalizedBy := "satisfied"
	if decision.Action == ActionDispatch && len(rounds) >= RedispatchCap {
		finalizedBy = "round_cap"
	}

	emailBody, err := DraftFinalEmail(ctx, spec, rounds)
	if err != nil {
		return fmt.Errorf("agents: DraftFinalEmail for %s: %w", requestID, err)
	}
	roundsJSON, err := json.Marshal(rounds)
	if err != nil {
		return fmt.Errorf("agents: encoding rounds for %s: %w", requestID, err)
	}
	return db.SaveAgentRequestState(ctx, requestID, StatusFinalized, roundsJSON, nil, emailBody, finalizedBy)
}

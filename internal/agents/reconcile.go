// Package agents holds the agent loop DESIGN.md's "Agent loop: LLM drives
// the search, Go stays narrow" describes. Durable state lives in
// agent_requests/agent_tasks (internal/catalog), not in a long-lived
// process or a workflow engine's replay log — every step commits to a row
// before returning, so a crash mid-step loses nothing.
//
// What triggers a step is internal/kafka, not a poll loop: a request
// becomes ready to decide, or a task becomes ready to run, and something
// (a follow-up email, a finished search) pushes a tiny message saying so.
// That's what makes this "async" in the sense that matters — nothing sits
// there checking on a timer, a step only runs because something real
// happened. The functions in this file are what a message's consumer
// (cmd/agent-worker, cmd/collector) actually calls once it gets one; they
// don't know Kafka exists, they just take a request/task id and the
// database, and do exactly one step.
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
	// StatusAwaitingUser: Decide got ActionAskUser and parked here with
	// the question in email_body (reusing the same column the finalize
	// path already uses for its own free-text output — not a separate
	// schema field). Unlike a dispatched task, nothing is in flight to
	// wake this request back up: cmd/email-intake -signal explicitly
	// flips it back to StatusAwaitingDecision and republishes a
	// DecisionTrigger once the user answers.
	StatusAwaitingUser = "awaiting_user"
	StatusFinalized    = "finalized"
)

// Decide is one request's decision step — what cmd/agent-worker calls
// after reading a DecisionTrigger off internal/kafka's agent-decisions
// topic. It loads the row, asks DecideNextAction what to do, and either
// dispatches a new task (returning its id so the caller can push it onto
// the search-tasks topic), parks it in StatusAwaitingUser, or finalizes
// (the latter two both returning ok=false — the signal to push no
// further message, which is how the chain stops until something
// explicit — a task result, or cmd/email-intake -signal — wakes it again).
//
// Guards against a stale/duplicate trigger: a request already
// "dispatched" (still waiting on its current task) or "finalized" is left
// untouched — decide is only meaningful in "awaiting_decision".
func Decide(ctx context.Context, llm LLMClient, db *catalog.SQLite, requestID string) (taskID string, ok bool, err error) {
	row, err := db.LoadAgentRequest(ctx, requestID)
	if err != nil {
		return "", false, err
	}
	if row.Status != StatusAwaitingDecision {
		return "", false, nil // already handled, or not ready yet — not an error
	}

	var spec Spec
	if err := json.Unmarshal([]byte(row.SpecJSON), &spec); err != nil {
		return "", false, fmt.Errorf("agents: decoding spec for %s: %w", requestID, err)
	}
	var rounds []RoundRecord
	if err := json.Unmarshal([]byte(row.RoundsJSON), &rounds); err != nil {
		return "", false, fmt.Errorf("agents: decoding rounds for %s: %w", requestID, err)
	}

	decision, err := DecideNextAction(ctx, llm, spec, rounds)
	if err != nil {
		return "", false, fmt.Errorf("agents: DecideNextAction for %s: %w", requestID, err)
	}

	if decision.Action == ActionAskUser {
		round := len(rounds) + 1
		rounds = append(rounds, RoundRecord{Round: round, Spec: spec, Decision: decision})
		roundsJSON, err := json.Marshal(rounds)
		if err != nil {
			return "", false, fmt.Errorf("agents: encoding rounds for %s: %w", requestID, err)
		}
		if err := db.SaveAgentRequestState(ctx, requestID, StatusAwaitingUser, roundsJSON, nil, decision.Question, ""); err != nil {
			return "", false, err
		}
		return "", false, nil
	}

	if decision.Action == ActionDispatch && len(rounds) < RedispatchCap {
		round := len(rounds) + 1
		taskID = fmt.Sprintf("%s-round-%d", requestID, round)
		paramsJSON, err := json.Marshal(decision.Request)
		if err != nil {
			return "", false, fmt.Errorf("agents: encoding task params for %s: %w", taskID, err)
		}
		if err := db.CreateAgentTask(ctx, taskID, requestID, round, paramsJSON); err != nil {
			return "", false, err
		}

		rounds = append(rounds, RoundRecord{Round: round, Spec: spec, Decision: decision, TaskID: taskID})
		roundsJSON, err := json.Marshal(rounds)
		if err != nil {
			return "", false, fmt.Errorf("agents: encoding rounds for %s: %w", requestID, err)
		}
		if err := db.SaveAgentRequestState(ctx, requestID, StatusDispatched, roundsJSON, nil, "", ""); err != nil {
			return "", false, err
		}
		return taskID, true, nil
	}

	// Finalize: either the decision said so, or the redispatch cap forced
	// it (DESIGN.md: "hitting either forces finalize-with-what-you-have").
	// No task id, ok=false: cmd/agent-worker pushes nothing further, and
	// that absence of a next message is the whole "stop" signal.
	finalizedBy := "satisfied"
	if decision.Action == ActionDispatch && len(rounds) >= RedispatchCap {
		finalizedBy = "round_cap"
	}

	emailBody, err := DraftFinalEmail(ctx, llm, spec, rounds)
	if err != nil {
		return "", false, fmt.Errorf("agents: DraftFinalEmail for %s: %w", requestID, err)
	}
	roundsJSON, err := json.Marshal(rounds)
	if err != nil {
		return "", false, fmt.Errorf("agents: encoding rounds for %s: %w", requestID, err)
	}
	return "", false, db.SaveAgentRequestState(ctx, requestID, StatusFinalized, roundsJSON, nil, emailBody, finalizedBy)
}

// RecordTaskResult is what cmd/collector calls right after it saves a
// finished (or failed) task's outcome — folds that outcome into its
// request's round history and hands the request back to
// "awaiting_decision". Returns the request id so the caller can push a
// DecisionTrigger for it onto the agent-decisions topic, waking the next
// round's Decide call.
func RecordTaskResult(ctx context.Context, db *catalog.SQLite, taskID string) (requestID string, err error) {
	task, err := db.GetAgentTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	row, err := db.LoadAgentRequest(ctx, task.RequestID)
	if err != nil {
		return "", err
	}
	var rounds []RoundRecord
	if err := json.Unmarshal([]byte(row.RoundsJSON), &rounds); err != nil {
		return "", fmt.Errorf("agents: decoding rounds for %s: %w", task.RequestID, err)
	}
	if len(rounds) == 0 {
		return "", fmt.Errorf("agents: %s has no rounds recorded for task %s", task.RequestID, taskID)
	}
	last := &rounds[len(rounds)-1]

	switch task.Status {
	case "done":
		var result CollectRouteResult
		if task.ResultJSON.Valid {
			if err := json.Unmarshal([]byte(task.ResultJSON.String), &result); err != nil {
				return "", fmt.Errorf("agents: decoding task result for %s: %w", taskID, err)
			}
		}
		last.Result = &result
	case "failed":
		// Treated the same as "no offers": DecideNextAction's stub policy
		// already knows how to widen and retry on that signal. The
		// actual error is still on the task row (task.Error) for debugging.
		last.Result = &CollectRouteResult{}
	default:
		return "", fmt.Errorf("agents: task %s has status %q, not done/failed yet", taskID, task.Status)
	}

	roundsJSON, err := json.Marshal(rounds)
	if err != nil {
		return "", fmt.Errorf("agents: encoding rounds for %s: %w", task.RequestID, err)
	}
	if err := db.SaveAgentRequestState(ctx, task.RequestID, StatusAwaitingDecision, roundsJSON, nil, "", ""); err != nil {
		return "", err
	}
	return task.RequestID, nil
}

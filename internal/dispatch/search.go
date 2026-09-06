// Package dispatch holds "run one dispatched agent_tasks row to
// completion" — DESIGN.md "Agent loop" step 3 and "Collector task
// dispatch." Shared by cmd/collector -worker (Kafka-triggered: dispatch
// and its result cross a process boundary, so durability matters) and
// cmd/email-intake -interactive (run synchronously, in-process, so one
// live conversation needs no other process running — DESIGN.md's "hands
// the request off" reasoning is about surviving a crash between
// processes, which doesn't apply to a single terminal already blocked on
// its own output waiting for the answer anyway).
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/routesearch"
)

// RunTask loads taskID's params, runs the search it asks for, saves the
// outcome (done or failed — a search error is a task failure, not a
// function error, so the agent loop's own retry logic sees it the same
// way it sees "no offers"), and folds it back into the request via
// agents.RecordTaskResult. Returns the request id so the caller can
// decide what to do next — cmd/collector publishes a DecisionTrigger for
// it; cmd/email-intake's synchronous driver just calls agents.Decide
// again directly.
func RunTask(ctx context.Context, db *catalog.SQLite, deps routesearch.Deps, taskID string) (requestID string, err error) {
	task, err := db.GetAgentTask(ctx, taskID)
	if err != nil {
		return "", err
	}

	var req agents.CollectRouteRequest
	if err := json.Unmarshal([]byte(task.ParamsJSON), &req); err != nil {
		if saveErr := db.SaveAgentTaskResult(ctx, taskID, "failed", nil, fmt.Sprintf("decoding task params: %v", err)); saveErr != nil {
			return "", saveErr
		}
		return agents.RecordTaskResult(ctx, db, taskID)
	}

	result, searchErr := runSearch(ctx, deps, req)
	if searchErr != nil {
		if err := db.SaveAgentTaskResult(ctx, taskID, "failed", nil, searchErr.Error()); err != nil {
			return "", err
		}
	} else {
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("dispatch: encoding result for %s: %w", taskID, err)
		}
		if err := db.SaveAgentTaskResult(ctx, taskID, "done", resultJSON, ""); err != nil {
			return "", err
		}
	}
	return agents.RecordTaskResult(ctx, db, taskID)
}

// runSearch wraps the existing internal/routesearch.Search (one-way,
// already-built) as the unit RunTask dispatches to. Trims
// routesearch.Plan down to agents.CollectRouteResult — the full Plan
// (candidate-by-candidate audit trail) stays in the store via
// routesearch.Search's own catalog write, not duplicated into the task's
// result_json.
func runSearch(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest) (agents.CollectRouteResult, error) {
	plan, err := routesearch.Search(ctx, deps, routesearch.Params{
		Origin:            req.Origin,
		Destination:       req.Destination,
		DepartDate:        req.DepartDate,
		MaxHours:          req.MaxHours,
		QueryBudget:       req.QueryBudget,
		MaxPrice:          req.MaxPrice,
		MinLayoverMinutes: req.MinLayoverMinutes,
		MaxLayoverMinutes: req.MaxLayoverMinutes,
		PricePerMile:      0.08,
	})
	if err != nil {
		return agents.CollectRouteResult{}, err
	}

	out := agents.CollectRouteResult{
		RequestID:   plan.RequestID,
		QueriesUsed: plan.QueriesUsed,
	}
	for _, r := range plan.FinalResult {
		out.Results = append(out.Results, agents.CollectRouteOffer{
			PriceUSD:        r.PriceUSD,
			DurationMinutes: r.DurationMinutes,
			Path:            r.Path,
			SelfTransfer:    r.SelfTransfer,
		})
	}
	return out, nil
}

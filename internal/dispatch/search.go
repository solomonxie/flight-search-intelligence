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
	"flight-search-intelligence/internal/openflights"
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
// already-built) as the unit RunTask dispatches to. req.Origin/
// req.Destination may each be a real IATA code or a plain city name
// (FormSpec fills in a city name rather than guessing when a multi-
// airport city like Beijing or London is named but no specific airport
// is) — resolveAirports expands either into the airport(s) to actually
// search, and a city with N candidates fans out into one
// routesearch.Search call per candidate, pareto-merged into one combined
// result set spanning every airport tried. Trims routesearch.Plan down
// to agents.CollectRouteResult — the full Plan (candidate-by-candidate
// audit trail) stays in the store via routesearch.Search's own catalog
// write, not duplicated into the task's result_json; with more than one
// candidate pair, only the last pair's plan.RequestID survives into
// CollectRouteResult.RequestID (the catalog holds every pair's own plan
// under its own request id regardless).
func runSearch(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest) (agents.CollectRouteResult, error) {
	radiusKm := req.SearchRadiusKm
	if radiusKm <= 0 {
		radiusKm = defaultSearchRadiusKm // FormSpec always sets 100, but guard against an older/zeroed Spec still on file
	}
	origins, err := resolveAirports(deps.Graph, req.Origin, radiusKm)
	if err != nil {
		return agents.CollectRouteResult{}, fmt.Errorf("dispatch: resolving origin %q: %w", req.Origin, err)
	}
	destinations, err := resolveAirports(deps.Graph, req.Destination, radiusKm)
	if err != nil {
		return agents.CollectRouteResult{}, fmt.Errorf("dispatch: resolving destination %q: %w", req.Destination, err)
	}

	out := agents.CollectRouteResult{}
	var finalResults []routesearch.Result
	var lastErr error
	for _, o := range origins {
		for _, d := range destinations {
			plan, err := routesearch.Search(ctx, deps, routesearch.Params{
				Origin:            o,
				Destination:       d,
				DepartDate:        req.DepartDate,
				MaxHours:          req.MaxHours,
				QueryBudget:       req.QueryBudget,
				MaxPrice:          req.MaxPrice,
				MinLayoverMinutes: req.MinLayoverMinutes,
				MaxLayoverMinutes: req.MaxLayoverMinutes,
				PricePerMile:      0.08,
			})
			if err != nil {
				lastErr = err // one candidate airport failing shouldn't sink every other candidate
				continue
			}
			out.RequestID = plan.RequestID
			out.QueriesUsed += plan.QueriesUsed
			finalResults = routesearch.MergeResults(finalResults, plan.FinalResult)
		}
	}
	if len(finalResults) == 0 && lastErr != nil {
		return agents.CollectRouteResult{}, fmt.Errorf("dispatch: every origin/destination candidate failed, e.g.: %w", lastErr)
	}

	for _, r := range finalResults {
		out.Results = append(out.Results, agents.CollectRouteOffer{
			PriceUSD:        r.PriceUSD,
			DurationMinutes: r.DurationMinutes,
			Path:            r.Path,
			SelfTransfer:    r.SelfTransfer,
		})
	}
	return out, nil
}

// defaultSearchRadiusKm mirrors formSpecSystemPromptTemplate's own
// documented default — kept here too since a Spec predating this field
// (or a bug upstream) could still hand runSearch a zero.
const defaultSearchRadiusKm = 100

// resolveAirports turns one Origin/Destination field into the airport(s)
// to actually search: itself alone, if it's already a real IATA code —
// naming one specific airport pins the search there, no radius fan-out —
// else every airport within radiusKm of that city's center, so a city
// with several airports (or a smaller one nearby) all get tried, not
// just whichever OpenFlights happens to file under that exact city name.
func resolveAirports(graph *openflights.Graph, field string, radiusKm float64) ([]string, error) {
	if _, ok := graph.Airport(field); ok {
		return []string{field}, nil
	}
	lat, lon, ok := graph.CityCenter(field)
	if !ok {
		return nil, fmt.Errorf("not a known airport code or city: %q", field)
	}
	codes := graph.AirportsWithinRadiusKm(lat, lon, radiusKm)
	if len(codes) == 0 {
		return nil, fmt.Errorf("no airports found within %gkm of %q", radiusKm, field)
	}
	return codes, nil
}

package main

import (
	"context"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/routesearch"
)

// fetchFare wraps the existing internal/routesearch.Search (one-way,
// already-built) as the unit runWorker dispatches to. Trims
// routesearch.Plan down to agents.CollectRouteResult — the full Plan
// (candidate-by-candidate audit trail) stays in the store via
// routesearch.Search's own catalog write, not duplicated into the task's
// result_json.
func fetchFare(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest) (agents.CollectRouteResult, error) {
	plan, err := routesearch.Search(ctx, deps, routesearch.Params{
		Origin:            req.Origin,
		Destination:       req.Destination,
		DepartDate:        req.DepartDate,
		MaxHours:          req.MaxHours,
		QueryBudget:       req.QueryBudget,
		MinLayoverMinutes: 45,
		MaxLayoverMinutes: 12 * 60,
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

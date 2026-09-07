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
	"time"

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

// runSearch wraps internal/routesearch.Search (one-way),
// routesearch.SearchRoundTrip (round-trip, picked by req.TripType), or
// routesearch.SearchFlexible (a date window, picked by req.WindowDays)
// as the unit RunTask dispatches to. req.Origin/req.Destination may each be a
// real IATA code or a plain city name (FormSpec fills in a city name
// rather than guessing when a multi-airport city like Beijing or London
// is named but no specific airport is) — resolveAirports expands either
// into the airport(s) to actually search, and a city with N candidates
// fans out into one search call per candidate pair, merged into one
// combined result spanning every airport tried. Trims the routesearch
// plan down to agents.CollectRouteResult — the full plan
// (candidate-by-candidate audit trail) stays in the store via
// routesearch's own catalog write, not duplicated into the task's
// result_json; with more than one candidate pair, only the last pair's
// plan.RequestID survives into CollectRouteResult.RequestID (the catalog
// holds every pair's own plan under its own request id regardless).
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

	if req.WindowDays > 0 {
		return runFlexibleSearch(ctx, deps, req, origins, destinations)
	}
	if req.TripType == "round_trip" && req.ReturnDate != "" {
		return runRoundTripSearch(ctx, deps, req, origins, destinations)
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

// runRoundTripSearch is runSearch's TripType == "round_trip" branch:
// routesearch.SearchRoundTrip per origin/destination candidate pair
// (bundled Google fare vs. two separately-priced one-ways, whichever's
// cheaper — see DESIGN.md "Round trips and flexible dates"), keeping
// only the single cheapest pair's result. Unlike the one-way path, this
// isn't pareto-merged across candidates: SearchRoundTrip already reduces
// each pair down to one best RoundTripResult, so "cheapest across pairs"
// is the natural way to pick among them too.
func runRoundTripSearch(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest, origins, destinations []string) (agents.CollectRouteResult, error) {
	out := agents.CollectRouteResult{}
	var best *routesearch.RoundTripResult
	var lastErr error
	for _, o := range origins {
		for _, d := range destinations {
			plan, err := routesearch.SearchRoundTrip(ctx, deps, routesearch.Params{
				Origin:            o,
				Destination:       d,
				DepartDate:        req.DepartDate,
				MaxHours:          req.MaxHours,
				QueryBudget:       req.QueryBudget,
				MaxPrice:          req.MaxPrice,
				MinLayoverMinutes: req.MinLayoverMinutes,
				MaxLayoverMinutes: req.MaxLayoverMinutes,
				PricePerMile:      0.08,
			}, req.ReturnDate)
			if err != nil {
				lastErr = err // one candidate airport pair failing shouldn't sink every other candidate
				continue
			}
			out.RequestID = plan.RequestID
			out.QueriesUsed += plan.QueriesUsed
			if plan.Result != nil && (best == nil || plan.Result.TotalPriceUSD < best.TotalPriceUSD) {
				best = plan.Result
			}
		}
	}
	if best == nil && lastErr != nil {
		return agents.CollectRouteResult{}, fmt.Errorf("dispatch: every origin/destination candidate failed, e.g.: %w", lastErr)
	}
	if best != nil {
		out.Results = []agents.CollectRouteOffer{{
			PriceUSD:              best.OutboundPriceUSD,
			DurationMinutes:       best.OutboundDurationMinutes,
			Path:                  best.OutboundPath,
			SelfTransfer:          best.OutboundSelfTransfer,
			Bundled:               best.Bundled,
			TotalPriceUSD:         best.TotalPriceUSD,
			ReturnPath:            best.ReturnPath,
			ReturnPriceUSD:        best.ReturnPriceUSD,
			ReturnDurationMinutes: best.ReturnDurationMinutes,
			ReturnSelfTransfer:    best.ReturnSelfTransfer,
		}}
	}
	return out, nil
}

// runFlexibleSearch is runSearch's WindowDays > 0 branch:
// routesearch.SearchFlexible per origin/destination candidate pair — a
// cheap per-date baseline scan across [DepartDate-WindowDays,
// DepartDate+WindowDays] (Phase A), then the full hub search on
// whichever date won (Phase B). Handles either TripType:
// SearchFlexible itself branches on FlexibleParams.RoundTrip, running
// Search or SearchRoundTrip for Phase B. Like round-trip, this isn't
// pareto-merged across candidate airports — each pair reduces to one
// chosen date and one best result, so "cheapest across pairs" is the
// natural way to compare them.
func runFlexibleSearch(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest, origins, destinations []string) (agents.CollectRouteResult, error) {
	roundTrip := req.TripType == "round_trip" && req.ReturnDate != ""
	var tripLengthDays int
	if roundTrip {
		if depart, err := time.Parse("2006-01-02", req.DepartDate); err == nil {
			if ret, err := time.Parse("2006-01-02", req.ReturnDate); err == nil {
				tripLengthDays = int(ret.Sub(depart).Hours() / 24)
			}
		}
	}

	out := agents.CollectRouteResult{}
	var best *routesearch.FlexiblePlan
	var bestPrice float64
	var lastErr error
	for _, o := range origins {
		for _, d := range destinations {
			plan, err := routesearch.SearchFlexible(ctx, deps, routesearch.FlexibleParams{
				Base: routesearch.Params{
					Origin: o, Destination: d, DepartDate: req.DepartDate,
					MaxHours:          req.MaxHours,
					QueryBudget:       req.QueryBudget,
					MaxPrice:          req.MaxPrice,
					MinLayoverMinutes: req.MinLayoverMinutes,
					MaxLayoverMinutes: req.MaxLayoverMinutes,
					PricePerMile:      0.08,
				},
				RoundTrip:      roundTrip,
				TripLengthDays: tripLengthDays,
				WindowDays:     req.WindowDays,
				StepDays:       req.StepDays,
			})
			if err != nil {
				lastErr = err // one candidate airport pair failing shouldn't sink every other candidate
				continue
			}
			out.RequestID = plan.RequestID
			for _, e := range plan.DateScan {
				if e.Queried {
					out.QueriesUsed++
				}
			}
			price, ok := flexiblePlanPrice(plan)
			if !ok {
				continue
			}
			if best == nil || price < bestPrice {
				best, bestPrice = plan, price
			}
		}
	}
	if best == nil && lastErr != nil {
		return agents.CollectRouteResult{}, fmt.Errorf("dispatch: every origin/destination candidate failed, e.g.: %w", lastErr)
	}
	if best != nil {
		out.ChosenDepartDate = best.ChosenDepartDate
		out.ChosenReturnDate = best.ChosenReturnDate
		switch {
		case best.RoundTripResult != nil:
			r := best.RoundTripResult
			out.Results = []agents.CollectRouteOffer{{
				PriceUSD:              r.OutboundPriceUSD,
				DurationMinutes:       r.OutboundDurationMinutes,
				Path:                  r.OutboundPath,
				SelfTransfer:          r.OutboundSelfTransfer,
				Bundled:               r.Bundled,
				TotalPriceUSD:         r.TotalPriceUSD,
				ReturnPath:            r.ReturnPath,
				ReturnPriceUSD:        r.ReturnPriceUSD,
				ReturnDurationMinutes: r.ReturnDurationMinutes,
				ReturnSelfTransfer:    r.ReturnSelfTransfer,
			}}
		case best.OneWayResult != nil:
			r := best.OneWayResult
			out.Results = []agents.CollectRouteOffer{{
				PriceUSD:        r.PriceUSD,
				DurationMinutes: r.DurationMinutes,
				Path:            r.Path,
				SelfTransfer:    r.SelfTransfer,
			}}
		}
	}
	return out, nil
}

// flexiblePlanPrice is the figure to compare candidate airport pairs
// by — TotalPriceUSD for a round trip, PriceUSD for one-way — and false
// if the plan never reached a feasible Phase B result at all.
func flexiblePlanPrice(plan *routesearch.FlexiblePlan) (float64, bool) {
	switch {
	case plan.RoundTripResult != nil:
		return plan.RoundTripResult.TotalPriceUSD, true
	case plan.OneWayResult != nil:
		return plan.OneWayResult.PriceUSD, true
	default:
		return 0, false
	}
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

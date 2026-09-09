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
	"sort"

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

// runSearch wraps internal/routesearch.Search (one-way, exact date),
// routesearch.SearchRoundTrip (round-trip, exact dates), or
// routesearch.SearchDateRange (either TripType, picked whenever a
// date's From differs from its To — a genuine range to search, not a
// single date) as the unit RunTask dispatches to. req.Origin/
// req.Destination may each be a real IATA code or a plain city name
// (FormSpec fills in a city name rather than guessing when a multi-
// airport city like Beijing or London is named but no specific airport
// is) — resolveAirports expands either into the airport(s) to actually
// search, and a city with N candidates fans out into one search call
// per candidate pair, merged into one combined result spanning every
// airport tried. Trims the routesearch plan down to
// agents.CollectRouteResult — the full plan (candidate-by-candidate
// audit trail) stays in the store via routesearch's own catalog write,
// not duplicated into the task's result_json; with more than one
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

	// roundTrip: TripType says round_trip AND a return is actually
	// resolved one of two mutually-exclusive ways — an independent
	// MinReturnDate, or a MinTripLengthDays/MaxTripLengthDays tolerance
	// coupled to the depart window (see agents.Spec.MinTripLengthDays'
	// doc) — never neither, missingRequiredFields already guarantees one
	// of the two is set before dispatch is reached.
	roundTrip := req.TripType == "round_trip" && (req.MinReturnDate != "" || req.MinTripLengthDays != 0 || req.MaxTripLengthDays != 0)
	// tripLengthFlex: the coupled-length shape specifically — routed to
	// routesearch.SearchFlexible (N depart-window queries x trip-length
	// tolerance), not the independent-ranges grid below (N x M queries),
	// since the whole point of offering this shape is the cheaper query
	// count when the trip length is actually fixed-ish rather than a
	// genuinely independent return window.
	tripLengthFlex := roundTrip && req.MinReturnDate == ""
	// independentRangeFlex: a genuine range on either end, priced via
	// routesearch.SearchDateRange's full depart x return grid.
	independentRangeFlex := !tripLengthFlex && (req.MinDepartDate != req.MaxDepartDate || (roundTrip && req.MinReturnDate != req.MaxReturnDate))

	if tripLengthFlex {
		return runFlexibleTripLengthSearch(ctx, deps, req, origins, destinations)
	}
	if independentRangeFlex {
		return runDateRangeSearch(ctx, deps, req, origins, destinations, roundTrip)
	}
	if roundTrip {
		return runRoundTripSearch(ctx, deps, req, origins, destinations)
	}

	out := agents.CollectRouteResult{}
	var finalResults []routesearch.Result
	var lastErr error
	for _, o := range origins {
		for _, d := range destinations {
			params := baseParams(req, o, d)
			params.DepartDate = req.MinDepartDate
			plan, err := routesearch.Search(ctx, deps, params)
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
			params := baseParams(req, o, d)
			params.DepartDate = req.MinDepartDate
			plan, err := routesearch.SearchRoundTrip(ctx, deps, params, req.MinReturnDate)
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

// runDateRangeSearch is runSearch's flexible branch (a genuine range on
// either end): routesearch.SearchDateRange per origin/destination
// candidate pair — prices every date/combination in the range(s) (Phase
// A), then the full hub search on whichever won (Phase B). Handles
// either TripType. Like round-trip, this isn't pareto-merged across
// candidate airports — each pair reduces to one chosen date and one
// best result, so "cheapest across pairs" is the natural way to compare
// them.
func runDateRangeSearch(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest, origins, destinations []string, roundTrip bool) (agents.CollectRouteResult, error) {
	return runFlexibleAcrossCandidates(origins, destinations, func(o, d string) (*routesearch.FlexiblePlan, error) {
		return routesearch.SearchDateRange(ctx, deps, routesearch.DateRangeParams{
			Base:           baseParams(req, o, d),
			RoundTrip:      roundTrip,
			DepartFrom:     req.MinDepartDate,
			DepartTo:       req.MaxDepartDate,
			ReturnFrom:     req.MinReturnDate,
			ReturnTo:       req.MaxReturnDate,
			StepDays:       req.StepDays,
			AvailableFrom:  req.MinRoundTripDate,
			AvailableUntil: req.MaxRoundTripDate,
			BlackoutDates:  req.BlackoutDates,
		})
	})
}

// runFlexibleTripLengthSearch is runSearch's tripLengthFlex branch: the
// coupled depart-window x trip-length-tolerance shape
// (routesearch.SearchFlexible), an alternative to runDateRangeSearch's
// independent depart x return grid whenever the request actually has a
// trip length in mind rather than a genuinely separate return window
// (see agents.Spec.MinTripLengthDays' doc for why that's cheaper: N
// queries instead of N x M).
func runFlexibleTripLengthSearch(ctx context.Context, deps routesearch.Deps, req agents.CollectRouteRequest, origins, destinations []string) (agents.CollectRouteResult, error) {
	return runFlexibleAcrossCandidates(origins, destinations, func(o, d string) (*routesearch.FlexiblePlan, error) {
		return routesearch.SearchFlexible(ctx, deps, routesearch.FlexibleParams{
			Base:               baseParams(req, o, d),
			RoundTrip:          true,
			DepartFrom:         req.MinDepartDate,
			DepartTo:           req.MaxDepartDate,
			StepDays:           req.StepDays,
			TripLengthDays:     req.MinTripLengthDays,
			TripLengthMaxDays:  req.MaxTripLengthDays,
			TripLengthStepDays: req.TripLengthStepDays,
			AvailableFrom:      req.MinRoundTripDate,
			AvailableUntil:     req.MaxRoundTripDate,
			BlackoutDates:      req.BlackoutDates,
		})
	})
}

// runFlexibleAcrossCandidates is runDateRangeSearch and
// runFlexibleTripLengthSearch's shared "try every origin/destination
// candidate pair, keep the cheapest FlexiblePlan" loop, factored out
// since both reduce each pair to one chosen date (or date + trip length)
// and one best result — "cheapest across pairs" is the natural way to
// compare them, same as round-trip's own single-pair reduction. search
// is the one thing that differs between the two callers: which
// routesearch entry point (SearchDateRange vs. SearchFlexible) actually
// runs for a given (origin, destination) pair.
func runFlexibleAcrossCandidates(origins, destinations []string, search func(origin, destination string) (*routesearch.FlexiblePlan, error)) (agents.CollectRouteResult, error) {
	out := agents.CollectRouteResult{}
	var best *routesearch.FlexiblePlan
	var bestPrice float64
	var lastErr error
	for _, o := range origins {
		for _, d := range destinations {
			plan, err := search(o, d)
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

// baseParams is the routesearch.Params common to every dispatch shape
// (exact one-way, exact round trip, date-range grid, trip-length flex)
// for one origin/destination candidate pair — DepartDate is set by
// whichever caller needs it (the flexible shapes leave it blank; their
// own DepartFrom/DepartTo carries the window instead).
func baseParams(req agents.CollectRouteRequest, origin, destination string) routesearch.Params {
	return routesearch.Params{
		Origin:            origin,
		Destination:       destination,
		MaxHours:          req.MaxHours,
		QueryBudget:       req.QueryBudget,
		MaxPrice:          req.MaxPrice,
		MinLayoverMinutes: req.MinLayoverMinutes,
		MaxLayoverMinutes: req.MaxLayoverMinutes,
		CheckedBags:       req.CheckedBags,
		MaxCountries:      req.MaxCountries,
		ExcludedCountries: req.ExcludedCountries,
		PricePerMile:      0.08,
	}
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
	codes = wellConnected(graph, codes)
	if len(codes) == 0 {
		return nil, fmt.Errorf("no well-connected airport found within %gkm of %q", radiusKm, field)
	}
	return codes, nil
}

// minCityAirportOutDegree/maxCityCandidates bound a city name's radius
// fan-out. AirportsWithinRadiusKm returns every airport OpenFlights
// knows about within range, unfiltered by real-world relevance — for a
// city like Vancouver that's a dozen-plus tiny regional strips alongside
// the one real international gateway. Searching every one of them
// multiplies query volume by however many candidates a multi-airport
// city resolves to on *each* end (a live run: 17 Vancouver-radius
// candidates x 3 Beijing candidates = 51 full searches for one request)
// — slow, and a real risk of tripping Google's own rate limiting on the
// query that actually matters (a live run's real bundled round-trip
// fare came back worse than a hand-checked one, plausibly because of
// exactly this: dozens of near-simultaneous scrapes against the same
// route/date). A real airport's own route count (out-degree) is a cheap,
// real signal for "actually useful as a candidate" that pure distance
// isn't — same reasoning as nhop.go's minHubOutDegree.
const (
	minCityAirportOutDegree = 5
	maxCityCandidates       = 5
)

func wellConnected(graph *openflights.Graph, codes []string) []string {
	type scored struct {
		code   string
		degree int
	}
	var kept []scored
	for _, c := range codes {
		if degree := len(graph.Routes[c]); degree >= minCityAirportOutDegree {
			kept = append(kept, scored{c, degree})
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].degree > kept[j].degree })
	if len(kept) > maxCityCandidates {
		kept = kept[:maxCityCandidates]
	}
	out := make([]string, len(kept))
	for i, k := range kept {
		out[i] = k.code
	}
	return out
}

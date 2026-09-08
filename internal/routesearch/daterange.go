package routesearch

// DateRangeParams/SearchDateRange is DESIGN.md's originally-deferred
// "combined [depart x return] version" of flexible dates ("Hub search
// runs per-direction... the combined version... multiplies candidate
// count for a savings case that's already the thinner one... Deferred,
// not designed away"), now built: depart and return each get their own
// independent range, and the search prices every valid combination
// (Phase A) before running the full hub search on the cheapest eligible
// one (Phase B) — the same two-phase shape SearchFlexible already uses,
// just enumerated over two dimensions instead of one.
//
// This is a real, accepted cost tradeoff, not an oversight: pricing an
// N-day x M-day grid is N*M queries, not N+M. Prefer
// FlexibleParams.TripLengthDays (a window of *start* dates with a fixed
// trip length coupled to it) whenever the trip length is actually
// fixed — same "flexible dates" goal, but N queries instead of N*M,
// because depart and return move together there rather than
// independently.

import (
	"context"
	"fmt"
	"time"
)

// DateRangeParams is an independent-ranges date search.
type DateRangeParams struct {
	Base       Params // Origin/Destination/MaxHours/etc — DepartDate/ReturnDate ignored, the ranges below are authoritative
	RoundTrip  bool
	DepartFrom string // YYYY-MM-DD, inclusive
	DepartTo   string
	ReturnFrom string // only used if RoundTrip; ignored for one-way
	ReturnTo   string
	StepDays   int // sample every StepDays within each range; 0 defaults to 1 (daily)

	// AvailableFrom/AvailableUntil: an outer eligibility bound both
	// depart and return must fall within (e.g. "I only have a month of
	// paid leave") — narrower than DepartFrom/To and ReturnFrom/To,
	// which only control what gets *priced*. Every combination in the
	// grid is still scanned; this only narrows which of the priced
	// combinations may be picked as the winner. Same semantics as
	// FlexibleParams' own eligibility fields (see eligibility, above).
	AvailableFrom, AvailableUntil string
}

// SearchDateRange runs the grid: Phase A prices every (depart, return)
// pair — or just every depart date, for one-way — within the given
// ranges (cheap, baseline-only, no hub search), stopping early if
// Base.QueryBudget is exhausted (an anytime algorithm, same principle
// as Search's own query budget: a truncated grid still returns the best
// of what it priced, rather than refusing to answer). Phase B then runs
// the full hub search on the cheapest eligible pair found.
func SearchDateRange(ctx context.Context, deps Deps, p DateRangeParams) (*FlexiblePlan, error) {
	requestID := fmt.Sprintf("GRID-%s-%s-%d", p.Base.Origin, p.Base.Destination, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &FlexiblePlan{RequestID: requestID, Status: "running"}
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))

	step := p.StepDays
	if step < 1 {
		step = 1
	}
	departDates, err := dateRange(p.DepartFrom, p.DepartTo, step)
	if err != nil {
		plan.Status = fmt.Sprintf("error: %v", err)
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, err
	}

	elig := eligibility{AvailableFrom: p.AvailableFrom, AvailableUntil: p.AvailableUntil}
	queriesUsed := 0
	budgetExhausted := false

	if !p.RoundTrip {
		log.Info("phase A: date-range scan (one-way)", "departures", len(departDates))
		for _, d := range departDates {
			if queriesUsed >= p.Base.QueryBudget {
				budgetExhausted = true
				break
			}
			entry, live := scanPair(ctx, deps, p.Base, d, "")
			if live {
				queriesUsed++
				sleepPacing(ctx, p.Base.Delay)
			}
			entry.Excluded = exclusionReason(elig, entry)
			plan.DateScan = append(plan.DateScan, entry)
		}
	} else {
		returnDates, err := dateRange(p.ReturnFrom, p.ReturnTo, step)
		if err != nil {
			plan.Status = fmt.Sprintf("error: %v", err)
			_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
			return plan, err
		}
		log.Info("phase A: date-range grid scan (round trip)", "departures", len(departDates), "returns", len(returnDates), "combinations", len(departDates)*len(returnDates))
	outer:
		for _, d := range departDates {
			for _, r := range returnDates {
				if r <= d {
					continue // a return on or before its own depart is never valid, not even worth a query
				}
				if queriesUsed >= p.Base.QueryBudget {
					budgetExhausted = true
					break outer
				}
				entry, live := scanPair(ctx, deps, p.Base, d, r)
				if live {
					queriesUsed++
					sleepPacing(ctx, p.Base.Delay)
				}
				entry.Excluded = exclusionReason(elig, entry)
				plan.DateScan = append(plan.DateScan, entry)
			}
		}
	}
	if budgetExhausted {
		log.Warn("date-range grid truncated: query budget exhausted before every combination was priced", "priced", len(plan.DateScan), "query_budget", p.Base.QueryBudget)
	}

	best := cheapestDateScanEntry(plan.DateScan)
	if best == nil {
		reason := "no feasible date combination in range"
		if anyPricedButExcluded(plan.DateScan) {
			reason = "every priced combination is excluded by AvailableFrom/AvailableUntil"
		}
		plan.Status = "error: " + reason
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}
	plan.ChosenDepartDate = best.DepartDate
	plan.ChosenReturnDate = best.ReturnDate
	log.Info("phase A winner", "depart", best.DepartDate, "return", best.ReturnDate, "price_usd", best.PriceUSD)

	log.Info("phase B: full hub search on the winning date(s)")
	anchored := p.Base
	anchored.DepartDate = best.DepartDate
	if p.RoundTrip {
		rtPlan, err := SearchRoundTrip(ctx, deps, anchored, best.ReturnDate)
		if err != nil {
			plan.Status = fmt.Sprintf("error: phase B: %v", err)
			_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
			return plan, err
		}
		plan.AnchoredPlanID = rtPlan.RequestID
		plan.RoundTripResult = rtPlan.Result
	} else {
		owPlan, err := Search(ctx, deps, anchored)
		if err != nil {
			plan.Status = fmt.Sprintf("error: phase B: %v", err)
			_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
			return plan, err
		}
		plan.AnchoredPlanID = owPlan.RequestID
		if len(owPlan.FinalResult) > 0 {
			plan.OneWayResult = cheapestResult(owPlan.FinalResult)
		}
	}

	plan.Status = "done"
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
	log.Info("date-range search done", "queries_used", queriesUsed)
	return plan, nil
}

// dateRange enumerates YYYY-MM-DD dates from from to to inclusive,
// stepping by step days.
func dateRange(from, to string, step int) ([]string, error) {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("routesearch: invalid range start %q: %w", from, err)
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("routesearch: invalid range end %q: %w", to, err)
	}
	if end.Before(start) {
		return nil, fmt.Errorf("routesearch: range end %s is before start %s", to, from)
	}
	var out []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, step) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out, nil
}

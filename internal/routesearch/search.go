package routesearch

import (
	"context"
	"fmt"
	"time"

	"flight-search-intelligence/internal/googleflights"
)

// Search runs the algorithm end to end: baseline direct search, hub
// candidate generation + geometry prune, then a best-first,
// budget-bounded loop over the survivors. Always returns a Plan (with
// Status set) even on a request-level error, since a partial audit
// trail is still worth keeping.
//
// MaxLegs > 1 hands off to searchNHop (nhop.go) instead — the label-
// setting generalization to arbitrary depth (DESIGN.md "Deeper
// itineraries"). Everything below this check is the original 1-stop
// loop, untouched, so MaxLegs 0/1 (today's default) behaves exactly as
// it always has.
func Search(ctx context.Context, deps Deps, p Params) (*Plan, error) {
	if p.MaxLegs > 1 {
		return searchNHop(ctx, deps, p)
	}
	requestID := fmt.Sprintf("%s-%s-%s-%d", p.Origin, p.Destination, p.DepartDate, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &Plan{RequestID: requestID, Input: p, Status: "running"}

	savePlan(ctx, deps, requestID, plan.Status, plan)

	_, ok1 := deps.Graph.Airport(p.Origin)
	_, ok2 := deps.Graph.Airport(p.Destination)
	if !ok1 || !ok2 {
		plan.Status = fmt.Sprintf("error: unknown airport (origin ok=%v, destination ok=%v)", ok1, ok2)
		savePlan(ctx, deps, requestID, plan.Status, plan)
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}

	queriesUsed := 0
	var best *Result

	// Step 0: baseline. Google's own search already finds the best
	// single-ticket itinerary; every split-ticket candidate below only
	// matters if it can beat this.
	log.Info("querying baseline direct route")
	baseOffers, live, err := deps.searchOffers(ctx, googleflights.SearchParams{
		Origin: p.Origin, Destination: p.Destination, DepartureDate: p.DepartDate,
		MaxPrice: maxPricePtr(p.MaxPrice), CheckedBags: checkedBagsPtr(p.CheckedBags),
	}, p.ForceRefresh)
	if live {
		queriesUsed++
	}
	if err != nil {
		log.Warn("baseline search failed", "error", err)
	}
	baseline := CandidateOutcome{Hub: "(direct)", Rank: 0}
	if offer, dur, ok := pickCheapestFeasible(baseOffers, deps.Graph, p.MaxHours, float64(p.MaxPrice)); ok {
		r := Result{Path: []string{p.Origin, p.Destination}, PriceUSD: float64(offer.Price), DurationMinutes: int(dur.Minutes())}
		best = &r
		plan.FinalResult = append(plan.FinalResult, r)
		baseline.Leg1 = &LegOutcome{Queried: true, PriceUSD: r.PriceUSD, QueriedAt: time.Now()}
		baseline.LBUSD = r.PriceUSD
		baseline.CombinedUSD = r.PriceUSD
		baseline.Outcome = "kept"
		log.Info("baseline found", "price_usd", r.PriceUSD, "duration_minutes", r.DurationMinutes)
	} else {
		baseline.Leg1 = &LegOutcome{Queried: true, QueriedAt: time.Now(), Reason: "no feasible offer"}
		baseline.Outcome = "leg1_infeasible"
	}
	// Always rank[0], ahead of every hub candidate below — it's what
	// every candidate is measured against, not just another option.
	plan.CandidatesRanked = append(plan.CandidatesRanked, baseline)
	if live {
		sleepPacing(ctx, p.Delay)
	}

	// Step 1: candidate hubs, geometry-pruned (pure arithmetic, no
	// scrapes) and price-ranked (cache-first, else distance × $/mile).
	// See ResolveCandidates — same logic, exposed standalone for
	// dry-run inspection (cmd/routesearch -dry-run).
	preview, err := ResolveCandidates(ctx, deps, p)
	if err != nil {
		plan.Status = fmt.Sprintf("error: %v", err)
		savePlan(ctx, deps, requestID, plan.Status, plan)
		return plan, err
	}
	plan.CandidatesConsidered = preview.CandidatesConsidered
	plan.CandidatesAfterGeometryPrune = preview.CandidatesAfterGeometryPrune
	survivors := preview.RankedHubs

	log.Info("candidate hubs ready",
		"considered", plan.CandidatesConsidered, "after_geometry_prune", plan.CandidatesAfterGeometryPrune)

	// Step 2: best-first loop, exhaustive by default (see DESIGN.md
	// "Exploration algorithm" for the full derivation). QueryBudget <= 0
	// (the default) means no artificial cap — the loop runs every
	// surviving candidate until the (*) frontier-cutoff below proves
	// nothing left can beat `best`, which is what makes the result
	// actually the best option, not just a good-enough one found fast.
	// A positive QueryBudget trades that guarantee for a bounded run.
	for i, c := range survivors {
		if best != nil && c.LBUSD >= best.PriceUSD {
			markRemaining(plan, survivors[i:], i+1, "frontier_cutoff", "LB >= best.price")
			break
		}
		if !withinBudget(queriesUsed, p.QueryBudget) {
			markRemaining(plan, survivors[i:], i+1, "budget_exhausted", "query budget exhausted")
			break
		}

		outcome := CandidateOutcome{Hub: c.Hub, LBUSD: c.LBUSD, Rank: i + 1}
		log.Info("querying leg 1", "hub", c.Hub, "lb_usd", c.LBUSD)

		leg1Offers, live, err := deps.searchOffers(ctx, googleflights.SearchParams{
			Origin: p.Origin, Destination: c.Hub, DepartureDate: p.DepartDate,
			MaxStops: googleflights.NonstopOnly(), CheckedBags: checkedBagsPtr(p.CheckedBags),
		}, p.ForceRefresh)
		if live {
			queriesUsed++
			sleepPacing(ctx, p.Delay)
		}

		leg1, _, ok := pickCheapestFeasible(leg1Offers, deps.Graph, p.MaxHours, 0) // leg-only price, not the whole trip — bestConnection enforces p.MaxPrice on the combined total
		if err != nil || !ok {
			outcome.Leg1 = &LegOutcome{Queried: true, QueriedAt: time.Now(), Reason: "no feasible offer"}
			outcome.Outcome = "leg1_infeasible"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			log.Info("leg 1 infeasible", "hub", c.Hub)
			continue
		}
		outcome.Leg1 = &LegOutcome{Queried: true, PriceUSD: float64(leg1.Price), QueriedAt: time.Now()}

		hEst := deps.lowerBoundUSD(ctx, c.Hub, p.Destination, p.DepartDate, c.Leg2Miles, p.PricePerMile)
		if best != nil && float64(leg1.Price)+hEst >= best.PriceUSD {
			outcome.Outcome = "pruned"
			outcome.Reason = "g1 + hEst >= best.price"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			log.Info("leg 2 skipped", "hub", c.Hub, "reason", outcome.Reason)
			continue
		}

		if !withinBudget(queriesUsed, p.QueryBudget) {
			outcome.Outcome = "pruned"
			outcome.Reason = "query budget exhausted before leg 2"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			break
		}

		// Query leg 2 for the date leg 1 actually lands on — handles an
		// overnight leg 1 landing the day after DepartDate.
		leg2Date := dateString(leg1.Segments[len(leg1.Segments)-1].ArrivalDate)
		log.Info("querying leg 2", "hub", c.Hub, "date", leg2Date)
		leg2Offers, live, err := deps.searchOffers(ctx, googleflights.SearchParams{
			Origin: c.Hub, Destination: p.Destination, DepartureDate: leg2Date,
			MaxStops: googleflights.NonstopOnly(), CheckedBags: checkedBagsPtr(p.CheckedBags),
		}, p.ForceRefresh)
		if live {
			sleepPacing(ctx, p.Delay)
			queriesUsed++
		}

		leg2, combined, layoverDur, feasible := deps.bestConnection(leg1, leg2Offers, p)
		if err != nil || !feasible {
			outcome.Leg2 = &LegOutcome{Queried: true, QueriedAt: time.Now(), Reason: "no feasible connection"}
			outcome.Outcome = "leg2_infeasible"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			log.Info("leg 2 infeasible", "hub", c.Hub)
			continue
		}

		total := leg1.Price + leg2.Price
		outcome.Leg2 = &LegOutcome{Queried: true, PriceUSD: float64(leg2.Price), QueriedAt: time.Now()}
		outcome.CombinedUSD = float64(total)
		outcome.Outcome = "kept"
		plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)

		r := Result{
			Path:            []string{p.Origin, c.Hub, p.Destination},
			PriceUSD:        float64(total),
			DurationMinutes: int(combined.Minutes()),
			SelfTransfer:    true,
			LayoverMinutes:  int(layoverDur.Minutes()),
			Stopover:        layoverDur > stopoverThreshold,
		}
		plan.FinalResult = paretoInsert(plan.FinalResult, r)
		if best == nil || r.PriceUSD < best.PriceUSD {
			best = &r
		}
		log.Info("candidate kept", "hub", c.Hub, "combined_usd", r.PriceUSD, "duration_minutes", r.DurationMinutes)
	}

	plan.QueriesUsed = queriesUsed
	plan.Status = "done"
	savePlan(ctx, deps, requestID, plan.Status, plan)
	log.Info("search done", "queries_used", queriesUsed, "results", len(plan.FinalResult))
	return plan, nil
}

// markRemaining records the candidates a break in the main loop left
// unqueried, so the audit trail accounts for every candidate that was
// ranked, not just the ones actually scraped.
// startRank is rest[0]'s 1-based rank among hub candidates — passed
// explicitly rather than inferred from len(plan.CandidatesRanked), since
// that slice also holds the rank-0 baseline entry and would otherwise
// shift every hub rank off by one.
func markRemaining(plan *Plan, rest []RankedHub, startRank int, outcome, reason string) {
	for j, c := range rest {
		plan.CandidatesRanked = append(plan.CandidatesRanked, CandidateOutcome{
			Hub: c.Hub, LBUSD: c.LBUSD, Rank: startRank + j,
			Outcome: outcome, Reason: reason,
		})
	}
}

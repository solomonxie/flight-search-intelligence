package routesearch

// searchNHop generalizes Search's fixed A->hub->B loop to arbitrary
// depth — DESIGN.md "Deeper itineraries: N-hop search and hop-country
// constraints", building on "Generalizing beyond 1-stop"'s label-setting
// model. Search calls this when Params.MaxLegs > 1; its own 1-stop
// path (the exact loop this generalizes) is left untouched, so today's
// default behavior is provably unaffected by this file existing.
//
// Same class of algorithm as the 1-stop case — best-first
// branch-and-bound with lazy, budgeted edge evaluation — generalized
// from "one candidate hub" to a label (node, price_so_far,
// duration_so_far, legs_so_far), a state discarded the moment another
// label at the same node dominates it (standard multi-criteria
// label-setting: Dijkstra's relaxation extended from one scalar cost to
// a Pareto pair). QUERY_BUDGET stays flat (a deliberate choice, not an
// oversight — see IMPLEMENTATION_PLAN.md's Phase 3) and is what keeps a
// high MaxLegs from exploring exhaustively, the same role it already
// plays at 1 hop.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
)

// minHubOutDegree is candidateEdges' hub-connectivity prune threshold —
// a candidate with fewer direct routes than this, and no direct route
// to Destination either, is treated as a dead end not worth a query.
// Same spirit as candidates.go's avgCruiseMPH/perLegOverheadMinute — a
// deliberately simple, hardcoded constant, not a tunable Params field.
const minHubOutDegree = 10

// nhopLabel is one partial itinerary: the airports visited so far, the
// real (scraped) price and elapsed duration to reach the last one, and
// the actual offer that got there — needed to compute the *next* hop's
// layover the same way bestConnection does for the 1-stop case.
type nhopLabel struct {
	Path            []string
	PriceUSD        float64
	DurationMinutes int
	LastOffer       googleflights.Offer // zero value at Path==[Origin]: no incoming leg yet
}

func (l nhopLabel) node() string { return l.Path[len(l.Path)-1] }
func (l nhopLabel) legs() int    { return len(l.Path) - 1 }

// nhopEdge is one not-yet-scraped candidate transition the frontier
// ranks: "from this partial itinerary, try this next airport." LB is
// the same admissible (never-overestimating) bound the 1-stop case
// ranks candidate hubs by, generalized: price already spent, plus an
// estimate for this leg, plus an estimate for however many more hops it
// takes to reach Destination from there.
type nhopEdge struct {
	From nhopLabel
	To   string
	LB   float64
}

// searchNHop mirrors Search's shape closely on purpose (same baseline
// step, same audit-trail-then-return structure) so the two are easy to
// compare side by side; see Search's own doc comment for what stays
// identical.
func searchNHop(ctx context.Context, deps Deps, p Params) (*Plan, error) {
	requestID := fmt.Sprintf("%s-%s-%s-%d", p.Origin, p.Destination, p.DepartDate, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &Plan{RequestID: requestID, Input: p, Status: "running"}
	if err := deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan)); err != nil {
		log.Warn("saving initial plan failed", "error", err)
	}

	if _, ok := deps.Graph.Airport(p.Origin); !ok {
		plan.Status = "error: unknown origin airport"
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}
	destination, ok := deps.Graph.Airport(p.Destination)
	if !ok {
		plan.Status = "error: unknown destination airport"
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}

	queriesUsed := 0
	var best *Result

	// Step 0: baseline, identical in spirit to Search's — Google's own
	// best full itinerary is still the floor every N-hop combo has to
	// beat, self-transfer risk and extra stops considered.
	log.Info("nhop: querying baseline direct route")
	baseOffers, live, err := deps.searchOffers(ctx, googleflights.SearchParams{
		Origin: p.Origin, Destination: p.Destination, DepartureDate: p.DepartDate, MaxPrice: maxPricePtr(p.MaxPrice),
	}, p.ForceRefresh)
	if live {
		queriesUsed++
	}
	if err != nil {
		log.Warn("nhop: baseline search failed", "error", err)
	}
	if offer, dur, ok := pickCheapestFeasible(baseOffers, deps.Graph, p.MaxHours, float64(p.MaxPrice)); ok {
		r := Result{Path: []string{p.Origin, p.Destination}, PriceUSD: float64(offer.Price), DurationMinutes: int(dur.Minutes())}
		best = &r
		plan.FinalResult = append(plan.FinalResult, r)
		log.Info("nhop: baseline found", "price_usd", r.PriceUSD, "duration_minutes", r.DurationMinutes)
	}
	if live {
		sleepPacing(ctx, p.Delay)
	}

	settled := map[string][]nhopLabel{} // per-node non-dominated labels seen so far — Pareto pruning
	origin := nhopLabel{Path: []string{p.Origin}}
	settled[p.Origin] = []nhopLabel{origin}
	frontier := candidateEdges(ctx, deps, p, origin, destination)

	for len(frontier) > 0 && queriesUsed < p.QueryBudget {
		sort.Slice(frontier, func(i, j int) bool { return frontier[i].LB < frontier[j].LB })
		edge := frontier[0]

		if best != nil && edge.LB >= best.PriceUSD {
			// Same argument as Search's own (*) break: frontier is
			// sorted ascending by an admissible bound, so nothing left
			// in it can beat best either.
			markEdgesCutoff(plan, frontier, "frontier_cutoff", "LB >= best.price")
			break
		}
		frontier = frontier[1:]

		queryDate := p.DepartDate
		if !isOrigin(edge.From) {
			queryDate = dateString(edge.From.LastOffer.Segments[len(edge.From.LastOffer.Segments)-1].ArrivalDate)
		}
		log.Info("nhop: querying edge", "from", edge.From.node(), "to", edge.To, "lb_usd", edge.LB, "legs_so_far", edge.From.legs())
		offers, liveQ, err := deps.searchOffers(ctx, googleflights.SearchParams{
			Origin: edge.From.node(), Destination: edge.To, DepartureDate: queryDate,
			MaxStops: googleflights.NonstopOnly(),
		}, p.ForceRefresh)
		if liveQ {
			queriesUsed++
			sleepPacing(ctx, p.Delay)
		}
		if err != nil {
			plan.NHopRanked = append(plan.NHopRanked, NHopOutcome{FromPath: edge.From.Path, To: edge.To, LBUSD: edge.LB, Outcome: "infeasible", Reason: err.Error()})
			continue
		}

		offer, newDuration, newPrice, ok := pickNextOffer(edge.From, offers, p, deps.Graph)
		if !ok {
			plan.NHopRanked = append(plan.NHopRanked, NHopOutcome{FromPath: edge.From.Path, To: edge.To, LBUSD: edge.LB, Outcome: "infeasible", Reason: "no feasible connecting offer"})
			continue
		}
		newLabel := nhopLabel{Path: append(append([]string{}, edge.From.Path...), edge.To), PriceUSD: newPrice, DurationMinutes: newDuration, LastOffer: offer}
		plan.NHopRanked = append(plan.NHopRanked, NHopOutcome{FromPath: edge.From.Path, To: edge.To, LBUSD: edge.LB, PriceUSD: newPrice, Outcome: "kept"})

		if edge.To == p.Destination {
			var layoverMin int
			if !isOrigin(edge.From) {
				if lay, ok := layover(edge.From.LastOffer, offer, deps.Graph); ok {
					layoverMin = int(lay.Minutes())
				}
			}
			r := Result{
				Path: newLabel.Path, PriceUSD: newLabel.PriceUSD, DurationMinutes: newLabel.DurationMinutes,
				SelfTransfer: true, TransferCount: len(newLabel.Path) - 2,
				LayoverMinutes: layoverMin, Stopover: time.Duration(layoverMin)*time.Minute > stopoverThreshold,
			}
			plan.FinalResult = paretoInsert(plan.FinalResult, r)
			if best == nil || r.PriceUSD < best.PriceUSD {
				best = &r
			}
			log.Info("nhop: reached destination", "path", newLabel.Path, "price_usd", r.PriceUSD)
			continue // Destination is a result, not a node to expand further
		}

		if dominated(settled[edge.To], newLabel) {
			plan.NHopRanked[len(plan.NHopRanked)-1].Outcome = "dominated"
			continue
		}
		settled[edge.To] = keepNonDominated(settled[edge.To], newLabel)

		if newLabel.legs() >= p.MaxLegs {
			continue // hard depth cutoff — this label simply never expands further
		}
		if queriesUsed >= p.QueryBudget {
			break
		}
		frontier = append(frontier, candidateEdges(ctx, deps, p, newLabel, destination)...)
	}

	plan.Status = "done"
	plan.QueriesUsed = queriesUsed
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
	log.Info("nhop: search done", "queries_used", queriesUsed, "results", len(plan.FinalResult))
	return plan, nil
}

func isOrigin(l nhopLabel) bool { return len(l.Path) == 1 }

// candidateEdges ranks every not-yet-visited neighbor of from's current
// node as a not-yet-scraped nhopEdge — the N-hop generalization of
// ResolveCandidates' hub ranking: geometry-pruned (can this candidate,
// plus a same-speed finish to Destination, still fit the remaining time
// budget?), country-filtered (Params.ExcludedCountries/MaxCountries),
// then ranked by admissible lower bound so the caller's best-first loop
// tries the most promising one first.
func candidateEdges(ctx context.Context, deps Deps, p Params, from nhopLabel, destination openflights.Airport) []nhopEdge {
	fromAirport, ok := deps.Graph.Airport(from.node())
	if !ok {
		return nil
	}
	visited := make(map[string]bool, len(from.Path))
	for _, a := range from.Path {
		visited[a] = true
	}

	remainingHours := p.MaxHours - float64(from.DurationMinutes)/60

	var edges []nhopEdge
	for _, candIATA := range deps.Graph.Neighbors(from.node()) {
		if visited[candIATA] {
			continue // never revisit an airport already on this itinerary
		}
		cand, ok := deps.Graph.Airport(candIATA)
		if !ok {
			continue
		}
		if excludesCountry(p.ExcludedCountries, cand.Country) {
			continue
		}
		if p.MaxCountries > 0 && countDistinctCountries(deps.Graph, from.Path, cand.Country) > p.MaxCountries {
			continue
		}

		// Hub-connectivity prune: CandidateHubs (the 1-stop case) only
		// ever considers airports that already connect directly to
		// Destination, which implicitly rules out a dead-end candidate.
		// Neighbors has no such built-in filter — an intermediate hop
		// legitimately doesn't need a direct route to Destination — so
		// without this check, a node with many direct routes (a major
		// hub) fans out into every one of them equally, including tiny
		// regional airports with no onward long-haul connectivity at
		// all. Those look deceptively cheap by raw distance alone (a
		// short first hop barely moves an admissible bound dominated by
		// one huge remaining leg, however geometrically pointless the
		// hop actually is) and starve the flat query budget before a
		// real path ever gets a second hop. Real long-haul routing goes
		// through well-connected hubs, not regional strips — a
		// candidate's own out-degree is a cheap, real signal for that,
		// unlike straight-line distance.
		if deps.Graph.Routes[candIATA] == nil ||
			(len(deps.Graph.Routes[candIATA]) < minHubOutDegree && !deps.Graph.Routes[candIATA][p.Destination]) {
			continue
		}

		legMiles := openflights.DistanceMiles(fromAirport, cand)
		remainingMiles := openflights.DistanceMiles(cand, destination)
		layoverFloor := 0.0
		if !isOrigin(from) {
			layoverFloor = float64(p.MinLayoverMinutes)
		}
		if estimateMinutes(legMiles)+estimateMinutes(remainingMiles)+layoverFloor > remainingHours*60 {
			continue // geometry prune: can't fit even in the best case
		}

		legLB := deps.lowerBoundUSD(ctx, from.node(), candIATA, p.DepartDate, legMiles, p.PricePerMile)
		remLB := deps.lowerBoundUSD(ctx, candIATA, p.Destination, p.DepartDate, remainingMiles, p.PricePerMile)
		edges = append(edges, nhopEdge{From: from, To: candIATA, LB: from.PriceUSD + legLB + remLB})
	}
	return edges
}

// pickNextOffer chooses the cheapest offer from `to` that's feasible
// given the label it extends — layover-compatible with the label's own
// last leg (skipped for the very first hop, which has none), and within
// the remaining MaxHours/MaxPrice budget — mirroring bestConnection's
// same "only the single cheapest offer, not every combination" tradeoff
// for the 1-stop case (see its own doc comment). Returns the offer plus
// the new cumulative duration/price a label extended with it would have.
func pickNextOffer(from nhopLabel, offers []googleflights.Offer, p Params, graph *openflights.Graph) (offer googleflights.Offer, durationMinutes int, priceUSD float64, ok bool) {
	first := isOrigin(from)
	var best googleflights.Offer
	var bestTotal time.Duration
	found := false
	for _, o := range offers {
		var lay time.Duration
		if !first {
			l, layOK := layover(from.LastOffer, o, graph)
			if !layOK {
				continue
			}
			if l < time.Duration(p.MinLayoverMinutes)*time.Minute || l > time.Duration(p.MaxLayoverMinutes)*time.Minute {
				continue
			}
			lay = l
		}
		legDur, durOK := tripDuration(o, graph)
		if !durOK {
			continue
		}
		total := time.Duration(from.DurationMinutes)*time.Minute + lay + legDur
		if total.Hours() > p.MaxHours {
			continue
		}
		if p.MaxPrice > 0 && from.PriceUSD+float64(o.Price) > float64(p.MaxPrice) {
			continue
		}
		if !found || o.Price < best.Price {
			best, bestTotal, found = o, total, true
		}
	}
	return best, int(bestTotal.Minutes()), from.PriceUSD + float64(best.Price), found
}

// dominated reports whether some label in existing already beats
// candidate on both price and duration — the standard multi-criteria
// label-setting check (Dijkstra's relaxation, extended to a Pareto
// pair): a dominated label can never be part of a better itinerary than
// one already found to the same node, so it's discarded rather than
// expanded.
func dominated(existing []nhopLabel, candidate nhopLabel) bool {
	for _, e := range existing {
		if e.PriceUSD <= candidate.PriceUSD && e.DurationMinutes <= candidate.DurationMinutes {
			return true
		}
	}
	return false
}

// keepNonDominated adds candidate to existing (candidate is assumed
// already checked non-dominated by the caller) and drops any existing
// label candidate itself now dominates — same idea as paretoInsert,
// applied to in-progress labels rather than finished Results.
func keepNonDominated(existing []nhopLabel, candidate nhopLabel) []nhopLabel {
	kept := existing[:0]
	for _, e := range existing {
		if !(candidate.PriceUSD <= e.PriceUSD && candidate.DurationMinutes <= e.DurationMinutes) {
			kept = append(kept, e)
		}
	}
	return append(kept, candidate)
}

// markEdgesCutoff records the frontier-cutoff audit entries in bulk —
// pure bookkeeping split out so the main loop's break statement reads
// as one line.
func markEdgesCutoff(plan *Plan, edges []nhopEdge, outcome, reason string) {
	for _, e := range edges {
		plan.NHopRanked = append(plan.NHopRanked, NHopOutcome{FromPath: e.From.Path, To: e.To, LBUSD: e.LB, Outcome: outcome, Reason: reason})
	}
}

// Package routesearch implements the "Cheap multi-leg route search"
// algorithm from DESIGN.md: best-first branch-and-bound over hub
// candidates drawn from the openflights route-existence graph, spending
// one real googleflights scrape per edge under a hard query budget,
// producing both a ranked (price, duration) result set and a full JSON
// audit trail of every candidate considered.
//
// This is the same direct-run shortcut cmd/collector already takes:
// no Temporal yet, so "pacing" is a plain time.Sleep between queries
// rather than a durable workflow timer, and the audit trail is written
// once at the end rather than incrementally per step. See DESIGN.md
// "Pacing, audit trail, and observability" for the target version.
package routesearch

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/store"
)

// Params is one user request's constraints. One-way only for now — see
// the package doc for what else is deliberately out of scope.
type Params struct {
	Origin            string
	Destination       string
	DepartDate        string // YYYY-MM-DD
	MaxHours          float64
	QueryBudget       int
	MinLayoverMinutes int
	MaxLayoverMinutes int
	PricePerMile      float64       // fallback lower-bound prior when nothing's cached
	Delay             time.Duration // stand-in for Temporal's durable timer; see DESIGN.md "Pacing"
}

// LegOutcome records what happened (or didn't) when a single leg was
// considered, for the audit trail.
type LegOutcome struct {
	Queried   bool      `json:"queried"`
	PriceUSD  float64   `json:"price_usd,omitempty"`
	QueriedAt time.Time `json:"queried_at,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// CandidateOutcome is one hub's full audit-trail entry.
type CandidateOutcome struct {
	Hub         string      `json:"hub"`
	LBUSD       float64     `json:"lb_usd"`
	Rank        int         `json:"rank"`
	Leg1        *LegOutcome `json:"leg1,omitempty"`
	Leg2        *LegOutcome `json:"leg2,omitempty"`
	Outcome     string      `json:"outcome"` // kept | pruned | leg1_infeasible | leg2_infeasible | frontier_cutoff | budget_exhausted
	CombinedUSD float64     `json:"combined_usd,omitempty"`
	Reason      string      `json:"reason,omitempty"`
}

// Result is one itinerary in the final Pareto set.
type Result struct {
	Path            []string `json:"path"` // e.g. ["SFO","DEN","JFK"]
	PriceUSD        float64  `json:"price_usd"`
	DurationMinutes int      `json:"duration_minutes"`
	SelfTransfer    bool     `json:"self_transfer"` // separate-ticket combo; see DESIGN.md "Output"
	LayoverMinutes  int      `json:"layover_minutes,omitempty"`
	Stopover        bool     `json:"stopover,omitempty"` // layover long enough it's really a mini-trip, not a connection
}

// stopoverThreshold: a layover past this is flagged as a deliberate
// stopover rather than a connection — long enough to plausibly need a
// hotel, which this tool doesn't price (no lodging data source), so it
// only labels the option rather than costing it in.
const stopoverThreshold = 6 * time.Hour

// Plan is the full per-request audit trail (see DESIGN.md "Audit
// trail") — persisted to the store as one JSON document.
type Plan struct {
	RequestID                    string             `json:"request_id"`
	Input                        Params             `json:"input"`
	CandidatesConsidered         int                `json:"candidates_considered"`
	CandidatesAfterGeometryPrune int                `json:"candidates_after_geometry_prune"`
	CandidatesRanked             []CandidateOutcome `json:"candidates_ranked"`
	FinalResult                  []Result           `json:"final_result"`
	QueriesUsed                  int                `json:"queries_used"`
	Status                       string             `json:"status"`
}

// Deps are this search's collaborators — a real googleflights client,
// the openflights route-existence graph, the local audit/price store,
// and a structured logger (see DESIGN.md "Step-level visibility").
type Deps struct {
	Flights *googleflights.Client
	Graph   *openflights.Graph
	Store   *store.SQLite
	Logger  *slog.Logger
}

const (
	avgCruiseMPH         = 500.0 // rough commercial cruise speed, for the geometry prune only
	perLegOverheadMinute = 45.0  // taxi/climb/descent/etc, not part of cruise time
)

func estimateMinutes(distanceMiles float64) float64 {
	return distanceMiles/avgCruiseMPH*60 + perLegOverheadMinute
}

// candidate is a hub still in play after the geometry prune, ordered by
// lb (the admissible lower-bound estimate of the full A→hub→B price).
type candidate struct {
	hub    string
	lb     float64
	leg2mi float64
}

// Search runs the algorithm end to end: baseline direct search, hub
// candidate generation + geometry prune, then a best-first,
// budget-bounded loop over the survivors. Always returns a Plan (with
// Status set) even on a request-level error, since a partial audit
// trail is still worth keeping.
func Search(ctx context.Context, deps Deps, p Params) (*Plan, error) {
	requestID := fmt.Sprintf("%s-%s-%s-%d", p.Origin, p.Destination, p.DepartDate, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &Plan{RequestID: requestID, Input: p, Status: "running"}

	if err := deps.Store.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan)); err != nil {
		log.Warn("saving initial plan failed", "error", err)
	}

	origin, ok1 := deps.Graph.Airport(p.Origin)
	destination, ok2 := deps.Graph.Airport(p.Destination)
	if !ok1 || !ok2 {
		plan.Status = fmt.Sprintf("error: unknown airport (origin ok=%v, destination ok=%v)", ok1, ok2)
		_ = deps.Store.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}

	queriesUsed := 0
	var best *Result

	// Step 0: baseline. Google's own search already finds the best
	// single-ticket itinerary; every split-ticket candidate below only
	// matters if it can beat this.
	log.Info("querying baseline direct route")
	baseOffers, _, err := deps.Flights.SearchFlightOffers(ctx, googleflights.SearchParams{
		Origin: p.Origin, Destination: p.Destination, DepartureDate: p.DepartDate,
	})
	queriesUsed++
	if err != nil {
		log.Warn("baseline search failed", "error", err)
	} else {
		deps.recordOffers(ctx, p.Origin, p.Destination, p.DepartDate, baseOffers)
	}
	baseline := CandidateOutcome{Hub: "(direct)", Rank: 0}
	if offer, dur, ok := pickCheapestFeasible(baseOffers, deps.Graph, p.MaxHours); ok {
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
	sleepPacing(ctx, p.Delay)

	// Step 1: candidate hubs, geometry-pruned (pure arithmetic, no
	// scrapes) and price-ranked (cache-first, else distance × $/mile).
	rawHubs := deps.Graph.CandidateHubs(p.Origin, p.Destination)
	plan.CandidatesConsidered = len(rawHubs)

	var survivors []candidate
	for _, h := range rawHubs {
		hub, ok := deps.Graph.Airport(h)
		if !ok {
			continue
		}
		d1 := openflights.DistanceMiles(origin, hub)
		d2 := openflights.DistanceMiles(hub, destination)
		if estimateMinutes(d1)+estimateMinutes(d2)+float64(p.MinLayoverMinutes) > p.MaxHours*60 {
			continue // geometry prune: can't fit even with a minimum-length layover
		}
		lb1 := deps.lowerBoundUSD(ctx, p.Origin, h, p.DepartDate, d1, p.PricePerMile)
		lb2 := deps.lowerBoundUSD(ctx, h, p.Destination, p.DepartDate, d2, p.PricePerMile)
		survivors = append(survivors, candidate{hub: h, lb: lb1 + lb2, leg2mi: d2})
	}
	plan.CandidatesAfterGeometryPrune = len(survivors)
	sort.Slice(survivors, func(i, j int) bool { return survivors[i].lb < survivors[j].lb })

	log.Info("candidate hubs ready",
		"considered", plan.CandidatesConsidered, "after_geometry_prune", plan.CandidatesAfterGeometryPrune)

	// Step 2: best-first, budget-bounded loop (see DESIGN.md
	// "Exploration algorithm" for the full derivation).
	for i, c := range survivors {
		if best != nil && c.lb >= best.PriceUSD {
			markRemaining(plan, survivors[i:], i+1, "frontier_cutoff", "LB >= best.price")
			break
		}
		if queriesUsed >= p.QueryBudget {
			markRemaining(plan, survivors[i:], i+1, "budget_exhausted", "query budget exhausted")
			break
		}

		outcome := CandidateOutcome{Hub: c.hub, LBUSD: c.lb, Rank: i + 1}
		log.Info("querying leg 1", "hub", c.hub, "lb_usd", c.lb)

		sleepPacing(ctx, p.Delay)
		leg1Offers, _, err := deps.Flights.SearchFlightOffers(ctx, googleflights.SearchParams{
			Origin: p.Origin, Destination: c.hub, DepartureDate: p.DepartDate,
		})
		queriesUsed++
		deps.recordOffers(ctx, p.Origin, c.hub, p.DepartDate, leg1Offers)

		leg1, _, ok := pickCheapestFeasible(leg1Offers, deps.Graph, p.MaxHours)
		if err != nil || !ok {
			outcome.Leg1 = &LegOutcome{Queried: true, QueriedAt: time.Now(), Reason: "no feasible offer"}
			outcome.Outcome = "leg1_infeasible"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			log.Info("leg 1 infeasible", "hub", c.hub)
			continue
		}
		outcome.Leg1 = &LegOutcome{Queried: true, PriceUSD: float64(leg1.Price), QueriedAt: time.Now()}

		hEst := deps.lowerBoundUSD(ctx, c.hub, p.Destination, p.DepartDate, c.leg2mi, p.PricePerMile)
		if best != nil && float64(leg1.Price)+hEst >= best.PriceUSD {
			outcome.Outcome = "pruned"
			outcome.Reason = "g1 + hEst >= best.price"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			log.Info("leg 2 skipped", "hub", c.hub, "reason", outcome.Reason)
			continue
		}

		if queriesUsed >= p.QueryBudget {
			outcome.Outcome = "pruned"
			outcome.Reason = "query budget exhausted before leg 2"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			break
		}

		// Query leg 2 for the date leg 1 actually lands on — handles an
		// overnight leg 1 landing the day after DepartDate.
		leg2Date := dateString(leg1.Segments[len(leg1.Segments)-1].ArrivalDate)
		log.Info("querying leg 2", "hub", c.hub, "date", leg2Date)
		sleepPacing(ctx, p.Delay)
		leg2Offers, _, err := deps.Flights.SearchFlightOffers(ctx, googleflights.SearchParams{
			Origin: c.hub, Destination: p.Destination, DepartureDate: leg2Date,
		})
		queriesUsed++
		deps.recordOffers(ctx, c.hub, p.Destination, leg2Date, leg2Offers)

		leg2, combined, layoverDur, feasible := deps.bestConnection(leg1, leg2Offers, p)
		if err != nil || !feasible {
			outcome.Leg2 = &LegOutcome{Queried: true, QueriedAt: time.Now(), Reason: "no feasible connection"}
			outcome.Outcome = "leg2_infeasible"
			plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)
			log.Info("leg 2 infeasible", "hub", c.hub)
			continue
		}

		total := leg1.Price + leg2.Price
		outcome.Leg2 = &LegOutcome{Queried: true, PriceUSD: float64(leg2.Price), QueriedAt: time.Now()}
		outcome.CombinedUSD = float64(total)
		outcome.Outcome = "kept"
		plan.CandidatesRanked = append(plan.CandidatesRanked, outcome)

		r := Result{
			Path:            []string{p.Origin, c.hub, p.Destination},
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
		log.Info("candidate kept", "hub", c.hub, "combined_usd", r.PriceUSD, "duration_minutes", r.DurationMinutes)
	}

	plan.QueriesUsed = queriesUsed
	plan.Status = "done"
	if err := deps.Store.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan)); err != nil {
		log.Warn("saving final plan failed", "error", err)
	}
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
func markRemaining(plan *Plan, rest []candidate, startRank int, outcome, reason string) {
	for j, c := range rest {
		plan.CandidatesRanked = append(plan.CandidatesRanked, CandidateOutcome{
			Hub: c.hub, LBUSD: c.lb, Rank: startRank + j,
			Outcome: outcome, Reason: reason,
		})
	}
}

func sleepPacing(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

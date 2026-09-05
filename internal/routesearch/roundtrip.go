package routesearch

import (
	"context"
	"fmt"
	"time"

	"flight-search-intelligence/internal/googleflights"
)

// RoundTripResult is the combined outcome: either Google's own bundled
// round-trip fare, or two independently-priced one-way directions
// (sometimes cheaper — the same "priced differently through different
// inventory" effect a hub split exploits), whichever wins. See
// DESIGN.md "Round trips and flexible dates".
type RoundTripResult struct {
	TotalPriceUSD           float64  `json:"total_price_usd"`
	Bundled                 bool     `json:"bundled"` // true: Google's bundled fare won over the summed one-ways
	OutboundPath            []string `json:"outbound_path"`
	OutboundPriceUSD        float64  `json:"outbound_price_usd,omitempty"`
	OutboundDurationMinutes int      `json:"outbound_duration_minutes,omitempty"`
	OutboundSelfTransfer    bool     `json:"outbound_self_transfer,omitempty"`
	ReturnPath              []string `json:"return_path"`
	ReturnPriceUSD          float64  `json:"return_price_usd,omitempty"`
	ReturnDurationMinutes   int      `json:"return_duration_minutes,omitempty"`
	ReturnSelfTransfer      bool     `json:"return_self_transfer,omitempty"`
}

// RoundTripPlan is the audit trail for one round-trip request. The
// outbound and return legs each get their own full one-way Plan (see
// Search) — OutboundPlanID/ReturnPlanID point at those, already
// persisted separately with their own candidates_ranked; this plan only
// records the round-trip-specific decision: bundled fare vs. summed
// one-ways.
type RoundTripPlan struct {
	RequestID       string           `json:"request_id"`
	Origin          string           `json:"origin"`
	Destination     string           `json:"destination"`
	DepartDate      string           `json:"depart_date"`
	ReturnDate      string           `json:"return_date"`
	BundledPriceUSD float64          `json:"bundled_price_usd,omitempty"`
	BundledQueried  bool             `json:"bundled_queried"`
	BundledReason   string           `json:"bundled_reason,omitempty"`
	OutboundPlanID  string           `json:"outbound_plan_id"`
	ReturnPlanID    string           `json:"return_plan_id"`
	Result          *RoundTripResult `json:"result,omitempty"`
	QueriesUsed     int              `json:"queries_used"` // this plan's own (bundled) query; outbound/return track their own separately
	Status          string           `json:"status"`
}

// SearchRoundTrip runs the round-trip comparison: Google's own bundled
// round-trip fare vs. two independent Search() calls (each already doing
// the full baseline + hub search from "Exploration algorithm"), keeping
// whichever total is cheaper.
func SearchRoundTrip(ctx context.Context, deps Deps, p Params, returnDate string) (*RoundTripPlan, error) {
	requestID := fmt.Sprintf("RT-%s-%s-%s-%s-%d", p.Origin, p.Destination, p.DepartDate, returnDate, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &RoundTripPlan{
		RequestID: requestID, Origin: p.Origin, Destination: p.Destination,
		DepartDate: p.DepartDate, ReturnDate: returnDate, Status: "running",
	}
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))

	log.Info("querying bundled round-trip baseline")
	bundledOffers, live, err := deps.searchOffers(ctx, googleflights.SearchParams{
		Origin: p.Origin, Destination: p.Destination, DepartureDate: p.DepartDate, ReturnDate: returnDate,
	}, p.ForceRefresh)
	if live {
		plan.QueriesUsed++
	}
	plan.BundledQueried = true
	switch {
	case err != nil:
		plan.BundledReason = err.Error()
	default:
		// Deliberately not filtered by MaxHours/tripDuration here: a
		// bundled offer's segments span outbound *and* return, so "first
		// departure to last arrival" is the whole trip length in days,
		// not a transit-time figure MaxHours was ever meant to bound.
		// Price alone decides this comparison.
		if offer, ok := cheapestOffer(bundledOffers); ok {
			plan.BundledPriceUSD = float64(offer.Price)
		} else {
			plan.BundledReason = "no feasible bundled offer"
		}
	}
	if live {
		sleepPacing(ctx, p.Delay)
	}

	log.Info("searching outbound leg", "date", p.DepartDate)
	outboundPlan, err := Search(ctx, deps, p)
	if err != nil {
		plan.Status = fmt.Sprintf("error: outbound leg: %v", err)
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: outbound leg: %w", err)
	}
	plan.OutboundPlanID = outboundPlan.RequestID
	plan.QueriesUsed += outboundPlan.QueriesUsed

	log.Info("searching return leg", "date", returnDate)
	returnParams := p
	returnParams.Origin, returnParams.Destination = p.Destination, p.Origin
	returnParams.DepartDate = returnDate
	returnPlan, err := Search(ctx, deps, returnParams)
	if err != nil {
		plan.Status = fmt.Sprintf("error: return leg: %v", err)
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: return leg: %w", err)
	}
	plan.ReturnPlanID = returnPlan.RequestID
	plan.QueriesUsed += returnPlan.QueriesUsed

	var outboundBest, returnBest *Result
	var summed float64
	if len(outboundPlan.FinalResult) > 0 {
		outboundBest = cheapestResult(outboundPlan.FinalResult)
		summed += outboundBest.PriceUSD
	}
	if len(returnPlan.FinalResult) > 0 {
		returnBest = cheapestResult(returnPlan.FinalResult)
		summed += returnBest.PriceUSD
	}

	plan.Result = combineRoundTrip(plan, outboundBest, returnBest, summed)
	plan.Status = "done"
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
	log.Info("round trip search done", "queries_used", plan.QueriesUsed)
	return plan, nil
}

func combineRoundTrip(plan *RoundTripPlan, outboundBest, returnBest *Result, summed float64) *RoundTripResult {
	haveBundled := plan.BundledQueried && plan.BundledPriceUSD > 0
	haveSummed := outboundBest != nil && returnBest != nil

	switch {
	case haveBundled && (!haveSummed || plan.BundledPriceUSD < summed):
		return &RoundTripResult{
			TotalPriceUSD: plan.BundledPriceUSD, Bundled: true,
			OutboundPath: []string{plan.Origin, plan.Destination},
			ReturnPath:   []string{plan.Destination, plan.Origin},
		}
	case haveSummed:
		return &RoundTripResult{
			TotalPriceUSD: summed, Bundled: false,
			OutboundPath: outboundBest.Path, OutboundPriceUSD: outboundBest.PriceUSD,
			OutboundDurationMinutes: outboundBest.DurationMinutes, OutboundSelfTransfer: outboundBest.SelfTransfer,
			ReturnPath: returnBest.Path, ReturnPriceUSD: returnBest.PriceUSD,
			ReturnDurationMinutes: returnBest.DurationMinutes, ReturnSelfTransfer: returnBest.SelfTransfer,
		}
	default:
		return nil
	}
}

package main

import (
	"fmt"

	"flight-search-intelligence/internal/routesearch"
)

func printSummary(plan *routesearch.Plan) {
	fmt.Printf("\nRequest %s: %d queries used, %d/%d candidate hubs survived the geometry prune.\n",
		plan.RequestID, plan.QueriesUsed, plan.CandidatesAfterGeometryPrune, plan.CandidatesConsidered)

	printCandidates(plan)

	if len(plan.FinalResult) == 0 {
		fmt.Println("\nNo feasible itineraries found.")
		return
	}
	fmt.Printf("\n%d result(s) (Pareto set — price vs. duration):\n", len(plan.FinalResult))
	for i, r := range plan.FinalResult {
		fmt.Printf("  [%d] %s\n", i+1, describeResult(r))
	}
	fmt.Printf("\nFull audit trail saved to the store (route_search_plans, id=%s).\n", plan.RequestID)
}

func printRoundTrip(plan *routesearch.RoundTripPlan) {
	fmt.Printf("\nRound trip %s -> %s, %s / %s. Queries used: %d (bundled=%v, outbound plan=%s, return plan=%s)\n",
		plan.Origin, plan.Destination, plan.DepartDate, plan.ReturnDate, plan.QueriesUsed,
		plan.BundledQueried, plan.OutboundPlanID, plan.ReturnPlanID)
	if plan.BundledQueried {
		if plan.BundledPriceUSD > 0 {
			fmt.Printf("Bundled round-trip fare: $%.0f\n", plan.BundledPriceUSD)
		} else {
			fmt.Printf("Bundled round-trip fare: unavailable (%s)\n", plan.BundledReason)
		}
	}
	if plan.Result == nil {
		fmt.Println("\nNo feasible round trip found.")
		return
	}
	r := plan.Result
	if r.Bundled {
		fmt.Printf("\nBest: $%.0f — bundled round-trip fare beat buying the two directions separately.\n", r.TotalPriceUSD)
		return
	}
	fmt.Printf("\nBest: $%.0f — cheaper bought as two separate one-ways than the bundled fare:\n", r.TotalPriceUSD)
	fmt.Printf("  Outbound: %s\n", describeResult(routesearch.Result{
		Path: r.OutboundPath, PriceUSD: r.OutboundPriceUSD, DurationMinutes: r.OutboundDurationMinutes, SelfTransfer: r.OutboundSelfTransfer,
	}))
	fmt.Printf("  Return:   %s\n", describeResult(routesearch.Result{
		Path: r.ReturnPath, PriceUSD: r.ReturnPriceUSD, DurationMinutes: r.ReturnDurationMinutes, SelfTransfer: r.ReturnSelfTransfer,
	}))
	fmt.Printf("\nSee outbound/return plans (route_search_plans ids %s, %s) for each direction's full candidate table.\n",
		plan.OutboundPlanID, plan.ReturnPlanID)
}

func printFlexible(plan *routesearch.FlexiblePlan) {
	fmt.Printf("\nDate scan (Phase A) — %d date point(s) tried:\n", len(plan.DateScan))
	fmt.Printf("%-12s %-12s %-9s %s\n", "Depart", "Return", "Price $", "Note")
	for _, e := range plan.DateScan {
		price := ""
		if e.PriceUSD > 0 {
			price = fmt.Sprintf("%.0f", e.PriceUSD)
		}
		marker := ""
		if e.DepartDate == plan.ChosenDepartDate && e.ReturnDate == plan.ChosenReturnDate {
			marker = "<- chosen"
		}
		fmt.Printf("%-12s %-12s %-9s %s %s\n", e.DepartDate, e.ReturnDate, price, e.Reason, marker)
	}

	if plan.AnchoredPlanID == "" {
		fmt.Printf("\nChosen date: %s", plan.ChosenDepartDate)
		if plan.ChosenReturnDate != "" {
			fmt.Printf(" / %s", plan.ChosenReturnDate)
		}
		fmt.Println(" (-scan-dates: connecting-hub search skipped)")
		return
	}

	fmt.Printf("\nPhase B ran on %s", plan.ChosenDepartDate)
	if plan.ChosenReturnDate != "" {
		fmt.Printf(" / %s", plan.ChosenReturnDate)
	}
	fmt.Printf(" (anchored plan id=%s):\n", plan.AnchoredPlanID)

	switch {
	case plan.RoundTripResult != nil:
		r := plan.RoundTripResult
		if r.Bundled {
			fmt.Printf("Best: $%.0f (bundled round-trip fare)\n", r.TotalPriceUSD)
		} else {
			fmt.Printf("Best: $%.0f (outbound $%.0f + return $%.0f, bought separately)\n",
				r.TotalPriceUSD, r.OutboundPriceUSD, r.ReturnPriceUSD)
		}
	case plan.OneWayResult != nil:
		fmt.Printf("Best: %s\n", describeResult(*plan.OneWayResult))
	default:
		fmt.Println("No feasible itinerary found on the chosen date.")
	}
}

func describeResult(r routesearch.Result) string {
	kind := "single-ticket"
	if r.SelfTransfer {
		kind = "SEPARATE TICKETS — self-transfer risk"
	}
	stopover := ""
	if r.Stopover {
		stopover = fmt.Sprintf(" [STOPOVER: %dh%02dm layover, not priced — factor in lodging yourself]",
			r.LayoverMinutes/60, r.LayoverMinutes%60)
	}
	return fmt.Sprintf("$%.0f, %dh%02dm, %s (%s)%s",
		r.PriceUSD, r.DurationMinutes/60, r.DurationMinutes%60, joinPath(r.Path), kind, stopover)
}

// printCandidatePreview renders ResolveCandidates' output (-dry-run):
// airport resolution + ranked hub candidates, no scraping done.
func printCandidatePreview(p *routesearch.CandidatePreview) {
	fmt.Printf("%s (%s, %s) -> %s (%s, %s)\n",
		p.Origin.IATA, p.Origin.Name, p.Origin.City,
		p.Destination.IATA, p.Destination.Name, p.Destination.City)
	fmt.Printf("Direct distance: %.0f mi. Direct nonstop exists: %v.\n", p.DirectDistanceMiles, p.HasNonstop)

	fmt.Printf("\n%d raw candidate hub(s), %d survive the geometry prune, ranked by lower-bound price:\n",
		p.CandidatesConsidered, p.CandidatesAfterGeometryPrune)
	fmt.Printf("%-6s %-9s %-10s %-10s\n", "Hub", "LB $", "Leg1 mi", "Leg2 mi")
	for _, h := range p.RankedHubs {
		fmt.Printf("%-6s %-9.0f %-10.0f %-10.0f\n", h.Hub, h.LBUSD, h.Leg1Miles, h.Leg2Miles)
	}
}

// printCandidates prints the full audit-trail table — rank 0 is always
// the direct baseline every hub candidate is measured against, in the
// same table as the hub candidates rather than off to the side.
func printCandidates(plan *routesearch.Plan) {
	fmt.Printf("\n%-5s %-6s %-9s %-18s %-8s %-8s %-11s %s\n",
		"Rank", "Hub", "LB $", "Outcome", "Leg1 $", "Leg2 $", "Combined $", "Reason")
	for _, c := range plan.CandidatesRanked {
		leg1, leg2 := "", ""
		if c.Leg1 != nil && c.Leg1.PriceUSD > 0 {
			leg1 = fmt.Sprintf("%.0f", c.Leg1.PriceUSD)
		}
		if c.Leg2 != nil && c.Leg2.PriceUSD > 0 {
			leg2 = fmt.Sprintf("%.0f", c.Leg2.PriceUSD)
		}
		combined := ""
		if c.CombinedUSD > 0 {
			combined = fmt.Sprintf("%.0f", c.CombinedUSD)
		}
		fmt.Printf("%-5d %-6s %-9.1f %-18s %-8s %-8s %-11s %s\n",
			c.Rank, c.Hub, c.LBUSD, c.Outcome, leg1, leg2, combined, c.Reason)
	}
}

func joinPath(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += " -> "
		}
		out += p
	}
	return out
}

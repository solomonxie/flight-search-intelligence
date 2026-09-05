// Command routesearch runs the "cheap multi-leg route search" algorithm
// (see DESIGN.md "Cheap multi-leg route search") directly: no Kafka, no
// Temporal, no email — one request, run to completion, same direct-run
// shortcut cmd/collector already takes for the plain single-leg case.
//
// Pacing between scrapes is a plain -delay flag here (default a few
// seconds, for interactive use) rather than the minutes-to-hours Temporal
// durable timer DESIGN.md's target version uses — see routesearch's
// package doc.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/common"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/routesearch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "routesearch:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = common.Load(".env")

	origin := flag.String("origin", "", "origin IATA airport code, e.g. SFO (required)")
	destination := flag.String("destination", "", "destination IATA airport code, e.g. JFK (required)")
	date := flag.String("date", "", "departure date, YYYY-MM-DD (required) — the flexible-date window's center, if -date-window-days > 0")
	returnDate := flag.String("return-date", "", "return date, YYYY-MM-DD — triggers round-trip mode (bundled-fare vs. summed-one-ways comparison)")
	maxHours := flag.Float64("max-hours", 30, "max tolerable total elapsed trip time, in hours")
	budget := flag.Int("budget", 20, "max number of hub-search scrapes to spend per direction")
	minLayover := flag.Int("min-layover-minutes", 45, "minimum feasible layover, in minutes")
	maxLayover := flag.Int("max-layover-minutes", 12*60, "maximum feasible layover, in minutes (raise this + -max-hours for a deliberate multi-day stopover)")
	pricePerMile := flag.Float64("price-per-mile", 0.08, "fallback $/mile prior used when no cached price exists yet")
	delay := flag.Duration("delay", 3*time.Second, "pacing delay between scrapes (stand-in for Temporal's durable timer)")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite store path (price cache + audit trail)")
	openflightsDir := flag.String("openflights-dir", "data/openflights", "cache dir for the OpenFlights airports/routes dataset")
	forceRefresh := flag.Bool("force-refresh", false, "bypass the offers cache and scrape Google Flights live even for a query made within the last hour")

	dateWindowDays := flag.Int("date-window-days", 0, "flexible-date scan: +/- this many days around -date (0 disables flexible dates)")
	dateStepDays := flag.Int("date-step-days", 1, "flexible-date scan: sample every N days within the window")
	tripLengthDays := flag.Int("trip-length-days", 0, "flexible round trip: fixed trip length; defaults to (-return-date minus -date) if both given")
	dryRun := flag.Bool("dry-run", false, "resolve airports + rank candidate hubs only, no scraping (step 1 of the algorithm; overrides every other mode below)")
	scanDates := flag.Bool("scan-dates", false, "with -date-window-days, check the fare for each date in the window and stop — no connecting-hub search")
	flag.Parse()

	if *origin == "" || *destination == "" || *date == "" {
		flag.Usage()
		return fmt.Errorf("origin, destination, and date are required")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	fmt.Fprintln(os.Stderr, "Loading OpenFlights route graph (downloads once, then cached)...")
	graph, err := openflights.Load(*openflightsDir)
	if err != nil {
		return fmt.Errorf("loading openflights graph: %w", err)
	}

	db, err := catalog.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	deps := routesearch.Deps{
		Flights: googleflights.NewClient(),
		Graph:   graph,
		Catalog: db,
		Logger:  logger,
	}

	base := routesearch.Params{
		Origin: *origin, Destination: *destination, DepartDate: *date,
		MaxHours: *maxHours, QueryBudget: *budget,
		MinLayoverMinutes: *minLayover, MaxLayoverMinutes: *maxLayover,
		PricePerMile: *pricePerMile, Delay: *delay, ForceRefresh: *forceRefresh,
	}

	// No overall context deadline anywhere below: this is a deliberately
	// slow, days-SLA search (see DESIGN.md "Pacing") — only bounded by
	// -budget/-date-window-days, not by wall-clock time.
	ctx := context.Background()

	if *dryRun {
		preview, err := routesearch.ResolveCandidates(ctx, deps, base)
		if err != nil {
			return err
		}
		printCandidatePreview(preview)
		return nil
	}

	switch {
	case *dateWindowDays > 0:
		tripLen := *tripLengthDays
		if *returnDate != "" && tripLen == 0 {
			d1, err1 := time.Parse("2006-01-02", *date)
			d2, err2 := time.Parse("2006-01-02", *returnDate)
			if err1 != nil || err2 != nil {
				return fmt.Errorf("parsing -date/-return-date for -trip-length-days: %v / %v", err1, err2)
			}
			tripLen = int(d2.Sub(d1).Hours() / 24)
		}
		plan, err := routesearch.SearchFlexible(ctx, deps, routesearch.FlexibleParams{
			Base: base, RoundTrip: *returnDate != "", TripLengthDays: tripLen,
			WindowDays: *dateWindowDays, StepDays: *dateStepDays, ScanOnly: *scanDates,
		})
		if err != nil {
			return err
		}
		printFlexible(plan)

	case *returnDate != "":
		plan, err := routesearch.SearchRoundTrip(ctx, deps, base, *returnDate)
		if err != nil {
			return err
		}
		printRoundTrip(plan)

	default:
		plan, err := routesearch.Search(ctx, deps, base)
		if err != nil {
			return err
		}
		printSummary(plan)
	}

	return nil
}

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

// printCandidates prints the full audit-trail table — rank 0 is always
// the direct baseline every hub candidate is measured against, in the
// same table as the hub candidates rather than off to the side.
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

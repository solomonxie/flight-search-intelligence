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

	"flight-search-intelligence/envs"
	"flight-search-intelligence/googleflights"
	"flight-search-intelligence/openflights"
	"flight-search-intelligence/routesearch"
	"flight-search-intelligence/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "routesearch:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = envs.Load(".env")

	origin := flag.String("origin", "", "origin IATA airport code, e.g. SFO (required)")
	destination := flag.String("destination", "", "destination IATA airport code, e.g. JFK (required)")
	date := flag.String("date", "", "departure date, YYYY-MM-DD (required)")
	maxHours := flag.Float64("max-hours", 30, "max tolerable total elapsed trip time, in hours")
	budget := flag.Int("budget", 20, "max number of scrapes to spend on this search")
	minLayover := flag.Int("min-layover-minutes", 45, "minimum feasible layover, in minutes")
	maxLayover := flag.Int("max-layover-minutes", 12*60, "maximum feasible layover, in minutes")
	pricePerMile := flag.Float64("price-per-mile", 0.08, "fallback $/mile prior used when no cached price exists yet")
	delay := flag.Duration("delay", 3*time.Second, "pacing delay between scrapes (stand-in for Temporal's durable timer)")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite store path (price cache + audit trail)")
	openflightsDir := flag.String("openflights-dir", "data/openflights", "cache dir for the OpenFlights airports/routes dataset")
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

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	deps := routesearch.Deps{
		Flights: googleflights.NewClient(),
		Graph:   graph,
		Store:   db,
		Logger:  logger,
	}

	// No overall context deadline: this is a deliberately slow, days-SLA
	// search (see DESIGN.md "Pacing") — only bounded by -budget, not by
	// wall-clock time.
	plan, err := routesearch.Search(context.Background(), deps, routesearch.Params{
		Origin:            *origin,
		Destination:       *destination,
		DepartDate:        *date,
		MaxHours:          *maxHours,
		QueryBudget:       *budget,
		MinLayoverMinutes: *minLayover,
		MaxLayoverMinutes: *maxLayover,
		PricePerMile:      *pricePerMile,
		Delay:             *delay,
	})
	if err != nil {
		return err
	}

	printSummary(plan)
	return nil
}

func printSummary(plan *routesearch.Plan) {
	fmt.Printf("\nRequest %s: %d queries used, %d/%d candidate hubs survived the geometry prune.\n",
		plan.RequestID, plan.QueriesUsed, plan.CandidatesAfterGeometryPrune, plan.CandidatesConsidered)

	if len(plan.FinalResult) == 0 {
		fmt.Println("No feasible itineraries found.")
		return
	}
	fmt.Printf("\n%d result(s) (Pareto set — price vs. duration):\n", len(plan.FinalResult))
	for i, r := range plan.FinalResult {
		kind := "single-ticket"
		if r.SelfTransfer {
			kind = "SEPARATE TICKETS — self-transfer risk"
		}
		fmt.Printf("  [%d] $%.0f, %dh%02dm, %s (%s)\n",
			i+1, r.PriceUSD, r.DurationMinutes/60, r.DurationMinutes%60, joinPath(r.Path), kind)
	}
	fmt.Printf("\nFull audit trail saved to the store (route_search_plans, id=%s).\n", plan.RequestID)
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

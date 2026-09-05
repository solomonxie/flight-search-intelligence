// Command routesearch runs the "cheap multi-leg route search" algorithm
// (see DESIGN.md "Cheap multi-leg route search") directly: no Kafka, no
// Temporal, no email — one request, run to completion, same direct-run
// shortcut cmd/collector already takes for the plain single-leg case.
//
// Pacing between scrapes is a plain -delay flag here (default a few
// seconds, for interactive use) rather than the minutes-to-hours Temporal
// durable timer DESIGN.md's target version uses — see routesearch's
// package doc.
//
// This file is flag parsing + dispatch only; terminal output formatting
// lives in print.go, and every actual search behavior lives in the
// routesearch package.
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

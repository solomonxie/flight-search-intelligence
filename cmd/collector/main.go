// Command collector fetches real flight fares for one route/date by
// scraping Google Flights and writes the raw result to a local file.
//
// This is a light, direct-run version: no Kafka, no Temporal, no S3 — just
// `go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05`
// against real (live) fare data. See DESIGN.md "Collector task queue" for
// the target on-demand/queue-driven architecture this will grow into; this
// is step one, proving the provider fetch itself works.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flight-search-intelligence/internal/envs"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "collector:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = envs.Load(".env")

	origin := flag.String("origin", "", "origin IATA airport code, e.g. SFO (required)")
	destination := flag.String("destination", "", "destination IATA airport code, e.g. JFK (required)")
	date := flag.String("date", "", "departure date, YYYY-MM-DD (required)")
	returnDate := flag.String("return-date", "", "return date, YYYY-MM-DD (optional, round-trip)")
	adults := flag.Int("adults", 1, "number of adult passengers")
	outDir := flag.String("out-dir", "data/raw", "directory to write the raw HTML result into")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite serving-store path to write parsed offers into")
	flag.Parse()

	if *origin == "" || *destination == "" || *date == "" {
		flag.Usage()
		return fmt.Errorf("origin, destination, and date are required")
	}

	client := googleflights.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("Fetching fares: %s -> %s on %s", *origin, *destination, *date)
	if *returnDate != "" {
		fmt.Printf(" (return %s)", *returnDate)
	}
	fmt.Println(" ...")

	offers, raw, err := client.SearchFlightOffers(ctx, googleflights.SearchParams{
		Origin:        *origin,
		Destination:   *destination,
		DepartureDate: *date,
		ReturnDate:    *returnDate,
		Adults:        *adults,
	})
	if err != nil {
		return err
	}

	path, err := writeRaw(*outDir, *origin, *destination, *date, raw)
	if err != nil {
		return fmt.Errorf("writing raw result: %w", err)
	}
	fmt.Printf("Wrote raw result to %s\n\n", path)

	printSummary(offers)

	if err := saveOffers(*dbPath, *origin, *destination, *date, *returnDate, offers); err != nil {
		return fmt.Errorf("saving parsed offers: %w", err)
	}
	fmt.Printf("Saved %d offer(s) to %s\n", len(offers), *dbPath)
	return nil
}

// saveOffers persists parsed offers as etl/dbt's raw.flight_prices shape
// (see store.FlightPrice) into the local SQLite serving-store stand-in —
// skipping the Spark/Delta Lake/dbt gold pipeline DESIGN.md targets, for
// now, the same way this collector already skips Kafka/Temporal/S3.
func saveOffers(dbPath, origin, destination, departDate, returnDate string, offers []googleflights.Offer) error {
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	scrapedAt := time.Now()
	rows := make([]store.FlightPrice, len(offers))
	for i, o := range offers {
		rows[i] = store.FlightPrice{
			Origin:      origin,
			Destination: destination,
			Airline:     strings.Join(o.Airlines, ","),
			DepartDate:  departDate,
			ReturnDate:  returnDate,
			PriceCents:  int64(o.Price) * 100,
			Currency:    "USD",
			Source:      "google_flights",
			ScrapedAt:   scrapedAt,
		}
	}
	return db.InsertFlightPrices(context.Background(), rows)
}

// writeRaw drops the untouched HTML response into outDir, named so repeat
// runs for the same route/date don't collide. Stands in for "the S3 raw
// zone" (see DESIGN.md) until the collector is wired to Kafka/Temporal/S3.
func writeRaw(outDir, origin, destination, date string, raw []byte) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s-%s_%s.html", date, origin, destination, time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(outDir, name)
	return path, os.WriteFile(path, raw, 0o644)
}

func printSummary(offers []googleflights.Offer) {
	if len(offers) == 0 {
		fmt.Println("No offers returned.")
		return
	}
	fmt.Printf("%d offer(s):\n", len(offers))
	for i, offer := range offers {
		fmt.Printf("  [%d] $%d (%s)\n", i+1, offer.Price, offer.Type)
		for _, seg := range offer.Segments {
			fmt.Printf("        %s (%02d:%02d) -> %s (%02d:%02d)  %s\n",
				seg.FromAirport, seg.DepartureTime[0], seg.DepartureTime[1],
				seg.ToAirport, seg.ArrivalTime[0], seg.ArrivalTime[1], seg.PlaneType)
		}
	}
}

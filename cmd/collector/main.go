// Command collector fetches real flight fares for one route/date from the
// Amadeus Self-Service test API and writes the raw result to a local file.
//
// This is a light, direct-run version: no Kafka, no Temporal, no S3 — just
// `go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05`
// against real (if test-environment) fare data. See DESIGN.md "Collector
// task queue" for the target on-demand/queue-driven architecture this will
// grow into; this is step one, proving the provider fetch itself works.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/solomonxie/flight-search-intelligence/amadeus"
	"github.com/solomonxie/flight-search-intelligence/envs"
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
	max := flag.Int("max", 10, "max number of offers to fetch")
	outDir := flag.String("out-dir", "data/raw", "directory to write the raw JSON result into")
	flag.Parse()

	if *origin == "" || *destination == "" || *date == "" {
		flag.Usage()
		return fmt.Errorf("origin, destination, and date are required")
	}

	clientID := os.Getenv("AMADEUS_CLIENT_ID")
	clientSecret := os.Getenv("AMADEUS_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf(
			"AMADEUS_CLIENT_ID and AMADEUS_CLIENT_SECRET must be set (env or .env).\n" +
				"Get free test-API credentials at https://developers.amadeus.com/my-apps")
	}

	client := amadeus.NewClient(clientID, clientSecret)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("Fetching fares: %s -> %s on %s", *origin, *destination, *date)
	if *returnDate != "" {
		fmt.Printf(" (return %s)", *returnDate)
	}
	fmt.Println(" ...")

	result, raw, err := client.SearchFlightOffers(ctx, amadeus.SearchParams{
		Origin:        *origin,
		Destination:   *destination,
		DepartureDate: *date,
		ReturnDate:    *returnDate,
		Adults:        *adults,
		MaxResults:    *max,
	})
	if err != nil {
		return err
	}

	path, err := writeRaw(*outDir, *origin, *destination, *date, raw)
	if err != nil {
		return fmt.Errorf("writing raw result: %w", err)
	}
	fmt.Printf("Wrote raw result to %s\n\n", path)

	printSummary(result)
	return nil
}

// writeRaw drops the untouched API response into outDir, named so repeat
// runs for the same route/date don't collide. Stands in for "the S3 raw
// zone" (see DESIGN.md) until the collector is wired to Kafka/Temporal/S3.
func writeRaw(outDir, origin, destination, date string, raw []byte) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s-%s_%s.json", date, origin, destination, time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(outDir, name)

	var pretty map[string]interface{}
	if err := json.Unmarshal(raw, &pretty); err == nil {
		if b, err := json.MarshalIndent(pretty, "", "  "); err == nil {
			raw = b
		}
	}
	return path, os.WriteFile(path, raw, 0o644)
}

func printSummary(result *amadeus.SearchResponse) {
	if len(result.Data) == 0 {
		fmt.Println("No offers returned.")
		return
	}
	fmt.Printf("%d offer(s):\n", len(result.Data))
	for i, offer := range result.Data {
		fmt.Printf("  [%d] %s %s\n", i+1, offer.Price.Total, offer.Price.Currency)
		for _, itin := range offer.Itineraries {
			for _, seg := range itin.Segments {
				fmt.Printf("        %s%s  %s (%s) -> %s (%s)\n",
					seg.CarrierCode, seg.Number,
					seg.Departure.IATACode, seg.Departure.At,
					seg.Arrival.IATACode, seg.Arrival.At)
			}
		}
	}
}

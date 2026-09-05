// Command email-intake is a dev-only, one-shot CLI standing in for SES
// inbound until that's actually built (DESIGN.md "Local development").
// Two modes, and — unlike an earlier version of this binary — no
// background loop: each run does one thing and exits, the same way a
// single inbound email would trigger one action, not a persistent
// process. See cmd/agent-worker for where the persistent, always-running
// half of the agent loop actually lives now.
//
//   - -start: create a new request (stands in for an initial request
//     email) and publish the first DecisionTrigger (internal/kafka) so
//     cmd/agent-worker picks it up.
//   - -signal: append a soft constraint to an existing request (stands in
//     for a reply email on an existing thread) — just a database update,
//     no message needed; whatever runs next for that request reads the
//     spec fresh.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/common"
	"flight-search-intelligence/internal/kafka"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "email-intake:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = common.Load(".env")

	start := flag.Bool("start", false, "create a new request (stands in for an initial request email)")
	signal := flag.Bool("signal", false, "append a soft constraint to an existing request (stands in for a reply email)")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite store path")
	kafkaBrokers := flag.String("kafka-brokers", "localhost:9092", "comma-separated Kafka broker addresses (-start only)")

	origin := flag.String("origin", "", "origin IATA airport code (-start)")
	destination := flag.String("destination", "", "destination IATA airport code (-start)")
	date := flag.String("date", "", "departure date, YYYY-MM-DD (-start)")
	returnDate := flag.String("return-date", "", "return date, YYYY-MM-DD (-start, optional)")
	maxHours := flag.Float64("max-hours", 30, "max tolerable total elapsed trip time, in hours (-start)")
	budget := flag.Int("budget", 20, "max hub-search scrapes to spend (-start)")
	softConstraintsCSV := flag.String("soft-constraints", "", "comma-separated plain-language constraints (-start), e.g. \"must be there for Christmas\"")
	wait := flag.Bool("wait", false, "block and print the outcome once finalized (-start; dev convenience — real SES intake would not block)")

	requestID := flag.String("request-id", "", "request to signal (-signal)")
	text := flag.String("text", "", "follow-up text to append (-signal)")
	flag.Parse()

	switch {
	case *start:
		var soft []string
		for _, s := range strings.Split(*softConstraintsCSV, ",") {
			if s = strings.TrimSpace(s); s != "" {
				soft = append(soft, s)
			}
		}
		return startRequest(*dbPath, strings.Split(*kafkaBrokers, ","), *origin, *destination, *date, *returnDate, *maxHours, *budget, soft, *wait)
	case *signal:
		return sendFollowUp(*dbPath, *requestID, *text)
	default:
		flag.Usage()
		return fmt.Errorf("one of -start or -signal is required")
	}
}

func startRequest(dbPath string, brokers []string, origin, destination, date, returnDate string, maxHours float64, budget int, soft []string, wait bool) error {
	if origin == "" || destination == "" || date == "" {
		return fmt.Errorf("-origin, -destination, and -date are required with -start")
	}

	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	_, specJSON, err := agents.NewRequest(origin, destination, date, returnDate, maxHours, budget, soft)
	if err != nil {
		return err
	}

	requestID := fmt.Sprintf("travel-request-%s-%s-%s-%d", origin, destination, date, time.Now().UnixNano())
	ctx := context.Background()
	if err := db.CreateAgentRequest(ctx, requestID, specJSON); err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	producer := kafka.NewProducer(brokers, kafka.TopicAgentDecisions)
	defer producer.Close()
	if err := producer.Send(ctx, requestID, kafka.DecisionTrigger{RequestID: requestID}); err != nil {
		return fmt.Errorf("publishing initial decision trigger: %w", err)
	}

	fmt.Printf("Created request %s and published its first decision trigger.\n", requestID)
	fmt.Printf("Send a follow-up with:\n  go run ./cmd/email-intake -signal -request-id %s -text \"...\"\n", requestID)

	if !wait {
		return nil
	}

	fmt.Println("\n-wait set: polling for the outcome (dev convenience only — real SES intake would not block)...")
	for {
		row, err := db.LoadAgentRequest(ctx, requestID)
		if err != nil {
			return err
		}
		if row.Status == agents.StatusFinalized {
			fmt.Printf("\nFinalized (%s):\n\n%s\n", row.FinalizedBy.String, row.EmailBody.String)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func sendFollowUp(dbPath, requestID, text string) error {
	if requestID == "" || text == "" {
		return fmt.Errorf("-request-id and -text are required with -signal")
	}

	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	ctx := context.Background()
	row, err := db.LoadAgentRequest(ctx, requestID)
	if err != nil {
		return err
	}
	updated, err := agents.AppendSoftConstraint([]byte(row.SpecJSON), text)
	if err != nil {
		return err
	}
	if err := db.UpdateAgentRequestSpec(ctx, requestID, updated); err != nil {
		return fmt.Errorf("updating spec: %w", err)
	}

	fmt.Printf("Appended follow-up to %s: %q\n", requestID, text)
	return nil
}

// Command email-intake is DESIGN.md "Where this lives in the repo"'s
// reconciler for internal/agents' agent loop, plus a dev-only CLI standing
// in for SES inbound (DESIGN.md "Local development": "Local dev exposes a
// small HTTP endpoint/CLI that accepts a raw email fixture... this tests
// everything downstream of 'an email arrived'"). No workflow engine here —
// see internal/agents' package doc for why. Three modes:
//
//   - -worker: poll agent_requests and advance whichever ones are ready.
//   - -start: create a new request (stands in for an initial request email).
//   - -signal: append a soft constraint to an existing request (stands in
//     for a reply email on an existing thread).
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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "email-intake:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = common.Load(".env")

	workerMode := flag.Bool("worker", false, "poll agent_requests and advance ready ones")
	start := flag.Bool("start", false, "create a new request (stands in for an initial request email)")
	signal := flag.Bool("signal", false, "append a soft constraint to an existing request (stands in for a reply email)")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite store path")
	pollInterval := flag.Duration("poll-interval", 15*time.Second, "how often to check for ready requests (-worker only)")

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
	case *workerMode:
		return runWorker(*dbPath, *pollInterval)
	case *start:
		var soft []string
		for _, s := range strings.Split(*softConstraintsCSV, ",") {
			if s = strings.TrimSpace(s); s != "" {
				soft = append(soft, s)
			}
		}
		return startRequest(*dbPath, *origin, *destination, *date, *returnDate, *maxHours, *budget, soft, *wait, *pollInterval)
	case *signal:
		return sendFollowUp(*dbPath, *requestID, *text)
	default:
		flag.Usage()
		return fmt.Errorf("one of -worker, -start, or -signal is required")
	}
}

// runWorker is the reconciler poll loop: advance every non-finalized
// request that's actually ready to move, then sleep. See internal/agents'
// package doc for why this — not a workflow engine — is the whole "agent
// loop" runtime.
func runWorker(dbPath string, pollInterval time.Duration) error {
	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	fmt.Printf("email-intake worker: polling %s every %s\n", dbPath, pollInterval)
	ctx := context.Background()

	for {
		rows, err := db.ListActiveAgentRequests(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "email-intake: listing active requests:", err)
			time.Sleep(pollInterval)
			continue
		}
		for _, row := range rows {
			if err := agents.AdvanceRequest(ctx, db, row); err != nil {
				fmt.Fprintf(os.Stderr, "email-intake: advancing %s: %v\n", row.RequestID, err)
			}
		}
		time.Sleep(pollInterval)
	}
}

func startRequest(dbPath, origin, destination, date, returnDate string, maxHours float64, budget int, soft []string, wait bool, pollInterval time.Duration) error {
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

	fmt.Printf("Created request %s. Run `go run ./cmd/email-intake -worker` (if not already running) to advance it.\n", requestID)
	fmt.Printf("Send a follow-up with:\n  go run ./cmd/email-intake -signal -request-id %s -text \"...\"\n", requestID)

	if !wait {
		return nil
	}

	fmt.Println("\n-wait set: polling for the outcome (dev convenience only — real SES intake would not block here)...")
	for {
		row, err := db.LoadAgentRequest(ctx, requestID)
		if err != nil {
			return err
		}
		if row.Status == agents.StatusFinalized {
			fmt.Printf("\nFinalized (%s):\n\n%s\n", row.FinalizedBy.String, row.EmailBody.String)
			return nil
		}
		time.Sleep(pollInterval)
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

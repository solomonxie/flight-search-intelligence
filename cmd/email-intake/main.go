// Command email-intake is a dev-only, one-shot CLI standing in for SES
// inbound until that's actually built (DESIGN.md "Local development").
// Two modes, and — unlike an earlier version of this binary — no
// background loop: each run does one thing and exits, the same way a
// single inbound email would trigger one action, not a persistent
// process. See cmd/agent-worker for where the persistent, always-running
// half of the agent loop actually lives now.
//
//   - -start: create a new request from free text (stands in for an
//     initial request email) — internal/agents.FormSpec turns it into a
//     Spec — and publish the first DecisionTrigger (internal/kafka) so
//     cmd/agent-worker picks it up.
//   - -signal: fold follow-up text into an existing request's Spec
//     (stands in for a reply email on an existing thread), via the same
//     FormSpec call. If the request was parked awaiting a clarifying
//     question (agents.StatusAwaitingUser), also flips it back to
//     "awaiting_decision" and republishes a DecisionTrigger — the one
//     case that needs an explicit nudge, since nothing else is in flight
//     to wake it (DESIGN.md "no message needed" only covers the
//     already-dispatched case).
package main

import (
	"context"
	"encoding/json"
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

	start := flag.Bool("start", false, "create a new request from free text (stands in for an initial request email)")
	signal := flag.Bool("signal", false, "fold follow-up text into an existing request (stands in for a reply email)")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite store path")
	kafkaBrokers := flag.String("kafka-brokers", "localhost:9092", "comma-separated Kafka broker addresses")

	text := flag.String("text", "", "free-text request or follow-up — internal/agents.FormSpec extracts the structured Spec from this")
	wait := flag.Bool("wait", false, "block and print the outcome once finalized or a question is asked (-start; dev convenience — real SES intake would not block)")
	requestID := flag.String("request-id", "", "request to signal (-signal)")
	flag.Parse()

	brokers := strings.Split(*kafkaBrokers, ",")
	switch {
	case *start:
		return startRequest(*dbPath, brokers, *text, *wait)
	case *signal:
		return sendFollowUp(*dbPath, brokers, *requestID, *text)
	default:
		flag.Usage()
		return fmt.Errorf("one of -start or -signal is required")
	}
}

func startRequest(dbPath string, brokers []string, text string, wait bool) error {
	if text == "" {
		return fmt.Errorf("-text is required with -start")
	}

	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	llmClient, err := agents.NewLLMClientFromEnv()
	if err != nil {
		return fmt.Errorf("building LLM client: %w", err)
	}

	ctx := context.Background()
	spec, reasoning, err := agents.FormSpec(ctx, llmClient, agents.Spec{}, text)
	if err != nil {
		return fmt.Errorf("forming spec: %w", err)
	}
	fmt.Printf("Spec formed from text (%s):\n  %+v\n", reasoning, spec)

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("encoding spec: %w", err)
	}

	requestID := fmt.Sprintf("travel-request-%d", time.Now().UnixNano())
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
		switch row.Status {
		case agents.StatusFinalized:
			fmt.Printf("\nFinalized (%s):\n\n%s\n", row.FinalizedBy.String, row.EmailBody.String)
			return nil
		case agents.StatusAwaitingUser:
			fmt.Printf("\nAgent needs more info before continuing:\n\n%s\n\nReply with:\n  go run ./cmd/email-intake -signal -request-id %s -text \"...\"\n", row.EmailBody.String, requestID)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func sendFollowUp(dbPath string, brokers []string, requestID, text string) error {
	if requestID == "" || text == "" {
		return fmt.Errorf("-request-id and -text are required with -signal")
	}

	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	llmClient, err := agents.NewLLMClientFromEnv()
	if err != nil {
		return fmt.Errorf("building LLM client: %w", err)
	}

	ctx := context.Background()
	row, err := db.LoadAgentRequest(ctx, requestID)
	if err != nil {
		return err
	}
	var existing agents.Spec
	if err := json.Unmarshal([]byte(row.SpecJSON), &existing); err != nil {
		return fmt.Errorf("decoding existing spec: %w", err)
	}

	updated, reasoning, err := agents.FormSpec(ctx, llmClient, existing, text)
	if err != nil {
		return fmt.Errorf("forming spec: %w", err)
	}
	fmt.Printf("Spec updated from follow-up (%s):\n  %+v\n", reasoning, updated)

	updatedJSON, err := json.Marshal(updated)
	if err != nil {
		return fmt.Errorf("encoding spec: %w", err)
	}
	if err := db.UpdateAgentRequestSpec(ctx, requestID, updatedJSON); err != nil {
		return fmt.Errorf("updating spec: %w", err)
	}
	fmt.Printf("Appended follow-up to %s: %q\n", requestID, text)

	if row.Status != agents.StatusAwaitingUser {
		return nil // a task's still in flight (or already finalized) — whatever runs next reads the spec fresh, no nudge needed
	}

	if err := db.SaveAgentRequestState(ctx, requestID, agents.StatusAwaitingDecision, []byte(row.RoundsJSON), nil, "", ""); err != nil {
		return fmt.Errorf("reawakening request: %w", err)
	}
	producer := kafka.NewProducer(brokers, kafka.TopicAgentDecisions)
	defer producer.Close()
	if err := producer.Send(ctx, requestID, kafka.DecisionTrigger{RequestID: requestID}); err != nil {
		return fmt.Errorf("publishing decision trigger: %w", err)
	}
	fmt.Println("Request was awaiting a reply to a clarifying question — reawakened and republished its decision trigger.")
	return nil
}

// Command email-intake is a dev-only stand-in for SES inbound until
// that's actually built (DESIGN.md "Local development"). Two shapes:
//
//   - -interactive: a REPL, no flags needed beyond it — type a request,
//     the agent replies (a clarifying question, or the final answer)
//     right there, you type the next line, repeat. One process for the
//     whole conversation, and no other process needed either: it drives
//     agents.Decide and internal/dispatch.RunTask directly, synchronously,
//     skipping Kafka entirely — DESIGN.md's "hands the request off"
//     durability reasoning is about surviving a crash between processes,
//     which doesn't apply to a single terminal already blocked on its
//     own output. Stands in for the shell channel.
//   - -start / -signal: one process per message instead — -start takes
//     -text and creates a request (stands in for an initial request
//     email); -signal takes -request-id and -text and folds a follow-up
//     into an existing one (stands in for a reply email on an existing
//     thread). Each publishes a DecisionTrigger for the real,
//     persistent cmd/agent-worker / cmd/collector -worker to pick up —
//     stands in for the email channel, where a reply is a separate event
//     arriving later, not something to block on.
//
// Both shapes turn free text into a Spec via the same internal/agents.
// FormSpec call, and both hand a clarifying-question round the same way
// — see applyFollowUp, the one piece of logic they share.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/common"
	"flight-search-intelligence/internal/dispatch"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/kafka"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/routesearch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "email-intake:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = common.Load(".env")

	interactive := flag.Bool("interactive", false, "enter a REPL: type a request, the agent replies right here, repeat")
	start := flag.Bool("start", false, "create a new request from -text (stands in for an initial request email)")
	signal := flag.Bool("signal", false, "fold -text into an existing -request-id (stands in for a reply email)")
	dbPath := flag.String("db", "data/flight_search.db", "SQLite store path")
	kafkaBrokers := flag.String("kafka-brokers", "localhost:9092", "comma-separated Kafka broker addresses (-start/-signal only — -interactive doesn't use Kafka)")
	openflightsDir := flag.String("openflights-dir", "data/openflights", "cache dir for the OpenFlights airports/routes dataset (-interactive only)")
	text := flag.String("text", "", "free-text request or follow-up (-start/-signal only — -interactive reads this from stdin instead)")
	wait := flag.Bool("wait", false, "block and print the outcome once finalized or a question is asked (-start; dev convenience — real SES intake would not block)")
	requestID := flag.String("request-id", "", "request to signal (-signal)")
	flag.Parse()

	brokers := strings.Split(*kafkaBrokers, ",")
	switch {
	case *interactive:
		return runREPL(*dbPath, *openflightsDir)
	case *start:
		return startRequest(*dbPath, brokers, *text, *wait)
	case *signal:
		return sendFollowUp(*dbPath, brokers, *requestID, *text)
	default:
		flag.Usage()
		return fmt.Errorf("one of -interactive, -start, or -signal is required")
	}
}

// runREPL is the shell-channel path: an ipython-style loop, not a
// single-shot command, and fully self-contained — no cmd/agent-worker or
// cmd/collector -worker needed in other terminals, since it drives both
// halves of the loop itself (see driveToNextStop). One request for the
// whole session, the same way a reply continues an existing email thread
// rather than starting a new one: the first line creates it, every line
// after — whether it's the answer to a clarifying question or something
// typed once the request has already finalized — is a follow-up on that
// same request (applyFollowUp reopens a finalized one, same as a real
// reply would). Ctrl-D (EOF) or typing "exit"/"quit" ends the session
// cleanly, at any prompt.
func runREPL(dbPath, openflightsDir string) error {
	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	llmClient, err := agents.NewLLMClientFromEnv()
	if err != nil {
		return fmt.Errorf("building LLM client: %w", err)
	}

	graph, err := openflights.Load(openflightsDir)
	if err != nil {
		return fmt.Errorf("loading openflights graph: %w", err)
	}
	deps := routesearch.Deps{
		Flights: googleflights.NewClient(),
		Graph:   graph,
		Catalog: db,
		Logger:  slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: common.LogLevel()})),
	}

	ctx := context.Background()
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Flight-search agent — type a request (Ctrl-D or \"exit\" to leave).")
	var requestID string
	for {
		line, ok := prompt(reader, "\n> ")
		if !ok {
			return nil
		}
		if line == "" {
			continue
		}

		if requestID == "" {
			id, err := createRequest(ctx, db, llmClient, nil, line, false)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			requestID = id
		} else if err := applyFollowUp(ctx, db, llmClient, nil, requestID, line, false); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}

		quit, err := converse(ctx, db, llmClient, deps, reader, requestID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		if quit {
			return nil
		}
	}
}

// converse runs one request through to completion: drive it as far as it
// goes (driveToNextStop), and whenever it's parked awaiting a clarifying
// question, prompt for the answer with the same reader/style the
// top-level REPL uses, fold it in via applyFollowUp, and drive again.
// Returns once finalized, or quit=true if the user left (Ctrl-D or
// "exit"/"quit") while answering — that ends the whole REPL, not just
// this one conversation, the same as typing it at the top-level prompt.
func converse(ctx context.Context, db *catalog.SQLite, llmClient agents.LLMClient, deps routesearch.Deps, reader *bufio.Reader, requestID string) (quit bool, err error) {
	for {
		if err := driveToNextStop(ctx, db, llmClient, deps, requestID); err != nil {
			return false, err
		}
		row, err := db.LoadAgentRequest(ctx, requestID)
		if err != nil {
			return false, err
		}
		switch row.Status {
		case agents.StatusFinalized:
			fmt.Printf("\nAgent: %s\n", row.EmailBody.String)
			return false, nil
		case agents.StatusAwaitingUser:
			answer, ok := prompt(reader, fmt.Sprintf("\nAgent: %s\n> ", row.EmailBody.String))
			if !ok {
				return true, nil
			}
			if err := applyFollowUp(ctx, db, llmClient, nil, requestID, answer, false); err != nil {
				return false, err
			}
			// loop: driveToNextStop runs again now that the request is
			// back in awaiting_decision
		default:
			return false, fmt.Errorf("unexpected status %q for %s after driving it — driveToNextStop should never return with a task still in flight", row.Status, requestID)
		}
	}
}

// driveToNextStop is -interactive's no-Kafka replacement for
// cmd/agent-worker + cmd/collector -worker: call agents.Decide, and
// whenever it dispatches, run that task immediately via
// internal/dispatch.RunTask (the exact same function cmd/collector calls
// from its Kafka handler) instead of publishing a message for some other
// process to pick up. Loops until Decide stops dispatching — i.e. the
// request is awaiting_user or finalized.
func driveToNextStop(ctx context.Context, db *catalog.SQLite, llmClient agents.LLMClient, deps routesearch.Deps, requestID string) error {
	for {
		taskID, dispatched, err := agents.Decide(ctx, llmClient, db, requestID)
		if err != nil {
			return fmt.Errorf("deciding: %w", err)
		}
		if !dispatched {
			return nil
		}
		fmt.Print("Searching")
		if _, err := dispatch.RunTask(ctx, db, deps, taskID); err != nil {
			fmt.Println()
			return fmt.Errorf("running task %s: %w", taskID, err)
		}
		fmt.Println()
	}
}

// prompt prints label, reads one line, and reports ok=false on EOF
// (Ctrl-D) or "exit"/"quit" — checked here, once, so leaving works the
// same way at both callers (the top-level REPL loop and converse's
// mid-conversation answer prompt) instead of only being honored at the
// top level.
func prompt(reader *bufio.Reader, label string) (line string, ok bool) {
	fmt.Print(label)
	raw, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "error reading input:", err)
		return "", false
	}
	line = strings.TrimSpace(raw)
	if (err == io.EOF && line == "") || line == "exit" || line == "quit" {
		fmt.Println()
		return "", false
	}
	return line, true
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
	requestID, err := createRequest(ctx, db, llmClient, brokers, text, true)
	if err != nil {
		return err
	}

	fmt.Printf("Send a follow-up with:\n  go run ./cmd/email-intake -signal -request-id %s -text \"...\"\n", requestID)
	if !wait {
		return nil
	}

	fmt.Println("\n-wait set: polling for the outcome (dev convenience only — real SES intake would not block; needs cmd/agent-worker and cmd/collector -worker running elsewhere)...")
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

// createRequest is the one piece of logic runREPL and startRequest
// share: form a Spec from text via agents.FormSpec and create the
// agent_requests row. notify controls whether it also publishes a
// DecisionTrigger — true for -start (a real, separate cmd/agent-worker
// needs the nudge), false for -interactive (which drives the decision
// itself right after, via driveToNextStop — publishing here too would
// just risk a second process racing it, if one happened to be running).
func createRequest(ctx context.Context, db *catalog.SQLite, llmClient agents.LLMClient, brokers []string, text string, notify bool) (requestID string, err error) {
	spec, reasoning, err := agents.FormSpec(ctx, llmClient, agents.Spec{}, text)
	if err != nil {
		return "", fmt.Errorf("forming spec: %w", err)
	}
	fmt.Printf("Spec formed from text (%s):\n  %+v\n", reasoning, spec)

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encoding spec: %w", err)
	}

	requestID = fmt.Sprintf("travel-request-%d", time.Now().UnixNano())
	if err := db.CreateAgentRequest(ctx, requestID, specJSON); err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	fmt.Printf("Created request %s.\n", requestID)

	if !notify {
		return requestID, nil
	}
	producer := kafka.NewProducer(brokers, kafka.TopicAgentDecisions)
	defer producer.Close()
	if err := producer.Send(ctx, requestID, kafka.DecisionTrigger{RequestID: requestID}); err != nil {
		return "", fmt.Errorf("publishing initial decision trigger: %w", err)
	}
	fmt.Println("Published its first decision trigger.")
	return requestID, nil
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

	return applyFollowUp(context.Background(), db, llmClient, brokers, requestID, text, true)
}

// applyFollowUp is the one piece of logic shared by -signal (the
// email-channel stand-in: one process per reply), converse (a
// clarifying-question answer), and runREPL (a line typed once the
// request already finalized — the shell-channel's "reply to the same
// thread" case). Folds text into the request's spec via agents.FormSpec,
// and reawakens it (StatusAwaitingUser or StatusFinalized — anything
// else already has something in flight that reads the spec fresh on its
// own, so no nudge needed). notify controls whether reawakening also
// publishes a DecisionTrigger — see createRequest's doc for why
// -interactive passes false (it drives the next step itself).
func applyFollowUp(ctx context.Context, db *catalog.SQLite, llmClient agents.LLMClient, brokers []string, requestID, text string, notify bool) error {
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

	if row.Status != agents.StatusAwaitingUser && row.Status != agents.StatusFinalized {
		return nil // a task's still in flight — whatever runs next reads the spec fresh, no nudge needed
	}

	wasFinalized := row.Status == agents.StatusFinalized
	if err := db.SaveAgentRequestState(ctx, requestID, agents.StatusAwaitingDecision, []byte(row.RoundsJSON), nil, "", ""); err != nil {
		return fmt.Errorf("reawakening request: %w", err)
	}
	if wasFinalized {
		fmt.Println("(request had already finalized — reopened as a follow-up; note the redispatch-round budget isn't reset on reopen, so a request that hit its cap the first time round will likely hit it again immediately)")
	}
	if !notify {
		return nil
	}
	producer := kafka.NewProducer(brokers, kafka.TopicAgentDecisions)
	defer producer.Close()
	if err := producer.Send(ctx, requestID, kafka.DecisionTrigger{RequestID: requestID}); err != nil {
		return fmt.Errorf("publishing decision trigger: %w", err)
	}
	fmt.Println("Reawakened and republished its decision trigger.")
	return nil
}

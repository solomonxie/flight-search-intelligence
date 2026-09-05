// Command agent-worker is the "decide what to do next" half of the agent
// loop (DESIGN.md "Agent loop"). It consumes internal/kafka's
// agent-decisions topic — one tiny message per request that's ready for a
// decision — and calls internal/agents.Decide. If that decides to
// dispatch a new search, it pushes the resulting task onto the
// search-tasks topic for cmd/collector to pick up; if it finalizes, it
// pushes nothing further, which is the whole "stop" signal (see
// internal/kafka's package doc).
//
// This replaced an earlier version of this binary (cmd/email-intake
// -worker) that polled the database on a timer instead of reacting to
// Kafka messages — see DESIGN.md "Agent loop" for why that was worth
// fixing: a poll loop only notices a change up to one interval late, and
// needs to keep checking even when nothing's happening. cmd/email-intake
// itself is back to being what its name says: a one-shot CLI standing in
// for an email arriving, not a background process.
package main

import (
	"context"
	"fmt"
	"os"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/common"
	"flight-search-intelligence/internal/kafka"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = common.Load(".env")

	dbPath := envOr("DB_PATH", "data/flight_search.db")
	brokers := []string{envOr("KAFKA_BROKERS", "localhost:9092")}

	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	consumer := kafka.NewConsumer(brokers, kafka.TopicAgentDecisions, "agent-worker")
	defer consumer.Close()
	producer := kafka.NewProducer(brokers, kafka.TopicSearchTasks)
	defer producer.Close()

	fmt.Printf("agent-worker: consuming %q at %v, db %s\n", kafka.TopicAgentDecisions, brokers, dbPath)
	ctx := context.Background()

	for {
		var trig kafka.DecisionTrigger
		commit, err := consumer.Next(ctx, &trig)
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent-worker: reading message:", err)
			continue // a decode failure still returns a commit func (see internal/kafka); anything else, just retry
		}

		taskID, dispatched, err := agents.Decide(ctx, db, trig.RequestID)
		if err != nil {
			// Deliberately not committed: this request's decision didn't
			// actually get made, so the message is left for a retry
			// (redelivered to this consumer group after a restart/rebalance)
			// rather than silently treated as handled.
			fmt.Fprintf(os.Stderr, "agent-worker: deciding for %s: %v\n", trig.RequestID, err)
			continue
		}

		if dispatched {
			if err := producer.Send(ctx, taskID, kafka.SearchTaskTrigger{TaskID: taskID}); err != nil {
				fmt.Fprintf(os.Stderr, "agent-worker: publishing task %s: %v\n", taskID, err)
				continue // same reasoning: don't commit a decision whose consequence didn't make it onto the queue
			}
		}

		if err := commit(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "agent-worker: committing message:", err)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

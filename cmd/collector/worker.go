package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/common"
	"flight-search-intelligence/internal/dispatch"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/kafka"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/routesearch"
)

// runWorker consumes internal/kafka's search-tasks topic — one tiny
// message per task that's ready to run — and does the actual fetch. When
// it's done, it folds the result into the task's request
// (agents.RecordTaskResult) and pushes a DecisionTrigger back onto
// agent-decisions, waking cmd/agent-worker's next round. See DESIGN.md
// "Agent loop" and internal/kafka's package doc for why steps chain this
// way instead of a poll loop checking the database on a timer.
func runWorker(dbPath, openflightsDir string, brokers []string, concurrency int) error {
	graph, err := openflights.Load(openflightsDir)
	if err != nil {
		return fmt.Errorf("loading openflights graph: %w", err)
	}
	db, err := catalog.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()

	deps := routesearch.Deps{
		Flights: googleflights.NewClient(),
		Graph:   graph,
		Catalog: db,
		Logger:  slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: common.LogLevel()})),
	}

	consumer := kafka.NewConsumer(brokers, kafka.TopicSearchTasks, "collector")
	defer consumer.Close()
	producer := kafka.NewProducer(brokers, kafka.TopicAgentDecisions)
	defer producer.Close()

	fmt.Printf("collector worker: consuming %q at %v (concurrency %d)\n", kafka.TopicSearchTasks, brokers, concurrency)
	ctx := context.Background()
	sem := make(chan struct{}, concurrency)

	for {
		var trig kafka.SearchTaskTrigger
		commit, err := consumer.Next(ctx, &trig)
		if err != nil {
			fmt.Fprintln(os.Stderr, "collector: reading message:", err)
			continue
		}

		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			runTask(ctx, db, deps, producer, trig.TaskID)
			if err := commit(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "collector: committing message:", err)
			}
		}()
	}
}

// runTask executes one task (a real, possibly slow scrape — see
// internal/routesearch's own pacing) via internal/dispatch.RunTask —
// shared with cmd/email-intake -interactive's synchronous, no-Kafka
// path — and, regardless of success or failure, pushes the request's
// next DecisionTrigger so the agent loop always hears back, never stalls
// on a failed task.
func runTask(ctx context.Context, db *catalog.SQLite, deps routesearch.Deps, producer *kafka.Producer, taskID string) {
	requestID, err := dispatch.RunTask(ctx, db, deps, taskID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "collector: running task %s: %v\n", taskID, err)
		return
	}
	if err := producer.Send(ctx, requestID, kafka.DecisionTrigger{RequestID: requestID}); err != nil {
		fmt.Fprintf(os.Stderr, "collector: publishing decision trigger for %s: %v\n", requestID, err)
	}
}

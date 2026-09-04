package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/routesearch"
)

// staleClaimLease is how long a claimed agent_tasks row can go without
// finishing before a sweep assumes its worker died and puts it back to
// "pending" — the poll-based stand-in for a Temporal Activity's own
// timeout+retry.
const staleClaimLease = 45 * time.Minute

// runWorker polls agent_tasks for pending work and runs it — see DESIGN.md
// "Agent loop" and internal/agents' package doc for why this is a plain
// poll loop, not a Temporal worker. concurrency caps how many fetches run
// at once, standing in for Temporal's per-task-queue concurrency limit
// (DESIGN.md "Rate limiting / politeness toward providers").
func runWorker(dbPath, openflightsDir string, pollInterval time.Duration, concurrency int) error {
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
		Logger:  slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}

	workerID := fmt.Sprintf("collector-%d", os.Getpid())
	fmt.Printf("collector worker %s: polling %s every %s (concurrency %d)\n", workerID, dbPath, pollInterval, concurrency)

	sem := make(chan struct{}, concurrency)
	ctx := context.Background()

	for {
		if n, err := db.ReapStaleAgentTaskClaims(ctx, staleClaimLease); err != nil {
			fmt.Fprintln(os.Stderr, "collector: reaping stale claims:", err)
		} else if n > 0 {
			fmt.Printf("collector: reclaimed %d stale task(s)\n", n)
		}

		task, ok, err := db.ClaimPendingAgentTask(ctx, workerID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "collector: claiming task:", err)
			time.Sleep(pollInterval)
			continue
		}
		if !ok {
			time.Sleep(pollInterval)
			continue
		}

		sem <- struct{}{}
		go func(t catalog.AgentTaskRow) {
			defer func() { <-sem }()
			runTask(ctx, db, deps, t)
		}(task)
	}
}

// runTask executes one claimed task (a real, possibly slow scrape — see
// internal/routesearch's own pacing) in its own goroutine, async from the
// claim loop above, and writes its result back when done — never blocking
// the poller from claiming other tasks meanwhile.
func runTask(ctx context.Context, db *catalog.SQLite, deps routesearch.Deps, task catalog.AgentTaskRow) {
	var req agents.CollectRouteRequest
	if err := json.Unmarshal([]byte(task.ParamsJSON), &req); err != nil {
		saveFailure(ctx, db, task.TaskID, fmt.Errorf("decoding task params: %w", err))
		return
	}

	result, err := fetchFare(ctx, deps, req)
	if err != nil {
		saveFailure(ctx, db, task.TaskID, err)
		return
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		saveFailure(ctx, db, task.TaskID, fmt.Errorf("encoding result: %w", err))
		return
	}
	if err := db.SaveAgentTaskResult(ctx, task.TaskID, "done", resultJSON, ""); err != nil {
		fmt.Fprintln(os.Stderr, "collector: saving task result:", err)
	}
}

func saveFailure(ctx context.Context, db *catalog.SQLite, taskID string, err error) {
	fmt.Fprintf(os.Stderr, "collector: task %s failed: %v\n", taskID, err)
	if saveErr := db.SaveAgentTaskResult(ctx, taskID, "failed", nil, err.Error()); saveErr != nil {
		fmt.Fprintln(os.Stderr, "collector: saving task failure:", saveErr)
	}
}

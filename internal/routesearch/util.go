package routesearch

import (
	"context"
	"encoding/json"
	"time"
)

// sleepPacing is a stand-in for Temporal's durable timer (see DESIGN.md
// "Pacing") — a plain sleep between scrapes, cancellable via ctx.
func sleepPacing(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// withinBudget reports whether used more scrapes is still allowed under
// budget. budget <= 0 means unlimited (exhaustive) — see Params.
// QueryBudget: this is the project's core selling point (README "Why
// this beats a plain flight search"), so "no budget set" must mean "keep
// going," not "stop now."
func withinBudget(used, budget int) bool {
	return budget <= 0 || used < budget
}

// mustJSON marshals v for the audit trail; a marshal failure becomes a
// small error document rather than a panic, since a failed encode of the
// plan shouldn't crash the search itself.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":"marshal failed"}`)
	}
	return b
}

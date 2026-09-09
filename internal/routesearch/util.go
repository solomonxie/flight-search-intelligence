package routesearch

import (
	"context"
	"encoding/json"
	"time"

	"flight-search-intelligence/internal/tracefile"
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

// savePlan upserts plan (a *Plan or *FlexiblePlan) to the DB audit-trail
// row every entry point already keeps, and — once status is no longer
// the initial "running" checkpoint — also exports it to a plain trace
// file (DESIGN.md "a trace file"): the same audit data, grep/jq-
// inspectable on disk, not written on every in-flight progress
// checkpoint, only once a search/scan actually reaches a final status.
// Both writes are best-effort: neither failing should fail the search
// itself, same as before this helper existed.
func savePlan(ctx context.Context, deps Deps, requestID, status string, plan any) {
	if err := deps.Catalog.SaveRouteSearchPlan(ctx, requestID, status, mustJSON(plan)); err != nil {
		deps.Logger.Warn("saving route search plan failed", "request_id", requestID, "error", err)
	}
	if status == "running" {
		return
	}
	if _, err := tracefile.Write(requestID, plan); err != nil {
		deps.Logger.Warn("writing trace file failed", "request_id", requestID, "error", err)
	}
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

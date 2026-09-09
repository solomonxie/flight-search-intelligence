// Package ratelimit is DESIGN.md "a shared, process-wide rate limiter":
// routesearch.Params.Delay/sleepPacing already spaces out one request's
// own scrapes, but cmd/collector -worker's goroutine pool runs several
// different requests' searches concurrently, each pacing only itself —
// real scrape throughput then scales with worker concurrency, not with
// Delay. A Limiter shared by every googleflights.Client in a process
// closes that gap at the one real HTTP call site, independent of how
// many goroutines are calling it.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter paces callers against a shared resource. Wait blocks until the
// caller may proceed, or returns ctx's error if ctx is done first.
type Limiter interface {
	Wait(ctx context.Context) error
}

// Window is one granularity's cap: at most Max calls within any trailing
// Per-long span.
type Window struct {
	Per time.Duration
	Max int
}

// FixedWindow enforces every Window given, independently, behind one
// mutex-guarded counter set — a call proceeds only once every window has
// room, and is recorded against all of them at once. ("Fixed window" per
// DESIGN.md's naming; the counters themselves are trailing/sliding, not
// reset on a wall-clock boundary, so a caller never sees a burst at a
// window edge the way a true fixed-window counter would allow.)
type FixedWindow struct {
	mu      sync.Mutex
	windows []Window
	// calls[i] holds windows[i]'s own recorded call timestamps, oldest
	// first, pruned lazily on each Wait.
	calls [][]time.Time
	// now is time.Now, overridable so a test can control the clock
	// instead of racing real wall-clock sleeps.
	now func() time.Time
}

// NewFixedWindow builds a FixedWindow enforcing every window given.
func NewFixedWindow(windows ...Window) *FixedWindow {
	return &FixedWindow{windows: windows, calls: make([][]time.Time, len(windows)), now: time.Now}
}

// Wait blocks until every window has room for one more call, records
// this call against all of them, and returns. Returns ctx.Err() if ctx
// is cancelled while waiting.
func (f *FixedWindow) Wait(ctx context.Context) error {
	for {
		wait, ready := f.tryAcquire()
		if ready {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// tryAcquire prunes every window's expired timestamps, checks whether
// all currently have room, and — if so — records this call against
// every window in the same locked section (so two goroutines can never
// both observe room and both proceed for what should have been one
// slot). Returns how long to wait before retrying when not ready.
func (f *FixedWindow) tryAcquire() (wait time.Duration, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	for i, w := range f.windows {
		f.calls[i] = pruneBefore(f.calls[i], now.Add(-w.Per))
		if len(f.calls[i]) >= w.Max {
			if d := f.calls[i][0].Add(w.Per).Sub(now); d > wait {
				wait = d
			}
		}
	}
	if wait > 0 {
		return wait, false
	}
	for i := range f.windows {
		f.calls[i] = append(f.calls[i], now)
	}
	return 0, true
}

// pruneBefore drops every timestamp strictly before cutoff — times is
// kept oldest-first, so this is a prefix trim, not a full re-scan.
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

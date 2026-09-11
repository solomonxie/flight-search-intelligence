package ratelimit

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestFixedWindow_EnforcesMax(t *testing.T) {
	lim := NewFixedWindow(Window{Per: 100 * time.Millisecond, Max: 2})
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := lim.Wait(ctx); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("3rd call under Max=2/Per=100ms returned after %v, want >= 100ms", elapsed)
	}
}

func TestFixedWindow_MultipleWindowsIndependent(t *testing.T) {
	// The short window never binds (Max 100); the long one caps at 2 —
	// every window given must have room, not just the tightest one.
	lim := NewFixedWindow(
		Window{Per: 20 * time.Millisecond, Max: 100},
		Window{Per: 200 * time.Millisecond, Max: 2},
	)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := lim.Wait(ctx); err != nil {
			t.Fatalf("Wait #%d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("3rd call under the 2-per-200ms window returned after %v, want >= 200ms", elapsed)
	}
}

func TestFixedWindow_ContextCancel(t *testing.T) {
	lim := NewFixedWindow(Window{Per: time.Second, Max: 1})
	if err := lim.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lim.Wait(cctx); err == nil {
		t.Fatal("Wait blocked on an exhausted window with a cancelled ctx returned nil, want ctx.Err()")
	}
}

// TestFixedWindow_ConcurrentRespectsMax exercises the mutex-guarded
// tryAcquire under real concurrency (run with -race) — every window's
// cap must hold even when many goroutines call Wait at once, not just
// under sequential calls.
func TestFixedWindow_ConcurrentRespectsMax(t *testing.T) {
	const per = 50 * time.Millisecond
	const max = 3
	lim := NewFixedWindow(Window{Per: per, Max: max})
	ctx := context.Background()

	const n = 15
	var mu sync.Mutex
	var timestamps []time.Time
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := lim.Wait(ctx); err != nil {
				t.Errorf("Wait: %v", err)
				return
			}
			mu.Lock()
			timestamps = append(timestamps, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(timestamps) != n {
		t.Fatalf("got %d completed calls, want %d", len(timestamps), n)
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i].Before(timestamps[j]) })
	for i := 0; i+max < len(timestamps); i++ {
		if d := timestamps[i+max].Sub(timestamps[i]); d < per {
			t.Errorf("calls %d and %d (max+1 apart) are only %v apart, want >= %v (Max=%d per %v violated)", i, i+max, d, per, max, per)
		}
	}
}

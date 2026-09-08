package routesearch

// Tests for SearchDateRange — the independent depart/return range grid
// search (DESIGN.md's originally-deferred "combined" flexible-date
// case) — against a fixture-backed Google Flights transport. Reuses
// testDeps/syntheticOfferHTML from nhop_test.go (same package).

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
)

// indexedPriceTransport serves a synthetic offer per call, with the
// price determined by that call's own index (0-based, in request
// order) — lets a test pin an exact price to an exact (depart, return)
// combination, since SearchDateRange's Phase A enumerates combinations
// in a fixed, deterministic order (depart ranges outer, return ranges
// inner, both ascending).
type indexedPriceTransport struct {
	mu    sync.Mutex
	n     int
	price func(callIndex int) int
}

func (t *indexedPriceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	i := t.n
	t.n++
	t.mu.Unlock()
	body := syntheticOfferHTML(t.price(i), 90, i)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), Request: req}, nil
}

// TestSearchDateRange_EligibilityExcludesCheaperIneligibleCombo is the
// direct test of the feature a "1 month of paid leave" request needs:
// AvailableFrom/AvailableUntil (round_trip_date_range, at the agent-loop
// layer) must actually keep the search from picking a combination that
// violates it, even when that combination is the single cheapest one
// priced. Phase A here tries 3 depart dates x 3 return dates (9 combos,
// call order fixed and known); the absolute cheapest ($50, call 0) sits
// outside the eligibility window, so the real answer should be the
// cheapest *eligible* one instead ($200, call 4) — not the excluded $50,
// and not one of the other, pricier eligible/ineligible combos.
func TestSearchDateRange_EligibilityExcludesCheaperIneligibleCombo(t *testing.T) {
	transport := &indexedPriceTransport{price: func(i int) int {
		switch i {
		case 0:
			return 50 // cheapest overall — but outside AvailableFrom/AvailableUntil below
		case 4:
			return 200 // cheapest *eligible* combo — the expected winner
		default:
			return 500
		}
	}}
	deps := testDeps(t, transport)

	plan, err := SearchDateRange(context.Background(), deps, DateRangeParams{
		Base: Params{
			Origin: "YVR", Destination: "PEK",
			MaxHours: 1e6, QueryBudget: 50, MinLayoverMinutes: 0, MaxLayoverMinutes: 1e7,
			PricePerMile: 0.08,
		},
		RoundTrip:      true,
		DepartFrom:     "2026-12-10",
		DepartTo:       "2026-12-12",
		ReturnFrom:     "2027-01-10",
		ReturnTo:       "2027-01-12",
		StepDays:       1,
		AvailableFrom:  "2026-12-11", // excludes every combo departing 2026-12-10
		AvailableUntil: "2027-01-11", // excludes every combo returning 2027-01-12
	})
	if err != nil {
		t.Fatalf("SearchDateRange: %v", err)
	}

	if plan.ChosenDepartDate != "2026-12-11" || plan.ChosenReturnDate != "2027-01-11" {
		t.Errorf("chosen = %s/%s, want 2026-12-11/2027-01-11 (the cheapest *eligible* combo, not the cheapest overall)",
			plan.ChosenDepartDate, plan.ChosenReturnDate)
	}

	// The excluded-but-cheaper combo must still show up in the audit
	// trail (priced, just not eligible to win) — same "still shown for
	// context" behavior as the coupled-window case.
	var sawExcludedCheapest bool
	for _, e := range plan.DateScan {
		if e.DepartDate == "2026-12-10" && e.ReturnDate == "2027-01-10" {
			if e.PriceUSD != 50 {
				t.Errorf("entry for the cheapest combo has PriceUSD = %v, want 50", e.PriceUSD)
			}
			if e.Excluded == "" {
				t.Error("the cheapest combo (outside AvailableFrom) should be marked Excluded, not eligible")
			}
			sawExcludedCheapest = true
		}
	}
	if !sawExcludedCheapest {
		t.Fatal("never saw a DateScan entry for 2026-12-10/2027-01-10 at all")
	}
}

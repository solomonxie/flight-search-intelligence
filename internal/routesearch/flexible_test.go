package routesearch

// Tests for SearchFlexible's Phase 5 additions: the trip-length
// tolerance range, the explicit DepartFrom/DepartTo window as an
// alternative to center+WindowDays, and the QueryBudget guard Phase A
// never had before. Reuses testDeps/syntheticOfferHTML (nhop_test.go)
// and indexedPriceTransport (daterange_test.go), same package.

import (
	"context"
	"testing"
)

// TestSearchFlexible_TripLengthRangeTriesEveryLength verifies
// TripLengthDays..TripLengthMaxDays actually enumerates every length in
// range, not just the single TripLengthDays value — one depart date, 3
// candidate trip lengths (7/8/9 days), the middle one cheapest.
func TestSearchFlexible_TripLengthRangeTriesEveryLength(t *testing.T) {
	transport := &indexedPriceTransport{price: func(i int) int {
		if i == 1 {
			return 100 // trip length 8 (the middle of 7..9) — expected winner
		}
		return 500
	}}
	deps := testDeps(t, transport)

	plan, err := SearchFlexible(context.Background(), deps, FlexibleParams{
		Base: Params{
			Origin: "YVR", Destination: "PEK", DepartDate: "2026-12-10",
			MaxHours: 1e6, QueryBudget: 50, MinLayoverMinutes: 0, MaxLayoverMinutes: 1e7,
			PricePerMile: 0.08,
		},
		RoundTrip: true, WindowDays: 0, StepDays: 1,
		TripLengthDays: 7, TripLengthMaxDays: 9, TripLengthStepDays: 1,
	})
	if err != nil {
		t.Fatalf("SearchFlexible: %v", err)
	}
	if len(plan.DateScan) != 3 {
		t.Fatalf("len(DateScan) = %d, want 3 (one per trip length 7/8/9)", len(plan.DateScan))
	}
	if plan.ChosenReturnDate != "2026-12-18" { // 2026-12-10 + 8 days
		t.Errorf("ChosenReturnDate = %s, want 2026-12-18 (depart + 8-day trip length, the cheapest)", plan.ChosenReturnDate)
	}
}

// TestSearchFlexible_ExplicitDepartWindowOverridesCenter verifies
// DepartFrom/DepartTo is used instead of Base.DepartDate+WindowDays once
// set, even though Base.DepartDate is itself a plausible (but
// out-of-window) date.
func TestSearchFlexible_ExplicitDepartWindowOverridesCenter(t *testing.T) {
	transport := &indexedPriceTransport{price: func(i int) int { return 100 }}
	deps := testDeps(t, transport)

	plan, err := SearchFlexible(context.Background(), deps, FlexibleParams{
		Base: Params{
			Origin: "YVR", Destination: "PEK", DepartDate: "2026-01-01", // deliberately outside DepartFrom/DepartTo below
			MaxHours: 1e6, QueryBudget: 50, MinLayoverMinutes: 0, MaxLayoverMinutes: 1e7,
			PricePerMile: 0.08,
		},
		DepartFrom: "2026-12-10", DepartTo: "2026-12-12", StepDays: 1,
	})
	if err != nil {
		t.Fatalf("SearchFlexible: %v", err)
	}
	if len(plan.DateScan) != 3 {
		t.Fatalf("len(DateScan) = %d, want 3 (2026-12-10..12)", len(plan.DateScan))
	}
	for _, want := range []string{"2026-12-10", "2026-12-11", "2026-12-12"} {
		var found bool
		for _, e := range plan.DateScan {
			if e.DepartDate == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DateScan missing expected depart date %s", want)
		}
	}
}

// TestSearchFlexible_QueryBudgetTruncatesScan proves Phase A now actually
// stops once QueryBudget scrapes are spent, rather than pricing every
// date in a wide window regardless (the gap IMPLEMENTATION_PLAN.md's
// Phase 5 called out — SearchDateRange already had this guard,
// SearchFlexible didn't).
func TestSearchFlexible_QueryBudgetTruncatesScan(t *testing.T) {
	transport := &indexedPriceTransport{price: func(i int) int { return 100 }}
	deps := testDeps(t, transport)

	plan, err := SearchFlexible(context.Background(), deps, FlexibleParams{
		Base: Params{
			Origin: "YVR", Destination: "PEK", DepartDate: "2026-12-15",
			MaxHours: 1e6, QueryBudget: 2, MinLayoverMinutes: 0, MaxLayoverMinutes: 1e7,
			PricePerMile: 0.08,
		},
		WindowDays: 10, StepDays: 1, ScanOnly: true, // 21 candidate dates without a budget cap
	})
	if err != nil {
		t.Fatalf("SearchFlexible: %v", err)
	}
	if len(plan.DateScan) != 2 {
		t.Fatalf("len(DateScan) = %d, want 2 (QueryBudget should have truncated the 21-date window)", len(plan.DateScan))
	}
}

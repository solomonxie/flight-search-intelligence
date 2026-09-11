package agents

import "testing"

// TestValidateDates is a regression test: a live run had FormSpec's LLM
// call resolve "next jan" (today in September) to *this* January
// instead of next year's, landing ReturnDate almost a year before
// DepartDate — the resulting round-trip request still dispatched,
// burning dozens of queries (every multi-airport candidate pair, both
// directions) on a search that could never succeed before finalizing a
// literally backwards "round trip" anyway. validateDates is the
// mechanical backstop: whatever the model computes, a return window on
// or before the departure window never reaches dispatch, and a reversed
// range gets swapped back into order.
func TestValidateDates(t *testing.T) {
	tests := []struct {
		name           string
		in             Spec
		wantReturnFrom string
		wantReturnTo   string
		wantDepartFrom string
		wantDepartTo   string
		wantNote       bool
	}{
		{
			name:           "return before depart is cleared",
			in:             Spec{TripType: "round_trip", MinDepartDate: "2026-12-15", MaxDepartDate: "2026-12-15", MinReturnDate: "2026-01-01", MaxReturnDate: "2026-01-01"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "", wantReturnTo: "",
			wantNote: true,
		},
		{
			name:           "return equal to depart is cleared",
			in:             Spec{TripType: "round_trip", MinDepartDate: "2026-12-15", MaxDepartDate: "2026-12-15", MinReturnDate: "2026-12-15", MaxReturnDate: "2026-12-15"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "", wantReturnTo: "",
			wantNote: true,
		},
		{
			name:           "return after depart is untouched",
			in:             Spec{TripType: "round_trip", MinDepartDate: "2026-12-15", MaxDepartDate: "2026-12-15", MinReturnDate: "2026-12-22", MaxReturnDate: "2026-12-22"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "2026-12-22", wantReturnTo: "2026-12-22",
			wantNote: false,
		},
		{
			name:           "one-way with no return date is untouched",
			in:             Spec{TripType: "one_way", MinDepartDate: "2026-12-15", MaxDepartDate: "2026-12-15"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "", wantReturnTo: "",
			wantNote: false,
		},
		{
			name:     "round trip missing dates entirely is untouched (nothing to validate yet)",
			in:       Spec{TripType: "round_trip"},
			wantNote: false,
		},
		{
			name:           "a reversed departure range is swapped back into order",
			in:             Spec{MinDepartDate: "2026-12-31", MaxDepartDate: "2026-12-15"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-31",
			wantNote: false,
		},
	}
	// A return-date range and a trip-length range are mutually exclusive
	// (see Spec.MinTripLengthDays' doc) — validateDates keeps the more
	// specific return-date range and clears the trip-length range,
	// noting why.
	t.Run("a return-date range and a trip-length range together clears the trip-length range", func(t *testing.T) {
		got := validateDates(Spec{
			TripType:          "round_trip",
			MinDepartDate:     "2026-12-15", MaxDepartDate: "2026-12-15",
			MinReturnDate:     "2026-12-22", MaxReturnDate: "2026-12-22",
			MinTripLengthDays: 7, MaxTripLengthDays: 9, TripLengthStepDays: 2,
		})
		if got.MinReturnDate != "2026-12-22" || got.MaxReturnDate != "2026-12-22" {
			t.Errorf("MinReturnDate/MaxReturnDate = %q/%q, want kept as-is", got.MinReturnDate, got.MaxReturnDate)
		}
		if got.MinTripLengthDays != 0 || got.MaxTripLengthDays != 0 || got.TripLengthStepDays != 0 {
			t.Errorf("trip-length fields = %d/%d/%d, want all cleared to 0", got.MinTripLengthDays, got.MaxTripLengthDays, got.TripLengthStepDays)
		}
		if len(got.Notes) == 0 {
			t.Error("Notes is empty, want a note explaining the trip-length range was cleared as redundant")
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateDates(tt.in)
			if got.MinDepartDate != tt.wantDepartFrom || got.MaxDepartDate != tt.wantDepartTo {
				t.Errorf("MinDepartDate/MaxDepartDate = %q/%q, want %q/%q", got.MinDepartDate, got.MaxDepartDate, tt.wantDepartFrom, tt.wantDepartTo)
			}
			if got.MinReturnDate != tt.wantReturnFrom || got.MaxReturnDate != tt.wantReturnTo {
				t.Errorf("MinReturnDate/MaxReturnDate = %q/%q, want %q/%q", got.MinReturnDate, got.MaxReturnDate, tt.wantReturnFrom, tt.wantReturnTo)
			}
			if hasNote := len(got.Notes) > 0; hasNote != tt.wantNote {
				t.Errorf("Notes = %v, want a note: %v", got.Notes, tt.wantNote)
			}
		})
	}
}

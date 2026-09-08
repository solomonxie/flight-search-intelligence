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
			in:             Spec{TripType: "round_trip", DepartDateFrom: "2026-12-15", DepartDateTo: "2026-12-15", ReturnDateFrom: "2026-01-01", ReturnDateTo: "2026-01-01"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "", wantReturnTo: "",
			wantNote: true,
		},
		{
			name:           "return equal to depart is cleared",
			in:             Spec{TripType: "round_trip", DepartDateFrom: "2026-12-15", DepartDateTo: "2026-12-15", ReturnDateFrom: "2026-12-15", ReturnDateTo: "2026-12-15"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "", wantReturnTo: "",
			wantNote: true,
		},
		{
			name:           "return after depart is untouched",
			in:             Spec{TripType: "round_trip", DepartDateFrom: "2026-12-15", DepartDateTo: "2026-12-15", ReturnDateFrom: "2026-12-22", ReturnDateTo: "2026-12-22"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-15",
			wantReturnFrom: "2026-12-22", wantReturnTo: "2026-12-22",
			wantNote: false,
		},
		{
			name:           "one-way with no return date is untouched",
			in:             Spec{TripType: "one_way", DepartDateFrom: "2026-12-15", DepartDateTo: "2026-12-15"},
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
			in:             Spec{DepartDateFrom: "2026-12-31", DepartDateTo: "2026-12-15"},
			wantDepartFrom: "2026-12-15", wantDepartTo: "2026-12-31",
			wantNote: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateDates(tt.in)
			if got.DepartDateFrom != tt.wantDepartFrom || got.DepartDateTo != tt.wantDepartTo {
				t.Errorf("DepartDateFrom/To = %q/%q, want %q/%q", got.DepartDateFrom, got.DepartDateTo, tt.wantDepartFrom, tt.wantDepartTo)
			}
			if got.ReturnDateFrom != tt.wantReturnFrom || got.ReturnDateTo != tt.wantReturnTo {
				t.Errorf("ReturnDateFrom/To = %q/%q, want %q/%q", got.ReturnDateFrom, got.ReturnDateTo, tt.wantReturnFrom, tt.wantReturnTo)
			}
			if hasNote := len(got.Notes) > 0; hasNote != tt.wantNote {
				t.Errorf("Notes = %v, want a note: %v", got.Notes, tt.wantNote)
			}
		})
	}
}

package agents

import "testing"

// TestValidateDates is a regression test: a live run had FormSpec's LLM
// call resolve "next jan" (today in September) to *this* January
// instead of next year's, landing ReturnDate almost a year before
// DepartDate — the resulting round-trip request still dispatched,
// burning dozens of queries (every multi-airport candidate pair, both
// directions) on a search that could never succeed before finalizing a
// literally backwards "round trip" anyway. validateDates is the
// mechanical backstop: whatever the model computes, a return on or
// before the departure date never reaches dispatch.
func TestValidateDates(t *testing.T) {
	tests := []struct {
		name           string
		in             Spec
		wantReturnDate string
		wantNote       bool
	}{
		{
			name:           "return before depart is cleared",
			in:             Spec{TripType: "round_trip", DepartDate: "2026-12-15", ReturnDate: "2026-01-01"},
			wantReturnDate: "",
			wantNote:       true,
		},
		{
			name:           "return equal to depart is cleared",
			in:             Spec{TripType: "round_trip", DepartDate: "2026-12-15", ReturnDate: "2026-12-15"},
			wantReturnDate: "",
			wantNote:       true,
		},
		{
			name:           "return after depart is untouched",
			in:             Spec{TripType: "round_trip", DepartDate: "2026-12-15", ReturnDate: "2026-12-22"},
			wantReturnDate: "2026-12-22",
			wantNote:       false,
		},
		{
			name:           "one-way with no return date is untouched",
			in:             Spec{TripType: "one_way", DepartDate: "2026-12-15"},
			wantReturnDate: "",
			wantNote:       false,
		},
		{
			name:           "round trip missing dates entirely is untouched (nothing to validate yet)",
			in:             Spec{TripType: "round_trip"},
			wantReturnDate: "",
			wantNote:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateDates(tt.in)
			if got.ReturnDate != tt.wantReturnDate {
				t.Errorf("ReturnDate = %q, want %q", got.ReturnDate, tt.wantReturnDate)
			}
			if hasNote := len(got.Notes) > 0; hasNote != tt.wantNote {
				t.Errorf("Notes = %v, want a note: %v", got.Notes, tt.wantNote)
			}
		})
	}
}

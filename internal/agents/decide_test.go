package agents

import "testing"

// TestMissingRequiredFields_TripLengthIsAReturnDateAlternative is a
// regression test for the gap noted in Phase 5: a round trip resolved
// via MinTripLengthDays/MaxTripLengthDays (a trip-length tolerance,
// see Spec.MinTripLengthDays' doc) rather than an independent
// MinReturnDate must not be treated as still missing a return date.
func TestMissingRequiredFields_TripLengthIsAReturnDateAlternative(t *testing.T) {
	base := CollectRouteRequest{
		Origin: "YVR", Destination: "PEK", TripType: "round_trip",
		MinDepartDate: "2026-12-15",
	}

	t.Run("neither return date nor trip length set is missing", func(t *testing.T) {
		if missing := missingRequiredFields(base); len(missing) == 0 {
			t.Error("got no missing fields, want the return date flagged")
		}
	})
	t.Run("MinReturnDate alone satisfies it", func(t *testing.T) {
		req := base
		req.MinReturnDate = "2026-12-22"
		if missing := missingRequiredFields(req); len(missing) != 0 {
			t.Errorf("missing = %v, want none", missing)
		}
	})
	t.Run("MinTripLengthDays alone satisfies it", func(t *testing.T) {
		req := base
		req.MinTripLengthDays = 7
		if missing := missingRequiredFields(req); len(missing) != 0 {
			t.Errorf("missing = %v, want none", missing)
		}
	})
	t.Run("one-way never asks for a return date regardless of trip length", func(t *testing.T) {
		req := base
		req.TripType = "one_way"
		if missing := missingRequiredFields(req); len(missing) != 0 {
			t.Errorf("missing = %v, want none", missing)
		}
	})
}

// TestFillDispatchDefaults_ForwardFillsPhase5Fields covers the six
// Phase 5 fields (trip-length tolerance, hop-country, blackout) added
// to fillDispatchDefaults' existing "zeroed reply field forward-fills
// from spec/prior round" pattern.
func TestFillDispatchDefaults_ForwardFillsPhase5Fields(t *testing.T) {
	spec := Spec{
		MinTripLengthDays: 7, MaxTripLengthDays: 10, TripLengthStepDays: 2,
		MaxCountries:      2,
		ExcludedCountries: []string{"Russia"},
		BlackoutDates:     []string{"2026-12-25"},
	}
	got := fillDispatchDefaults(CollectRouteRequest{}, spec, nil)
	if got.MinTripLengthDays != 7 || got.MaxTripLengthDays != 10 || got.TripLengthStepDays != 2 {
		t.Errorf("trip-length fields = %d/%d/%d, want 7/10/2", got.MinTripLengthDays, got.MaxTripLengthDays, got.TripLengthStepDays)
	}
	if got.MaxCountries != 2 {
		t.Errorf("MaxCountries = %d, want 2", got.MaxCountries)
	}
	if len(got.ExcludedCountries) != 1 || got.ExcludedCountries[0] != "Russia" {
		t.Errorf("ExcludedCountries = %v, want [Russia]", got.ExcludedCountries)
	}
	if len(got.BlackoutDates) != 1 || got.BlackoutDates[0] != "2026-12-25" {
		t.Errorf("BlackoutDates = %v, want [2026-12-25]", got.BlackoutDates)
	}

	// A reply that already set these fields itself is never overridden by
	// the fallback — same "reply wins when non-zero" rule every other
	// field in fillDispatchDefaults already follows.
	reply := CollectRouteRequest{
		MinTripLengthDays: 3, MaxTripLengthDays: 5, TripLengthStepDays: 1,
		MaxCountries:      1,
		ExcludedCountries: []string{"Iran"},
		BlackoutDates:     []string{"2026-01-01"},
	}
	got = fillDispatchDefaults(reply, spec, nil)
	if got.MinTripLengthDays != 3 || got.MaxTripLengthDays != 5 || got.TripLengthStepDays != 1 {
		t.Errorf("trip-length fields = %d/%d/%d, want the reply's own 3/5/1, not spec's fallback", got.MinTripLengthDays, got.MaxTripLengthDays, got.TripLengthStepDays)
	}
	if got.MaxCountries != 1 {
		t.Errorf("MaxCountries = %d, want the reply's own 1", got.MaxCountries)
	}
	if len(got.ExcludedCountries) != 1 || got.ExcludedCountries[0] != "Iran" {
		t.Errorf("ExcludedCountries = %v, want the reply's own [Iran]", got.ExcludedCountries)
	}
	if len(got.BlackoutDates) != 1 || got.BlackoutDates[0] != "2026-01-01" {
		t.Errorf("BlackoutDates = %v, want the reply's own [2026-01-01]", got.BlackoutDates)
	}
}

// TestEstimateDateCombinations covers estimateDateCombinations' three
// shapes: a plain depart-range one-way, an independent depart x return
// grid, and the trip-length-tolerance coupled shape — the mechanical
// figure DecideNextAction discloses on the first ask_user round (see
// wideRangeQueryThreshold).
func TestEstimateDateCombinations(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want int
	}{
		{
			name: "one-way exact date",
			spec: Spec{TripType: "one_way", MinDepartDate: "2026-12-15", MaxDepartDate: "2026-12-15"},
			want: 1,
		},
		{
			name: "one-way depart range, every day",
			spec: Spec{TripType: "one_way", MinDepartDate: "2026-12-01", MaxDepartDate: "2026-12-05"},
			want: 5,
		},
		{
			name: "one-way depart range, every other day",
			spec: Spec{TripType: "one_way", MinDepartDate: "2026-12-01", MaxDepartDate: "2026-12-05", StepDays: 2},
			want: 3, // Dec 1, 3, 5
		},
		{
			name: "round trip, independent depart x return ranges",
			spec: Spec{
				TripType:      "round_trip",
				MinDepartDate: "2026-12-01", MaxDepartDate: "2026-12-03",
				MinReturnDate: "2026-12-10", MaxReturnDate: "2026-12-14",
			},
			want: 3 * 5,
		},
		{
			name: "round trip, trip-length tolerance coupled to depart window",
			spec: Spec{
				TripType:      "round_trip",
				MinDepartDate: "2026-12-01", MaxDepartDate: "2026-12-03",
				MinTripLengthDays: 7, MaxTripLengthDays: 9,
			},
			want: 3 * 3, // 3 depart dates x lengths {7,8,9}
		},
		{
			name: "round trip with neither a return range nor a trip length falls back to depart count alone",
			spec: Spec{TripType: "round_trip", MinDepartDate: "2026-12-01", MaxDepartDate: "2026-12-03"},
			want: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateDateCombinations(tt.spec); got != tt.want {
				t.Errorf("estimateDateCombinations() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsFirstAskUser(t *testing.T) {
	if !isFirstAskUser(nil) {
		t.Error("no rounds yet: want true")
	}
	rounds := []RoundRecord{{Decision: Decision{Action: ActionDispatch}}}
	if !isFirstAskUser(rounds) {
		t.Error("only a dispatch round so far: want true")
	}
	rounds = append(rounds, RoundRecord{Decision: Decision{Action: ActionAskUser}})
	if isFirstAskUser(rounds) {
		t.Error("an earlier ask_user round already happened: want false")
	}
}

func TestJoinMissing(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b, and c"},
	}
	for _, tt := range tests {
		if got := joinMissing(tt.in); got != tt.want {
			t.Errorf("joinMissing(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

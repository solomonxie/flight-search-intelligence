package agents

// Spec is the running, structured understanding of one travel request —
// DESIGN.md "Agent loop" step 1. Concrete fields are what a tool call
// needs; SoftConstraints stays plain language on purpose (see DESIGN.md:
// "the second list doesn't become new Go fields; it stays something only
// the agent reads").
type Spec struct {
	Origin      string
	Destination string
	// TripType is "one_way", "round_trip", or "" (not yet known — a
	// blank ReturnDate alone must never be silently read as "one-way";
	// see missingRequiredFields, which asks explicitly instead).
	TripType string
	// MinDepartDate/MaxDepartDate bound when departure may occur —
	// YYYY-MM-DD, inclusive. An exact, non-flexible date is simply
	// From == To (e.g. a specific "December 15th" sets both to
	// "2026-12-15"); a real range (From < To) makes this a flexible
	// search — routesearch.SearchDateRange prices every date in it and
	// keeps whichever's cheapest, rather than a single fixed date.
	// MinReturnDate/MaxReturnDate are the same idea for the return leg,
	// only meaningful when TripType is "round_trip" — depart and return
	// ranges are independent (see StepDays' doc for the cost tradeoff
	// that implies), unlike a single fixed trip length.
	MinDepartDate string
	MaxDepartDate string
	MinReturnDate string
	MaxReturnDate string
	// MinRoundTripDate/MaxRoundTripDate (round_trip only, optional): an outer
	// eligibility bound both the depart and return date must fall
	// within, e.g. "I only have a month of paid leave" — narrower than
	// MinDepartDate/MaxDepartDate and MinReturnDate/MaxReturnDate, which only control what
	// gets *priced*, not which priced combination is allowed to win.
	// Blank means no such constraint beyond the ranges themselves.
	MinRoundTripDate  string
	MaxRoundTripDate  string
	MaxHours          float64
	QueryBudget       int
	MaxPrice          int // USD hard ceiling on the whole trip; 0 = no cap
	MinLayoverMinutes int
	MaxLayoverMinutes int
	// CheckedBags: whole-trip checked-bag count — DESIGN.md "Baggage cost
	// is a query input, not a scoring adjustment." 0 = not mentioned,
	// same "0 has a real, legitimate meaning" story as MaxPrice, so it's
	// never forward-filled with a nonzero default (see normalizeDefaults).
	CheckedBags int
	// SearchRadiusKm: how far around a named city's center to look for
	// alternate airports when Origin/Destination is a city rather than
	// one specific airport (see dispatch.resolveAirports) — e.g.
	// "Vancouver" within the default 100km also considers Abbotsford.
	// Meaningless when the field already names a specific airport.
	SearchRadiusKm float64
	// StepDays samples every StepDays within a date range above; 0
	// defaults to 1 (every day). Only matters when a range is genuinely
	// flexible (From < To) — a real cost knob: a wide range on both ends
	// of a round trip prices From-to-To-squared combinations, so
	// coarser sampling (e.g. every 3 days) can matter a lot more here
	// than it did for the older single-window case.
	StepDays        int
	SoftConstraints []string
	// Notes carries forward *why* FormSpec left a field blank or unresolved
	// (e.g. "Beijing has multiple airports (PEK/PKX), none specified") —
	// info that would otherwise vanish once FormSpec returns just the
	// blank field itself, leaving DecideNextAction unable to ask anything
	// sharper than a generic "what's your destination?" on every retry.
	Notes []string
	// LastIntent/LastIntentReasoning are FormSpec's read on what the most
	// recent text was doing to the Spec (see Intent) — set fresh on every
	// FormSpec call, not carried forward once stale. DecideNextAction
	// reads this to judge whether an already-dispatched round's result
	// still answers the request: IntentRewrite means a field that
	// already drove a round changed value, not just filled a blank, so
	// that round's result no longer reflects what's being asked.
	LastIntent          Intent
	LastIntentReasoning string
}

// Intent is FormSpec's classification of what one turn of new text was
// doing, judged against the existing Spec/rounds so far — DecideNextAction
// reads it to tell "more detail for the same request" apart from "this
// changes what was already asked and dispatched."
type Intent string

const (
	// IntentNewRequest: this is the first message of a request, or reads
	// like a wholly separate trip from anything already on file.
	IntentNewRequest Intent = "new_request"
	// IntentAdditionalInfo: fills in a field that was blank — narrows or
	// completes the same request, doesn't contradict anything already set.
	IntentAdditionalInfo Intent = "additional_info"
	// IntentRewrite: changes a field that was already set to a different
	// value (e.g. round-trip to one-way, a new destination) — any round
	// already dispatched under the old value is stale.
	IntentRewrite Intent = "rewrite"
	// IntentQuestion: not asking for a (new) search at all — a question
	// about a result already given, answerable from context on file.
	IntentQuestion Intent = "question_about_result"
)

// toCollectRouteRequest is the spec's own view as a dispatch argument
// set — what round 1 (no prior round to copy instead) dispatches with.
func (s Spec) toCollectRouteRequest() CollectRouteRequest {
	return CollectRouteRequest{
		Origin: s.Origin, Destination: s.Destination, TripType: s.TripType,
		MinDepartDate: s.MinDepartDate, MaxDepartDate: s.MaxDepartDate,
		MinReturnDate: s.MinReturnDate, MaxReturnDate: s.MaxReturnDate,
		MinRoundTripDate: s.MinRoundTripDate, MaxRoundTripDate: s.MaxRoundTripDate,
		MaxHours: s.MaxHours, QueryBudget: s.QueryBudget, MaxPrice: s.MaxPrice,
		MinLayoverMinutes: s.MinLayoverMinutes, MaxLayoverMinutes: s.MaxLayoverMinutes,
		CheckedBags:    s.CheckedBags,
		SearchRadiusKm: s.SearchRadiusKm, StepDays: s.StepDays,
	}
}

// Action is what DecideNextAction returns: which of the three moves
// DESIGN.md step 2 names (dispatch, defer, finalize).
type Action string

const (
	ActionDispatch Action = "dispatch"
	ActionDefer    Action = "defer" // not produced by DecideNextAction's real LLM call either — see its system prompt
	ActionAskUser  Action = "ask_user"
	ActionFinalize Action = "finalize"
)

// Decision is one round's output from the (stubbed, for this first draft —
// see DESIGN.md "the LLM call... must be an Activity") decision Activity.
type Decision struct {
	Action    Action
	Request   CollectRouteRequest // set when Action == ActionDispatch
	Question  string              // set when Action == ActionAskUser — DESIGN.md step 2's fourth move
	Reasoning string              // logged to the audit trail regardless of action
}

// RoundRecord is one loop iteration's audit-trail entry — DESIGN.md
// "Audit trail gets a layer above Plan, not a replacement for it."
type RoundRecord struct {
	Round    int
	Spec     Spec
	Decision Decision
	TaskID   string              // agent_tasks row this round dispatched, if any
	Result   *CollectRouteResult // nil until that task completes
}

// Outcome is TravelRequestAgentWorkflow's final return value.
type Outcome struct {
	Spec        Spec
	Rounds      []RoundRecord
	EmailBody   string
	FinalizedBy string // "satisfied" | "round_cap" | "no_soft_constraints"
}

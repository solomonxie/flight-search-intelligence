package agents

// Spec is the running, structured understanding of one travel request —
// DESIGN.md "Agent loop" step 1. Concrete fields are what a tool call
// needs; SoftConstraints stays plain language on purpose (see DESIGN.md:
// "the second list doesn't become new Go fields; it stays something only
// the agent reads").
type Spec struct {
	Origin            string
	Destination       string
	DepartDate        string
	ReturnDate        string
	MaxHours          float64
	QueryBudget       int
	MaxPrice          int // USD hard ceiling on the whole trip; 0 = no cap
	MinLayoverMinutes int
	MaxLayoverMinutes int
	SoftConstraints   []string
}

// toCollectRouteRequest is the spec's own view as a dispatch argument
// set — what round 1 (no prior round to copy instead) dispatches with.
func (s Spec) toCollectRouteRequest() CollectRouteRequest {
	return CollectRouteRequest{
		Origin: s.Origin, Destination: s.Destination, DepartDate: s.DepartDate, ReturnDate: s.ReturnDate,
		MaxHours: s.MaxHours, QueryBudget: s.QueryBudget, MaxPrice: s.MaxPrice,
		MinLayoverMinutes: s.MinLayoverMinutes, MaxLayoverMinutes: s.MaxLayoverMinutes,
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

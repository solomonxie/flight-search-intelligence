package agents

// Spec is the running, structured understanding of one travel request —
// DESIGN.md "Agent loop" step 1. Concrete fields are what a tool call
// needs; SoftConstraints stays plain language on purpose (see DESIGN.md:
// "the second list doesn't become new Go fields; it stays something only
// the agent reads").
type Spec struct {
	Origin          string
	Destination     string
	DepartDate      string
	ReturnDate      string
	MaxHours        float64
	QueryBudget     int
	SoftConstraints []string
}

// Action is what DecideNextAction returns: which of the three moves
// DESIGN.md step 2 names (dispatch, defer, finalize).
type Action string

const (
	ActionDispatch Action = "dispatch"
	ActionDefer    Action = "defer"
	ActionFinalize Action = "finalize"
)

// Decision is one round's output from the (stubbed, for this first draft —
// see DESIGN.md "the LLM call... must be an Activity") decision Activity.
type Decision struct {
	Action    Action
	Request   CollectRouteRequest // set when Action == ActionDispatch
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

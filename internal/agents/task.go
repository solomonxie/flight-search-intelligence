package agents

// CollectRouteRequest is what the agent loop dispatches as one tool call —
// the agent_tasks.params_json payload cmd/collector -worker claims and
// runs (see DESIGN.md "Where this lives in the repo"). Referenced by both
// packages without either importing the other's cmd/internal package —
// this type is the whole contract between them.
// deliberately the same fixed, typed shape as routesearch.Params' core
// fields (see DESIGN.md "Go code's job shrinks to match, deliberately").
// cmd/collector depends on this type; this package does not depend on
// cmd/collector, keeping the import direction one-way.
type CollectRouteRequest struct {
	Origin      string
	Destination string
	DepartDate  string
	ReturnDate  string
	MaxHours    float64
	QueryBudget int
}

// CollectRouteResult is the structured result a dispatched search returns —
// deliberately thin for this first draft (routesearch.Plan's concrete
// fields the agent's soft-constraint check needs), not the full Plan.
type CollectRouteResult struct {
	RequestID   string
	QueriesUsed int
	Results     []CollectRouteOffer
}

// CollectRouteOffer is one itinerary from the Pareto set routesearch.Search
// returns, trimmed to what the finalize-email step and the soft-constraint
// check (DESIGN.md step 4) need.
type CollectRouteOffer struct {
	PriceUSD        float64
	DurationMinutes int
	Path            []string
	SelfTransfer    bool
}

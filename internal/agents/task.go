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
	Origin            string
	Destination       string
	TripType          string // "one_way" | "round_trip" | "" (unresolved) — see Spec.TripType; dispatch.runSearch routes "round_trip" to routesearch.SearchRoundTrip instead of the plain one-way Search
	DepartDate        string
	ReturnDate        string
	MaxHours          float64
	QueryBudget       int
	MaxPrice          int // USD hard ceiling on the whole trip; 0 = no cap
	MinLayoverMinutes int
	MaxLayoverMinutes int
	SearchRadiusKm    float64 // see Spec.SearchRadiusKm
}

// CollectRouteResult is the structured result a dispatched search returns —
// deliberately thin for this first draft (routesearch.Plan's concrete
// fields the agent's soft-constraint check needs), not the full Plan.
type CollectRouteResult struct {
	RequestID   string
	QueriesUsed int
	Results     []CollectRouteOffer
}

// CollectRouteOffer is one itinerary — from the Pareto set
// routesearch.Search returns for TripType "one_way", or the single best
// comparison routesearch.SearchRoundTrip returns for "round_trip" —
// trimmed to what the finalize-email step and the soft-constraint check
// (DESIGN.md step 4) need.
//
// For "round_trip", PriceUSD/DurationMinutes/Path/SelfTransfer describe
// the outbound leg alone (zero/empty when Bundled is true — a bundled
// fare prices the whole trip as one ticket, with no separate outbound
// price to report); TotalPriceUSD is the trip's real price and what to
// quote, with the Return* fields describing the other direction.
type CollectRouteOffer struct {
	PriceUSD        float64
	DurationMinutes int
	Path            []string
	SelfTransfer    bool

	Bundled               bool     `json:",omitempty"` // true: one bundled Google fare beat pricing outbound+return separately
	TotalPriceUSD         float64  `json:",omitempty"` // round-trip only: the whole trip's price
	ReturnPath            []string `json:",omitempty"`
	ReturnPriceUSD        float64  `json:",omitempty"`
	ReturnDurationMinutes int      `json:",omitempty"`
	ReturnSelfTransfer    bool     `json:",omitempty"`
}

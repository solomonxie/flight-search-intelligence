package routesearch

import (
	"log/slog"
	"time"

	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
)

// Params is one user request's constraints. One-way only for now — see
// the package doc for what else is deliberately out of scope.
type Params struct {
	Origin            string
	Destination       string
	DepartDate        string // YYYY-MM-DD
	MaxHours          float64
	QueryBudget       int
	MinLayoverMinutes int
	MaxLayoverMinutes int
	PricePerMile      float64       // fallback lower-bound prior when nothing's cached
	Delay             time.Duration // stand-in for Temporal's durable timer; see DESIGN.md "Pacing"
	ForceRefresh      bool          // bypass the offers cache and scrape live even within offersCacheFreshness
}

// Deps are this search's collaborators — a real googleflights client,
// the openflights route-existence graph, the local audit/price store,
// and a structured logger (see DESIGN.md "Step-level visibility").
type Deps struct {
	Flights *googleflights.Client
	Graph   *openflights.Graph
	Catalog *catalog.SQLite
	Logger  *slog.Logger
}

// LegOutcome records what happened (or didn't) when a single leg was
// considered, for the audit trail.
type LegOutcome struct {
	Queried   bool      `json:"queried"`
	PriceUSD  float64   `json:"price_usd,omitempty"`
	QueriedAt time.Time `json:"queried_at,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// CandidateOutcome is one hub's full audit-trail entry.
type CandidateOutcome struct {
	Hub         string      `json:"hub"`
	LBUSD       float64     `json:"lb_usd"`
	Rank        int         `json:"rank"`
	Leg1        *LegOutcome `json:"leg1,omitempty"`
	Leg2        *LegOutcome `json:"leg2,omitempty"`
	Outcome     string      `json:"outcome"` // kept | pruned | leg1_infeasible | leg2_infeasible | frontier_cutoff | budget_exhausted
	CombinedUSD float64     `json:"combined_usd,omitempty"`
	Reason      string      `json:"reason,omitempty"`
}

// Result is one itinerary in the final Pareto set.
type Result struct {
	Path            []string `json:"path"` // e.g. ["SFO","DEN","JFK"]
	PriceUSD        float64  `json:"price_usd"`
	DurationMinutes int      `json:"duration_minutes"`
	SelfTransfer    bool     `json:"self_transfer"` // separate-ticket combo; see DESIGN.md "Output"
	LayoverMinutes  int      `json:"layover_minutes,omitempty"`
	Stopover        bool     `json:"stopover,omitempty"` // layover long enough it's really a mini-trip, not a connection
}

// stopoverThreshold: a layover past this is flagged as a deliberate
// stopover rather than a connection — long enough to plausibly need a
// hotel, which this tool doesn't price (no lodging data source), so it
// only labels the option rather than costing it in.
const stopoverThreshold = 6 * time.Hour

// Plan is the full per-request audit trail (see DESIGN.md "Audit
// trail") — persisted to the store as one JSON document.
type Plan struct {
	RequestID                    string             `json:"request_id"`
	Input                        Params             `json:"input"`
	CandidatesConsidered         int                `json:"candidates_considered"`
	CandidatesAfterGeometryPrune int                `json:"candidates_after_geometry_prune"`
	CandidatesRanked             []CandidateOutcome `json:"candidates_ranked"`
	FinalResult                  []Result           `json:"final_result"`
	QueriesUsed                  int                `json:"queries_used"`
	Status                       string             `json:"status"`
}

// RankedHub is a hub still in play after the geometry prune, ordered by
// LBUSD (the admissible lower-bound estimate of the full A→hub→B price).
type RankedHub struct {
	Hub       string  `json:"hub"`
	LBUSD     float64 `json:"lb_usd"`
	Leg1Miles float64 `json:"leg1_miles"`
	Leg2Miles float64 `json:"leg2_miles"`
}

// CandidatePreview is Search's Step 0/1 setup — airport resolution, raw
// hub candidates, geometry prune, and lower-bound ranking — with no
// scraping done yet. See ResolveCandidates.
type CandidatePreview struct {
	Origin, Destination          openflights.Airport
	DirectDistanceMiles          float64
	HasNonstop                   bool
	CandidatesConsidered         int
	CandidatesAfterGeometryPrune int
	RankedHubs                   []RankedHub
}

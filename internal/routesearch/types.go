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
	Origin      string
	Destination string
	DepartDate  string // YYYY-MM-DD
	MaxHours    float64
	// QueryBudget caps how many scrapes the search may spend. <= 0 (the
	// default) means unlimited: the search runs exhaustively, every
	// surviving candidate, until the frontier's own provably-optimal
	// cutoff proves nothing left can beat the current best — this is
	// the project's core selling point over a plain flight search (see
	// README). A positive value trades that optimality guarantee for a
	// bounded, faster/cheaper run.
	QueryBudget       int
	MinLayoverMinutes int
	MaxLayoverMinutes int
	MaxPrice          int           // USD hard ceiling on the whole trip; 0 = no cap
	PricePerMile      float64       // fallback lower-bound prior when nothing's cached
	Delay             time.Duration // stand-in for Temporal's durable timer; see DESIGN.md "Pacing"
	ForceRefresh      bool          // bypass the offers cache and scrape live even within offersCacheFreshness

	// MaxLegs raises the 1-stop depth cap — DESIGN.md "Deeper
	// itineraries." 0 or 1 (the default/unset value) keeps today's
	// exact 1-stop behavior (Search's original A->hub->B loop,
	// untouched); >1 switches to searchNHop's label-setting
	// generalization (nhop.go). QUERY_BUDGET stays flat, not scaled by
	// this — see IMPLEMENTATION_PLAN.md's Phase 3 for why.
	MaxLegs int
	// MaxCountries caps how many distinct countries a candidate path may
	// transit, independent of MaxLegs (0 = no cap). ExcludedCountries is
	// a hard prune, not a soft preference — a real constraint (visa
	// eligibility, sanctions, safety), decided by the agent loop and
	// only ever *applied* here, never derived from anything routesearch
	// itself knows (see DESIGN.md "Deeper itineraries" — "the exclusion
	// list's source is the agent loop, not routesearch").
	MaxCountries      int
	ExcludedCountries []string
	// CheckedBags: whole-trip checked-bag count, threaded straight through
	// to every googleflights query — DESIGN.md "Baggage cost is a query
	// input, not a scoring adjustment": Offer.Price already comes back
	// bag-inclusive, so nothing downstream (scoring, Pareto set,
	// bestConnection) needs to change. 0 = don't tell Google at all
	// (today's existing behavior), same as MaxPrice's "0 = no cap" — not
	// "explicitly zero checked bags."
	CheckedBags int
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
	// TransferCount is len(Path)-2 — how many separate-ticket connection
	// points the itinerary has (0 for a direct/bundled fare). SelfTransfer
	// alone doesn't distinguish "one hub" from "four hops through four
	// airports," a materially different risk profile (DESIGN.md "Deeper
	// itineraries": "Update self-transfer risk reporting from a single
	// bool to a real [risk profile]") — this is that without discarding
	// the simpler bool everything already checks.
	TransferCount  int  `json:"transfer_count,omitempty"`
	LayoverMinutes int  `json:"layover_minutes,omitempty"`
	Stopover       bool `json:"stopover,omitempty"` // layover long enough it's really a mini-trip, not a connection
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
	// NHopRanked is CandidatesRanked's MaxLegs > 1 counterpart —
	// CandidateOutcome's Leg1/Leg2 shape is specific to the exactly-one-hub
	// case, so an arbitrary-depth search gets its own, sibling audit
	// entry per edge tried rather than forcing that shape to stretch.
	// Populated only when Input.MaxLegs > 1; empty otherwise.
	NHopRanked []NHopOutcome `json:"nhop_ranked,omitempty"`
}

// NHopOutcome is one candidate edge's audit-trail entry from searchNHop
// (nhop.go) — "starting from this partial itinerary, trying this next
// airport." See CandidateOutcome for the 1-stop case's equivalent.
type NHopOutcome struct {
	FromPath []string `json:"from_path"`
	To       string   `json:"to"`
	LBUSD    float64  `json:"lb_usd"`
	PriceUSD float64  `json:"price_usd,omitempty"`
	Outcome  string   `json:"outcome"` // kept | infeasible | dominated | depth_cutoff | frontier_cutoff | budget_exhausted
	Reason   string   `json:"reason,omitempty"`
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
	DirectRow                    RankedHub // display only; Search never scrapes this — see ResolveCandidates
	RankedHubs                   []RankedHub
}

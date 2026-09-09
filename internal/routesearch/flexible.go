package routesearch

import (
	"context"
	"fmt"
	"time"

	"flight-search-intelligence/internal/googleflights"
)

// DateScanEntry is one date (or date pair) tried in Phase A of a
// flexible-date search — see DESIGN.md "Round trips and flexible
// dates".
type DateScanEntry struct {
	DepartDate string  `json:"depart_date"`
	ReturnDate string  `json:"return_date,omitempty"`
	PriceUSD   float64 `json:"price_usd,omitempty"`
	Queried    bool    `json:"queried"`
	Reason     string  `json:"reason,omitempty"`
	// Excluded, when non-empty, says why this entry — priced fine — is
	// ineligible to be the Phase A winner (see FlexibleParams' Available*/
	// Exclude*/Blackout* constraints). It's still shown/priced for
	// context; cheapestDateScanEntry just skips it.
	Excluded string `json:"excluded,omitempty"`
}

// FlexibleParams is a flexible-date request: a depart-date window plus
// how wide/coarse to scan it, and — for a round trip — a trip-length
// tolerance coupled to each depart date rather than a full depart×return
// grid (see DateRangeParams for that independent-ranges alternative).
type FlexibleParams struct {
	Base      Params // Origin/Destination/MaxHours/QueryBudget/etc — DepartDate is the window's center, only when DepartFrom/DepartTo are unset
	RoundTrip bool
	// TripLengthDays: return = depart + TripLengthDays, coupled (not a
	// full depart×return grid). TripLengthMaxDays makes this a tolerance
	// range instead of one fixed length: every length from TripLengthDays
	// through TripLengthMaxDays is tried, sampled every TripLengthStepDays
	// (0 defaults to 1, every day) — TripLengthMaxDays 0 (or < TripLengthDays)
	// means "just TripLengthDays," today's single-fixed-length behavior,
	// unchanged.
	TripLengthDays     int
	TripLengthMaxDays  int
	TripLengthStepDays int
	// WindowDays/StepDays: the depart-date window, scanned
	// [center-WindowDays, center+WindowDays] every StepDays — only used
	// when DepartFrom/DepartTo (below) are both unset.
	WindowDays int
	StepDays   int
	// DepartFrom/DepartTo: an explicit depart-date window, as an
	// alternative to Base.DepartDate (window center) + WindowDays above —
	// set one shape or the other; DepartFrom/DepartTo wins if either is
	// set, since naming an explicit window is more specific than a center
	// point.
	DepartFrom string
	DepartTo   string
	ScanOnly   bool // stop after Phase A (the date scan); skip Phase B's hub search

	// Eligibility constraints on which scanned date can actually be
	// picked as the winner — deliberately separate from WindowDays/
	// StepDays, which only control what gets *priced*. Every date in the
	// window is still scanned and shown either way; these just narrow
	// which of the priced dates cheapestDateScanEntry may choose.
	AvailableFrom   string         // YYYY-MM-DD; depart/return must both be >= this, if set ("I'm not free before...")
	AvailableUntil  string         // YYYY-MM-DD; depart/return must both be <= this, if set ("...or after")
	ExcludeWeekdays []time.Weekday // depart dates falling on these weekdays are ineligible
	BlackoutDates   []string       // YYYY-MM-DD; a depart or return date matching one is ineligible
}

// FlexiblePlan is the audit trail for a flexible-date search: the full
// date_scan (Phase A, cheap baseline-only queries) plus which single
// date won and got the full hub search (Phase B) — see AnchoredPlanID
// for that phase's own detail.
type FlexiblePlan struct {
	RequestID        string           `json:"request_id"`
	Input            FlexibleParams   `json:"input"`
	DateScan         []DateScanEntry  `json:"date_scan"`
	ChosenDepartDate string           `json:"chosen_depart_date"`
	ChosenReturnDate string           `json:"chosen_return_date,omitempty"`
	AnchoredPlanID   string           `json:"anchored_plan_id"`
	OneWayResult     *Result          `json:"one_way_result,omitempty"`
	RoundTripResult  *RoundTripResult `json:"round_trip_result,omitempty"`
	Status           string           `json:"status"`
}

// SearchFlexible runs the two-phase algorithm from DESIGN.md: a cheap
// baseline-only scan across the date window (Phase A — one query per
// date point, no hub search), then the full Search/SearchRoundTrip
// (Phase B, the expensive part) on just the date(s) that won.
func SearchFlexible(ctx context.Context, deps Deps, p FlexibleParams) (*FlexiblePlan, error) {
	requestID := fmt.Sprintf("FLEX-%s-%s-%d", p.Base.Origin, p.Base.Destination, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &FlexiblePlan{RequestID: requestID, Input: p, Status: "running"}
	savePlan(ctx, deps, requestID, plan.Status, plan)

	departDates, err := p.departDates()
	if err != nil {
		plan.Status = fmt.Sprintf("error: %v", err)
		savePlan(ctx, deps, requestID, plan.Status, plan)
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}
	tripLengths := p.tripLengths()

	elig := eligibility{AvailableFrom: p.AvailableFrom, AvailableUntil: p.AvailableUntil, ExcludeWeekdays: p.ExcludeWeekdays, BlackoutDates: p.BlackoutDates}
	queriesUsed := 0
	budgetExhausted := false
	log.Info("phase A: date scan", "departs", len(departDates), "trip_lengths", len(tripLengths), "round_trip", p.RoundTrip)
outer:
	for _, depart := range departDates {
		for _, tripLen := range tripLengths {
			if !withinBudget(queriesUsed, p.Base.QueryBudget) {
				budgetExhausted = true
				break outer
			}
			entry, live := scanOneDateAt(ctx, deps, p, depart, tripLen)
			entry.Excluded = exclusionReason(elig, entry)
			plan.DateScan = append(plan.DateScan, entry)
			log.Info("date scan point", "depart", entry.DepartDate, "return", entry.ReturnDate,
				"price_usd", entry.PriceUSD, "reason", entry.Reason, "live", live)
			// Only pace/count a real scrape — a cache hit costs Google
			// nothing, so waiting p.Base.Delay anyway made a fully-cached
			// rerun look just as slow as the first live run, masking that
			// the cache worked.
			if live {
				queriesUsed++
				sleepPacing(ctx, p.Base.Delay)
			}
		}
	}
	if budgetExhausted {
		log.Warn("date scan truncated: query budget exhausted before every combination was priced", "priced", len(plan.DateScan), "query_budget", p.Base.QueryBudget)
	}

	best := cheapestDateScanEntry(plan.DateScan)
	if best == nil {
		reason := "no feasible date in window"
		if anyPricedButExcluded(plan.DateScan) {
			reason = "every priced date is excluded by AvailableFrom/AvailableUntil/ExcludeWeekdays/BlackoutDates"
		}
		plan.Status = "error: " + reason
		savePlan(ctx, deps, requestID, plan.Status, plan)
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}
	plan.ChosenDepartDate = best.DepartDate
	plan.ChosenReturnDate = best.ReturnDate
	log.Info("phase A winner", "depart", best.DepartDate, "return", best.ReturnDate, "price_usd", best.PriceUSD)

	if p.ScanOnly {
		plan.Status = "done (scan only, phase B skipped)"
		savePlan(ctx, deps, requestID, plan.Status, plan)
		log.Info("scan-only: skipping phase B")
		return plan, nil
	}

	log.Info("phase B: full hub search on the winning date(s)")
	anchored := p.Base
	anchored.DepartDate = best.DepartDate
	if p.RoundTrip {
		rtPlan, err := SearchRoundTrip(ctx, deps, anchored, best.ReturnDate)
		if err != nil {
			plan.Status = fmt.Sprintf("error: phase B: %v", err)
			savePlan(ctx, deps, requestID, plan.Status, plan)
			return plan, err
		}
		plan.AnchoredPlanID = rtPlan.RequestID
		plan.RoundTripResult = rtPlan.Result
	} else {
		owPlan, err := Search(ctx, deps, anchored)
		if err != nil {
			plan.Status = fmt.Sprintf("error: phase B: %v", err)
			savePlan(ctx, deps, requestID, plan.Status, plan)
			return plan, err
		}
		plan.AnchoredPlanID = owPlan.RequestID
		if len(owPlan.FinalResult) > 0 {
			plan.OneWayResult = cheapestResult(owPlan.FinalResult)
		}
	}

	plan.Status = "done"
	savePlan(ctx, deps, requestID, plan.Status, plan)
	log.Info("flexible search done")
	return plan, nil
}

// departDates resolves FlexibleParams' depart-date window into a plain
// list: the explicit DepartFrom/DepartTo window when either is set,
// otherwise Base.DepartDate (the center) +/- WindowDays — see
// FlexibleParams' doc for why one shape wins over the other.
func (p FlexibleParams) departDates() ([]string, error) {
	step := p.StepDays
	if step < 1 {
		step = 1
	}
	if p.DepartFrom != "" || p.DepartTo != "" {
		dates, err := dateRange(p.DepartFrom, p.DepartTo, step)
		if err != nil {
			return nil, fmt.Errorf("invalid depart window: %w", err)
		}
		return dates, nil
	}
	center, err := time.Parse("2006-01-02", p.Base.DepartDate)
	if err != nil {
		return nil, fmt.Errorf("invalid depart date: %w", err)
	}
	var out []string
	for offset := -p.WindowDays; offset <= p.WindowDays; offset += step {
		out = append(out, center.AddDate(0, 0, offset).Format("2006-01-02"))
	}
	return out, nil
}

// tripLengths resolves the round-trip length tolerance range into a
// plain list of day counts: TripLengthDays through TripLengthMaxDays
// (defaulting to just TripLengthDays, today's single fixed length,
// unchanged), sampled every TripLengthStepDays (0 defaults to 1). A
// one-way search has no trip length at all — a single zero-value
// placeholder so its caller's loop still runs exactly once.
func (p FlexibleParams) tripLengths() []int {
	if !p.RoundTrip {
		return []int{0}
	}
	max := p.TripLengthMaxDays
	if max < p.TripLengthDays {
		max = p.TripLengthDays
	}
	step := p.TripLengthStepDays
	if step < 1 {
		step = 1
	}
	var out []int
	for n := p.TripLengthDays; n <= max; n += step {
		out = append(out, n)
	}
	return out
}

// EstimatedCombinations is the worst-case number of Phase A price checks
// this request implies — depart dates x trip lengths — the pre-flight
// cost estimate cmd/routesearch's confirmation prompt (and the agent
// loop's own ask_user disclosure, see agents.decideSystemPrompt) shows
// before spending a single real scrape. Returns 0 if the depart window
// itself fails to resolve (e.g. an invalid date) — the caller's own
// SearchFlexible call surfaces that error properly; this is
// display-estimate only.
func (p FlexibleParams) EstimatedCombinations() int {
	departDates, err := p.departDates()
	if err != nil {
		return 0
	}
	return len(departDates) * len(p.tripLengths())
}

// scanOneDateAt is one Phase-A query for a given depart date and
// (round-trip only) trip length: cheapest-by-price only, no hub search —
// the whole point of this phase is staying cheap per combination so a
// wide window is affordable. Thin wrapper around scanPair, which
// SearchDateRange (daterange.go) also uses for the independent-ranges
// case.
func scanOneDateAt(ctx context.Context, deps Deps, p FlexibleParams, depart string, tripLenDays int) (DateScanEntry, bool) {
	if !p.RoundTrip {
		return scanPair(ctx, deps, p.Base, depart, "")
	}
	d, err := time.Parse("2006-01-02", depart)
	if err != nil {
		return DateScanEntry{DepartDate: depart, Reason: err.Error()}, false
	}
	ret := d.AddDate(0, 0, tripLenDays).Format("2006-01-02")
	return scanPair(ctx, deps, p.Base, depart, ret)
}

// scanPair is the low-level Phase-A query for one (depart[, return])
// combination: cheapest-by-price only, no hub search. ret == "" means
// one-way. Shared by scanOneDate (a fixed offset from a center date)
// and SearchDateRange's grid (every depart x return combination in two
// independent ranges) — both are just different ways of enumerating
// which pairs to try; the query itself doesn't care which.
func scanPair(ctx context.Context, deps Deps, base Params, depart, ret string) (DateScanEntry, bool) {
	entry := DateScanEntry{DepartDate: depart, ReturnDate: ret}
	params := googleflights.SearchParams{
		Origin: base.Origin, Destination: base.Destination, DepartureDate: depart,
		MaxPrice: maxPricePtr(base.MaxPrice), CheckedBags: checkedBagsPtr(base.CheckedBags),
	}
	if ret != "" {
		params.ReturnDate = ret
	}
	offers, live, err := deps.searchOffers(ctx, params, base.ForceRefresh)
	entry.Queried = true
	if err != nil {
		entry.Reason = err.Error()
		return entry, live
	}
	if ret != "" {
		// Whole-trip bundled price — no duration/feasibility filter, same
		// as SearchRoundTrip's own bundled comparison (price alone decides).
		if offer, ok := cheapestOffer(offers); ok {
			entry.PriceUSD = float64(offer.Price)
		} else {
			entry.Reason = "no offers"
		}
		return entry, live
	}
	if offer, _, ok := pickCheapestFeasible(offers, deps.Graph, base.MaxHours, float64(base.MaxPrice)); ok {
		entry.PriceUSD = float64(offer.Price)
	} else {
		entry.Reason = "no feasible offer"
	}
	return entry, live
}

// eligibility is the "real-world window narrower than what gets priced"
// constraint both FlexibleParams (coupled window+length) and
// DateRangeParams (independent ranges) support — e.g. limited PTO.
// Every scanned combination is still priced and shown either way; this
// only narrows which of them cheapestDateScanEntry may pick as the
// winner. Factored out of FlexibleParams so both share one check.
type eligibility struct {
	AvailableFrom, AvailableUntil string
	ExcludeWeekdays               []time.Weekday
	BlackoutDates                 []string
}

// exclusionReason reports why entry, if priced, may not be picked as the
// Phase A winner — empty if it's fully eligible. Checked independently of
// PriceUSD/Reason so an excluded date still shows its price for context
// (e.g. "yes it's $50 cheaper, but it's a blackout date").
func exclusionReason(e eligibility, entry DateScanEntry) string {
	if e.AvailableFrom != "" {
		if entry.DepartDate < e.AvailableFrom || (entry.ReturnDate != "" && entry.ReturnDate < e.AvailableFrom) {
			return "before AvailableFrom " + e.AvailableFrom
		}
	}
	if e.AvailableUntil != "" {
		if entry.DepartDate > e.AvailableUntil || (entry.ReturnDate != "" && entry.ReturnDate > e.AvailableUntil) {
			return "after AvailableUntil " + e.AvailableUntil
		}
	}
	if len(e.ExcludeWeekdays) > 0 {
		if depart, err := time.Parse("2006-01-02", entry.DepartDate); err == nil {
			for _, wd := range e.ExcludeWeekdays {
				if depart.Weekday() == wd {
					return "excluded weekday " + wd.String()
				}
			}
		}
	}
	for _, b := range e.BlackoutDates {
		if entry.DepartDate == b || (entry.ReturnDate != "" && entry.ReturnDate == b) {
			return "blackout date " + b
		}
	}
	return ""
}

// anyPricedButExcluded distinguishes "the whole window came back empty"
// from "prices came back fine, but every one was excluded" — the two
// SearchFlexible failure modes read very differently.
func anyPricedButExcluded(entries []DateScanEntry) bool {
	for _, e := range entries {
		if e.Queried && e.PriceUSD > 0 && e.Excluded != "" {
			return true
		}
	}
	return false
}

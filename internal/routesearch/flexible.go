package routesearch

import (
	"context"
	"fmt"
	"time"

	"flight-search-intelligence/internal/googleflights"
)

// DateSweepEntry is one date (or date pair) tried in Phase A of a
// flexible-date search — see DESIGN.md "Round trips and flexible
// dates".
type DateSweepEntry struct {
	DepartDate string  `json:"depart_date"`
	ReturnDate string  `json:"return_date,omitempty"`
	PriceUSD   float64 `json:"price_usd,omitempty"`
	Queried    bool    `json:"queried"`
	Reason     string  `json:"reason,omitempty"`
}

// FlexibleParams is a flexible-date request: a target date (Base.
// DepartDate is the sweep window's center) plus how wide/coarse to
// sweep around it.
type FlexibleParams struct {
	Base           Params // Origin/Destination/MaxHours/QueryBudget/etc — DepartDate is the window's center
	RoundTrip      bool
	TripLengthDays int // only used if RoundTrip: return = depart + TripLengthDays, coupled (not a full depart×return grid)
	WindowDays     int // sweep [center-WindowDays, center+WindowDays]
	StepDays       int // sample every StepDays within the window; 1 = every day
}

// FlexiblePlan is the audit trail for a flexible-date search: the full
// date_sweep (Phase A, cheap baseline-only queries) plus which single
// date won and got the full hub search (Phase B) — see AnchoredPlanID
// for that phase's own detail.
type FlexiblePlan struct {
	RequestID        string           `json:"request_id"`
	Input            FlexibleParams   `json:"input"`
	DateSweep        []DateSweepEntry `json:"date_sweep"`
	ChosenDepartDate string           `json:"chosen_depart_date"`
	ChosenReturnDate string           `json:"chosen_return_date,omitempty"`
	AnchoredPlanID   string           `json:"anchored_plan_id"`
	OneWayResult     *Result          `json:"one_way_result,omitempty"`
	RoundTripResult  *RoundTripResult `json:"round_trip_result,omitempty"`
	Status           string           `json:"status"`
}

// SearchFlexible runs the two-phase algorithm from DESIGN.md: a cheap
// baseline-only sweep across the date window (Phase A — one query per
// date point, no hub search), then the full Search/SearchRoundTrip
// (Phase B, the expensive part) on just the date(s) that won.
func SearchFlexible(ctx context.Context, deps Deps, p FlexibleParams) (*FlexiblePlan, error) {
	requestID := fmt.Sprintf("FLEX-%s-%s-%d", p.Base.Origin, p.Base.Destination, time.Now().UnixNano())
	log := deps.Logger.With("request_id", requestID)
	plan := &FlexiblePlan{RequestID: requestID, Input: p, Status: "running"}
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))

	center, err := time.Parse("2006-01-02", p.Base.DepartDate)
	if err != nil {
		plan.Status = fmt.Sprintf("error: invalid depart date: %v", err)
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}
	step := p.StepDays
	if step < 1 {
		step = 1
	}

	log.Info("phase A: date sweep", "window_days", p.WindowDays, "step_days", step, "round_trip", p.RoundTrip)
	for offset := -p.WindowDays; offset <= p.WindowDays; offset += step {
		entry := sweepOneDate(ctx, deps, p, center, offset)
		plan.DateSweep = append(plan.DateSweep, entry)
		log.Info("date sweep point", "depart", entry.DepartDate, "return", entry.ReturnDate,
			"price_usd", entry.PriceUSD, "reason", entry.Reason)
		sleepPacing(ctx, p.Base.Delay)
	}

	best := cheapestDateSweepEntry(plan.DateSweep)
	if best == nil {
		plan.Status = "error: no feasible date in window"
		_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
		return plan, fmt.Errorf("routesearch: %s", plan.Status)
	}
	plan.ChosenDepartDate = best.DepartDate
	plan.ChosenReturnDate = best.ReturnDate
	log.Info("phase A winner", "depart", best.DepartDate, "return", best.ReturnDate, "price_usd", best.PriceUSD)

	log.Info("phase B: full hub search on the winning date(s)")
	anchored := p.Base
	anchored.DepartDate = best.DepartDate
	if p.RoundTrip {
		rtPlan, err := SearchRoundTrip(ctx, deps, anchored, best.ReturnDate)
		if err != nil {
			plan.Status = fmt.Sprintf("error: phase B: %v", err)
			_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
			return plan, err
		}
		plan.AnchoredPlanID = rtPlan.RequestID
		plan.RoundTripResult = rtPlan.Result
	} else {
		owPlan, err := Search(ctx, deps, anchored)
		if err != nil {
			plan.Status = fmt.Sprintf("error: phase B: %v", err)
			_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
			return plan, err
		}
		plan.AnchoredPlanID = owPlan.RequestID
		if len(owPlan.FinalResult) > 0 {
			plan.OneWayResult = cheapestResult(owPlan.FinalResult)
		}
	}

	plan.Status = "done"
	_ = deps.Catalog.SaveRouteSearchPlan(ctx, requestID, plan.Status, mustJSON(plan))
	log.Info("flexible search done")
	return plan, nil
}

// sweepOneDate is one Phase-A query: cheapest-by-price only, no hub
// search — the whole point of this phase is staying cheap per date so a
// wide window is affordable.
func sweepOneDate(ctx context.Context, deps Deps, p FlexibleParams, center time.Time, offsetDays int) DateSweepEntry {
	depart := center.AddDate(0, 0, offsetDays).Format("2006-01-02")
	entry := DateSweepEntry{DepartDate: depart}

	if p.RoundTrip {
		ret := center.AddDate(0, 0, offsetDays+p.TripLengthDays).Format("2006-01-02")
		entry.ReturnDate = ret
		offers, _, err := deps.Flights.SearchFlightOffers(ctx, googleflights.SearchParams{
			Origin: p.Base.Origin, Destination: p.Base.Destination, DepartureDate: depart, ReturnDate: ret,
		})
		entry.Queried = true
		if err != nil {
			entry.Reason = err.Error()
			return entry
		}
		if offer, ok := cheapestOffer(offers); ok {
			entry.PriceUSD = float64(offer.Price)
		} else {
			entry.Reason = "no offers"
		}
		return entry
	}

	offers, _, err := deps.Flights.SearchFlightOffers(ctx, googleflights.SearchParams{
		Origin: p.Base.Origin, Destination: p.Base.Destination, DepartureDate: depart,
	})
	entry.Queried = true
	if err != nil {
		entry.Reason = err.Error()
		return entry
	}
	deps.recordOffers(ctx, p.Base.Origin, p.Base.Destination, depart, offers)
	if offer, _, ok := pickCheapestFeasible(offers, deps.Graph, p.Base.MaxHours); ok {
		entry.PriceUSD = float64(offer.Price)
	} else {
		entry.Reason = "no feasible offer"
	}
	return entry
}

func cheapestDateSweepEntry(entries []DateSweepEntry) *DateSweepEntry {
	var best *DateSweepEntry
	for i := range entries {
		e := &entries[i]
		if !e.Queried || e.PriceUSD <= 0 {
			continue
		}
		if best == nil || e.PriceUSD < best.PriceUSD {
			best = e
		}
	}
	return best
}

package agents

import (
	"context"
	"fmt"
)

// DecideNextAction stands in for the real LLM call DESIGN.md's agent loop
// step 2 describes. This stub applies a fixed, deterministic rule instead
// of a model call, so the loop, redispatch cap, and store-backed state
// machine around it (reconcile.go) can be built and tested before the LLM
// itself is wired in (DESIGN.md "LLM choice / call shape: deferred
// entirely"). Kept as its own function (ctx included, for whatever a real
// LLM client needs) so swapping in a real call later doesn't touch
// reconcile.go at all.
//
// Stub policy: dispatch with the spec's current params on round 0; after a
// result, finalize if it has any offer, otherwise widen (MaxHours +25%,
// QueryBudget +5) and retry. Soft-constraint checking (DESIGN.md step 4)
// is NOT actually evaluated here — flagged in Reasoning, not silently
// skipped — since that judgment call is exactly what a real LLM call is for.
func DecideNextAction(ctx context.Context, spec Spec, rounds []RoundRecord) (Decision, error) {
	if len(rounds) == 0 {
		return Decision{
			Action: ActionDispatch,
			Request: CollectRouteRequest{
				Origin: spec.Origin, Destination: spec.Destination,
				DepartDate: spec.DepartDate, ReturnDate: spec.ReturnDate,
				MaxHours: spec.MaxHours, QueryBudget: spec.QueryBudget,
			},
			Reasoning: "first round: dispatch with the spec as given",
		}, nil
	}

	last := rounds[len(rounds)-1]
	if last.Result != nil && len(last.Result.Results) > 0 {
		reasoning := "dispatched search returned offer(s)"
		if len(spec.SoftConstraints) > 0 {
			reasoning += fmt.Sprintf("; NOTE: %d soft constraint(s) recorded (%v) were NOT evaluated — "+
				"this is the stub decision function, not a real LLM call; see DESIGN.md 'LLM choice / call shape'",
				len(spec.SoftConstraints), spec.SoftConstraints)
		}
		return Decision{Action: ActionFinalize, Reasoning: reasoning}, nil
	}

	widened := last.Decision.Request
	widened.MaxHours *= 1.25
	widened.QueryBudget += 5
	return Decision{
		Action:    ActionDispatch,
		Request:   widened,
		Reasoning: fmt.Sprintf("round %d returned no offers; widening MaxHours to %.1f, QueryBudget to %d and retrying", last.Round, widened.MaxHours, widened.QueryBudget),
	}, nil
}

// DraftFinalEmail stands in for the LLM-drafting step DESIGN.md's
// "Components" section already names. Plain-text template for this first
// draft — real prose drafting is separate work from the loop mechanics
// this package exists to prove out.
func DraftFinalEmail(ctx context.Context, spec Spec, rounds []RoundRecord) (string, error) {
	var last *RoundRecord
	for i := len(rounds) - 1; i >= 0; i-- {
		if rounds[i].Result != nil {
			last = &rounds[i]
			break
		}
	}

	if last == nil || len(last.Result.Results) == 0 {
		return fmt.Sprintf(
			"We searched %s -> %s around %s but didn't find a feasible itinerary within %d round(s). "+
				"We're being upfront rather than presenting a partial answer as final — happy to keep looking if you can loosen a constraint.",
			spec.Origin, spec.Destination, spec.DepartDate, len(rounds)), nil
	}

	best := last.Result.Results[0]
	kind := "single-ticket"
	if best.SelfTransfer {
		kind = "separate tickets (self-transfer risk: no through checked bags, no rebooking if the first leg is delayed)"
	}
	return fmt.Sprintf(
		"Best option found for %s -> %s around %s: $%.0f, %dh%02dm via %v (%s). Found in %d round(s), %d total quer(y/ies) used.",
		spec.Origin, spec.Destination, spec.DepartDate, best.PriceUSD,
		best.DurationMinutes/60, best.DurationMinutes%60, best.Path, kind,
		len(rounds), last.Result.QueriesUsed), nil
}

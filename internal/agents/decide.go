package agents

import (
	"context"
	"encoding/json"
	"fmt"
)

// decideSystemPrompt is DESIGN.md "Agent loop" steps 2 and 4-5: choose
// the next tool call (with what arguments), ask the user, or finalize —
// judging each round's result against the spec's soft constraints, which
// concrete field checks (already enforced by routesearch itself) can't
// express.
const decideSystemPrompt = `You are the decision step of a flight-search agent loop. You are given the current Spec (concrete fields plus a plain-language SoftConstraints list) and every round dispatched so far, each with the arguments used and — once it ran — its result (a list of offers, each with PriceUSD, DurationMinutes, Path, and SelfTransfer).

Choose exactly one action:
  "dispatch": run one more search with a new set of concrete arguments. On round 1 (no rounds yet), dispatch using the Spec's concrete fields exactly as given — it already carries sensible values/defaults. On a retry, adjust arguments based on what the last round's result showed (e.g. raise QueryBudget or MaxHours to see more of the search frontier, tighten MaxPrice, etc).
  "ask_user": the request is genuinely underspecified — e.g. no departure date and nothing implying a date range, or an origin/destination too ambiguous to search — not just "could be narrower." Set "Question" to one clear clarifying question.
  "finalize": stop and hand back what's been found, or an honest "didn't find one that fits" if nothing qualifies.
Never choose "defer" — it exists in the type system but isn't wired to anything in this build.

Judge each round's result against BOTH halves of the spec: concrete fields are already enforced by the search itself (never re-check those — a result violating MaxHours/MaxPrice simply won't appear), but SoftConstraints are plain language only you can judge — e.g. "no self-transfer / separate tickets" is violated by any result with SelfTransfer:true. A result violating a soft constraint is NOT "good enough," even if it's the only or cheapest option found: dispatch again with adjusted arguments instead of finalizing, unless you're genuinely out of ideas for how to adjust — then finalize, and say plainly in Reasoning that a soft constraint went unmet.

For "dispatch", "Request" must be a JSON object with these fields (Go field names, exactly): Origin, Destination, DepartDate, ReturnDate (YYYY-MM-DD; "" = one-way), MaxHours (float), QueryBudget (int), MaxPrice (int USD, 0 = no cap), MinLayoverMinutes, MaxLayoverMinutes (int minutes).

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Action": "dispatch"|"ask_user"|"finalize", "Request": {...only for dispatch...}, "Question": "...only for ask_user...", "Reasoning": "one or two sentences, always present"}`

// DecideNextAction is DESIGN.md's agent-loop step 2, judgment call made
// by a real LLMClient call (replacing the earlier deterministic stub —
// see IMPLEMENTATION_PLAN.md Phase 2). Prints the full prompt/reply so
// whichever process calls this (cmd/agent-worker) shows the live
// decision transcript in its own terminal.
func DecideNextAction(ctx context.Context, llm LLMClient, spec Spec, rounds []RoundRecord) (Decision, error) {
	user := decideUserPrompt(spec, rounds)
	raw, err := llm.Chat(ctx, decideSystemPrompt, user)
	fmt.Printf("\n=== agents.DecideNextAction: LLM call (round %d) ===\n--- system ---\n%s\n--- user ---\n%s\n--- raw reply ---\n%s\n=====================================================\n\n",
		len(rounds)+1, decideSystemPrompt, user, raw)
	if err != nil {
		return Decision{}, fmt.Errorf("agents: LLM decide call: %w", err)
	}

	var reply struct {
		Action    string
		Request   *CollectRouteRequest
		Question  string
		Reasoning string
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return Decision{}, fmt.Errorf("agents: parsing LLM decision reply %q: %w", raw, err)
	}

	switch Action(reply.Action) {
	case ActionDispatch:
		if reply.Request == nil {
			return Decision{}, fmt.Errorf("agents: LLM chose dispatch with no Request (raw reply: %s)", raw)
		}
		return Decision{Action: ActionDispatch, Request: fillDispatchDefaults(*reply.Request, spec, rounds), Reasoning: reply.Reasoning}, nil
	case ActionAskUser:
		return Decision{Action: ActionAskUser, Question: reply.Question, Reasoning: reply.Reasoning}, nil
	case ActionFinalize:
		return Decision{Action: ActionFinalize, Reasoning: reply.Reasoning}, nil
	default:
		return Decision{}, fmt.Errorf("agents: LLM returned unknown action %q (raw reply: %s)", reply.Action, raw)
	}
}

// decideUserPrompt renders the spec plus round history as the JSON the
// system prompt tells the model to expect.
func decideUserPrompt(spec Spec, rounds []RoundRecord) string {
	view := struct {
		Spec        Spec
		RoundsSoFar []RoundRecord
	}{Spec: spec, RoundsSoFar: rounds}
	b, err := json.Marshal(view)
	if err != nil {
		// Spec/RoundRecord are always JSON-marshalable Go structs; a
		// failure here is a programming error, not bad input.
		panic(fmt.Sprintf("agents: marshaling decide prompt: %v", err))
	}
	return "Decide the next action for this request:\n\n" + string(b)
}

// fillDispatchDefaults treats a zeroed numeric field in the model's reply
// as "keep whatever was already in force," not "set it to zero" — a
// small local model sometimes omits a field the prompt asked for, and a
// silently zeroed MaxHours would make every offer infeasible. Falls back
// to the previous round's dispatched args, or the spec's own fields on
// round 1. ReturnDate is exempt: "" legitimately means one-way.
func fillDispatchDefaults(req CollectRouteRequest, spec Spec, rounds []RoundRecord) CollectRouteRequest {
	fallback := spec.toCollectRouteRequest()
	if len(rounds) > 0 {
		fallback = rounds[len(rounds)-1].Decision.Request
	}
	if req.Origin == "" {
		req.Origin = fallback.Origin
	}
	if req.Destination == "" {
		req.Destination = fallback.Destination
	}
	if req.DepartDate == "" {
		req.DepartDate = fallback.DepartDate
	}
	if req.MaxHours == 0 {
		req.MaxHours = fallback.MaxHours
	}
	if req.QueryBudget == 0 {
		req.QueryBudget = fallback.QueryBudget
	}
	if req.MaxPrice == 0 {
		req.MaxPrice = fallback.MaxPrice
	}
	if req.MinLayoverMinutes == 0 {
		req.MinLayoverMinutes = fallback.MinLayoverMinutes
	}
	if req.MaxLayoverMinutes == 0 {
		req.MaxLayoverMinutes = fallback.MaxLayoverMinutes
	}
	return req
}

// DraftFinalEmail stands in for the LLM-drafting step DESIGN.md's
// "Components" section already names. Plain-text template for this first
// draft — real prose drafting is separate work from the decision core
// this file exists to prove out.
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

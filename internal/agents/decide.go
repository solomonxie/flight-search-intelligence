package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// decideSystemPrompt is DESIGN.md "Agent loop" steps 2 and 4-5: choose
// the next tool call (with what arguments), ask the user, or finalize —
// judging each round's result against the spec's soft constraints, which
// concrete field checks (already enforced by routesearch itself) can't
// express.
const decideSystemPrompt = `You are the decision step of a flight-search agent loop. You are given the current Spec (concrete fields plus a plain-language SoftConstraints list) and every round dispatched so far, each with the arguments used and — once it ran — its result (a list of offers, each with PriceUSD, DurationMinutes, Path, and SelfTransfer).

The top-level "Spec" is always the current, up-to-date state — a follow-up may have filled in a field since an earlier round ran. Each entry in "RoundsSoFar" also carries its own "Spec" snapshot, but that's what the spec looked like *at that round*, not now: if round 1's snapshot shows Origin blank but the top-level Spec shows Origin set, the origin is known — do not re-ask a question an earlier round already asked if the top-level Spec now answers it.

The top-level Spec's LastIntent/LastIntentReasoning report what the most recent text was doing to it: "additional_info" just filled in a blank, "new_request" starts over — in either case a round whose own snapshot already matches the current Spec is still good. But "rewrite" means a field a prior round already searched under (TripType, Origin/Destination, a date range) was changed to a genuinely different value — that round's result answered the *old* request, not this one, so don't treat it as sufficient to finalize; dispatch a fresh round with the current Spec instead. "question_about_result" means the text isn't asking for a new search at all — it's a question about a result already on file, answerable from RoundsSoFar as-is; choose "finalize" (never "dispatch") so the reply step can answer it directly from context, unless the Spec genuinely still lacks something no prior round ever searched for.

Choose exactly one action:
  "dispatch": run one more search with a new set of concrete arguments. On round 1 (no rounds yet), dispatch using the Spec's concrete fields exactly as given — it already carries sensible values/defaults. On a retry, adjust arguments based on what the last round's result showed (e.g. raise QueryBudget or MaxHours to see more of the search frontier, tighten MaxPrice, etc). Origin/Destination may be a plain city name rather than an IATA code (e.g. "Beijing", not "PEK") — that's expected for a multi-airport city, not a gap to fill in yourself: pass it through unchanged, the dispatched search resolves it into every airport that city has and keeps the cheapest, so never invent or guess a specific airport code the Spec didn't already give you.
  "ask_user": the request is genuinely underspecified — e.g. no departure date and nothing implying a date range, TripType still unknown (see below — this is common, don't skip it), or a place name that isn't recognizable as any real city or airport — not just "could be narrower." Set "Question" to one message covering every unresolved thing at once (e.g. "What's your departure date, and is this one-way or round-trip?") — never ask about just one gap and save the rest for a later round; each round is a full round-trip with the traveler, so collect everything you need in it. Spec.Notes explains *why* a field is blank (e.g. an unresolved date range) — when a note exists for a blank field, ask about that note's specifics instead of a generic re-ask; a generic re-ask of a question the traveler already answered reads as the agent having ignored them.
  On the FIRST ask_user round only (RoundsSoFar has no earlier ask_user entry — don't repeat this on a later round, it'd read as nagging), also disclose what the search will otherwise assume: read Spec's own MaxHours/MaxPrice/MinLayoverMinutes/MaxLayoverMinutes/SearchRadiusKm (FormSpec has already filled these with real defaults, e.g. MaxHours 30, MaxPrice 0 meaning no cap) and add one sentence stating them as defaults the traveler can override, e.g. "If you have no preference, I'll default to up to 30 hours total travel time, no price cap, and layovers between 2 and 12 hours." — so an assumption they'd have wanted to change surfaces now, not only once they see the final result.
  "finalize": stop and hand back what's been found, or an honest "didn't find one that fits" if nothing qualifies.
Never choose "defer" — it exists in the type system but isn't wired to anything in this build.

Judge each round's result against BOTH halves of the spec: concrete fields are already enforced by the search itself (never re-check those — a result violating MaxHours/MaxPrice simply won't appear), but SoftConstraints are plain language only you can judge — e.g. "no self-transfer / separate tickets" is violated by any result with SelfTransfer:true (round-trip results: check OutboundSelfTransfer and ReturnSelfTransfer, either can be true independent of the other). A result violating a soft constraint is NOT "good enough," even if it's the only or cheapest option found: dispatch again with adjusted arguments instead of finalizing, unless you're genuinely out of ideas for how to adjust — then finalize, and say plainly in Reasoning that a soft constraint went unmet.

For "dispatch", "Request" must be a JSON object with these fields (Go field names, exactly): Origin, Destination, TripType ("one_way" or "round_trip" — must already be resolved, never ""), MinDepartDate, MaxDepartDate, MinReturnDate, MaxReturnDate (YYYY-MM-DD; Return* only set when TripType is "round_trip"), MinRoundTripDate, MaxRoundTripDate (YYYY-MM-DD, only if Spec has them — an absolute outer bound, e.g. limited leave), StepDays (int), MaxHours (float), QueryBudget (int), MaxPrice (int USD, 0 = no cap), MinLayoverMinutes, MaxLayoverMinutes (int minutes), CheckedBags (int, 0 = not mentioned), SearchRadiusKm (float; only matters when Origin/Destination is a city name, not a specific airport). Copy every one of these straight from the top-level Spec's own same-named field — never invent or narrow a date range yourself, that judgment call already happened in FormSpec; a From/To pair where From < To is a real range to search, not a mistake to collapse.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Action": "dispatch"|"ask_user"|"finalize", "Request": {...only for dispatch...}, "Question": "...only for ask_user...", "Reasoning": "one or two sentences, always present"}`

// DecideNextAction is DESIGN.md's agent-loop step 2, judgment call made
// by a real LLMClient call (replacing the earlier deterministic stub —
// see IMPLEMENTATION_PLAN.md Phase 2). Logs the full prompt/reply at
// Debug (LOG_LEVEL=debug — see log.go) and a one-line summary at Info,
// so the live decision is always visible without always showing the
// whole prompt behind it.
func DecideNextAction(ctx context.Context, llm LLMClient, spec Spec, rounds []RoundRecord) (Decision, error) {
	round := len(rounds) + 1
	user := decideUserPrompt(spec, rounds)
	var reply struct {
		Action    string
		Request   *CollectRouteRequest
		Question  string
		Reasoning string
	}
	raw, err := chatJSON(ctx, llm, decideSystemPrompt, user, &reply)
	debugLog.Debug("DecideNextAction LLM call", "round", round, "system", decideSystemPrompt, "user", user, "raw_reply", raw)
	if err != nil {
		return Decision{}, fmt.Errorf("agents: LLM decide call: %w", err)
	}

	switch Action(reply.Action) {
	case ActionDispatch:
		if reply.Request == nil {
			return Decision{}, fmt.Errorf("agents: LLM chose dispatch with no Request (raw reply: %s)", raw)
		}
		req := fillDispatchDefaults(*reply.Request, spec, rounds)
		// Whether Origin/Destination/DepartDate are set is mechanical,
		// not a judgment call — routesearch.Search cannot run without
		// them, so this is enforced here in Go rather than left to the
		// model to always remember (it doesn't always: a live run
		// dispatched twice with DepartDate still blank, burning two of
		// three redispatch rounds on a search that could never
		// succeed, before a third round finally asked for the date).
		if missing := missingRequiredFields(req); len(missing) > 0 {
			question := "What is " + joinMissing(missing) + "?"
			// spec.Notes explains *why* a field is blank (e.g. a named
			// but ambiguous multi-airport city) — surface it here too,
			// or this hardcoded fallback repeats the same generic
			// question the model's own ask_user branch was told to
			// avoid, undoing that fix whenever dispatch is the one that
			// gets overridden instead.
			if len(spec.Notes) > 0 {
				question += " (" + strings.Join(spec.Notes, "; ") + ")"
			}
			decision := Decision{
				Action:    ActionAskUser,
				Question:  question,
				Reasoning: fmt.Sprintf("chose dispatch, but %s still missing — a search can't run without them, so this asks for all of them in one round instead of spending a round per field on a request that can't succeed", joinMissing(missing)),
			}
			debugLog.Info("decided", "round", round, "action", decision.Action, "reasoning", decision.Reasoning, "overridden_from", "dispatch")
			return decision, nil
		}
		decision := Decision{Action: ActionDispatch, Request: req, Reasoning: reply.Reasoning}
		debugLog.Info("decided", "round", round, "action", decision.Action, "reasoning", decision.Reasoning)
		return decision, nil
	case ActionAskUser:
		if decision, ok := overrideBogusStop(spec, rounds, round, "ask_user"); ok {
			return decision, nil
		}
		decision := Decision{Action: ActionAskUser, Question: reply.Question, Reasoning: reply.Reasoning}
		debugLog.Info("decided", "round", round, "action", decision.Action, "reasoning", decision.Reasoning)
		return decision, nil
	case ActionFinalize:
		if decision, ok := overrideBogusStop(spec, rounds, round, "finalize"); ok {
			return decision, nil
		}
		decision := Decision{Action: ActionFinalize, Reasoning: reply.Reasoning}
		debugLog.Info("decided", "round", round, "action", decision.Action, "reasoning", decision.Reasoning)
		return decision, nil
	default:
		return Decision{}, fmt.Errorf("agents: LLM returned unknown action %q (raw reply: %s)", reply.Action, raw)
	}
}

// overrideBogusStop catches the mirror-image mistake to the dispatch-side
// guard above: the model choosing ask_user or finalize while the
// top-level Spec already has everything routesearch.Search needs and
// nothing has ever actually been dispatched — always wrong, since there's
// no legitimate reason to stop (for more info, or for a result) before
// even trying the search once. decideSystemPrompt already tells the model
// the top-level Spec is authoritative over a stale round snapshot, but a
// live run still finalized claiming "origin and departure date were not
// specified" when both were set and round 1 had only ever asked a
// question — never dispatched. Mechanical, not a judgment call, so
// enforced here rather than trusted to the model.
func overrideBogusStop(spec Spec, rounds []RoundRecord, round int, from string) (Decision, bool) {
	req := spec.toCollectRouteRequest()
	if len(missingRequiredFields(req)) > 0 {
		return Decision{}, false
	}
	for _, r := range rounds {
		if r.TaskID != "" {
			return Decision{}, false // a search has actually run before; the model's call to make
		}
	}
	decision := Decision{
		Action:    ActionDispatch,
		Request:   fillDispatchDefaults(req, spec, rounds),
		Reasoning: fmt.Sprintf("overriding %s: Spec already has origin/destination/depart date and no search has run yet", from),
	}
	debugLog.Info("decided", "round", round, "action", decision.Action, "reasoning", decision.Reasoning, "overridden_from", from)
	return decision, true
}

// missingRequiredFields reports every one of Origin/Destination/TripType/
// DepartDate(/ReturnDate) still unresolved in req, in the order a person
// would naturally be asked for them — nil once all are set (the only
// fields routesearch.Search cannot proceed without at all, TripType
// included: a blank ReturnDate is otherwise indistinguishable from "not
// asked yet" and a live run once silently searched one-way, only saying
// so after the fact, when the traveler was never actually asked which
// they meant). MaxHours/QueryBudget etc. always carry a usable value via
// fillDispatchDefaults, so they're not checked here. Collecting all of
// them, rather than just the first, lets the caller ask one combined
// question instead of burning a round per missing field.
func missingRequiredFields(req CollectRouteRequest) []string {
	var missing []string
	if req.Origin == "" {
		missing = append(missing, "your departure city or airport")
	}
	if req.Destination == "" {
		missing = append(missing, "your destination city or airport")
	}
	if req.MinDepartDate == "" {
		missing = append(missing, "your departure date")
	}
	switch req.TripType {
	case "":
		missing = append(missing, "whether this is one-way or round-trip")
	case "round_trip":
		if req.MinReturnDate == "" {
			missing = append(missing, "your return date")
		}
	}
	return missing
}

// joinMissing renders missing fields as "a", "a and b", or "a, b, and c" —
// one combined phrase to ask in a single question rather than one per
// field.
func joinMissing(missing []string) string {
	switch len(missing) {
	case 1:
		return missing[0]
	case 2:
		return missing[0] + " and " + missing[1]
	default:
		return strings.Join(missing[:len(missing)-1], ", ") + ", and " + missing[len(missing)-1]
	}
}

// decideUserPrompt renders the spec plus round history as the JSON the
// system prompt tells the model to expect.
func decideUserPrompt(spec Spec, rounds []RoundRecord) string {
	return "Decide the next action for this request:\n\n" + specAndRoundsJSON(spec, rounds)
}

// specAndRoundsJSON renders spec + round history as the JSON both
// DecideNextAction and DraftFinalEmail hand the LLM as "the current state
// of this request."
func specAndRoundsJSON(spec Spec, rounds []RoundRecord) string {
	view := struct {
		Spec        Spec
		RoundsSoFar []RoundRecord
	}{Spec: spec, RoundsSoFar: rounds}
	b, err := json.Marshal(view)
	if err != nil {
		// Spec/RoundRecord are always JSON-marshalable Go structs; a
		// failure here is a programming error, not bad input.
		panic(fmt.Sprintf("agents: marshaling spec/rounds: %v", err))
	}
	return string(b)
}

// fillDispatchDefaults treats a zeroed field in the model's reply as
// "keep whatever was already in force," not "set it to zero" — a small
// local model sometimes omits a field the prompt asked for, and a
// silently zeroed MaxHours would make every offer infeasible. Falls back
// to the previous round's dispatched args, or the spec's own fields on
// round 1. TripType/ReturnDate carry forward like everything else here —
// "" is never a legitimate resolved value for TripType (missingRequiredFields
// catches a genuinely blank one before dispatch is ever reached), so
// there's no case where forward-filling it from a prior round is wrong.
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
	if req.TripType == "" {
		req.TripType = fallback.TripType
	}
	if req.MinDepartDate == "" {
		req.MinDepartDate = fallback.MinDepartDate
	}
	if req.MaxDepartDate == "" {
		req.MaxDepartDate = fallback.MaxDepartDate
	}
	if req.MinReturnDate == "" {
		req.MinReturnDate = fallback.MinReturnDate
	}
	if req.MaxReturnDate == "" {
		req.MaxReturnDate = fallback.MaxReturnDate
	}
	if req.MinRoundTripDate == "" {
		req.MinRoundTripDate = fallback.MinRoundTripDate
	}
	if req.MaxRoundTripDate == "" {
		req.MaxRoundTripDate = fallback.MaxRoundTripDate
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
	if req.CheckedBags == 0 {
		req.CheckedBags = fallback.CheckedBags
	}
	if req.SearchRadiusKm == 0 {
		req.SearchRadiusKm = fallback.SearchRadiusKm
	}
	if req.StepDays == 0 {
		req.StepDays = fallback.StepDays
	}
	return req
}

// draftFinalEmailSystemPrompt is DESIGN.md "Components"' LLM-drafting
// idea: the reply a traveler actually reads once the loop stops, not a
// re-statement of the Spec.
const draftFinalEmailSystemPrompt = `You write the final reply to a traveler whose flight-search agent loop just stopped — either it found something worth presenting, or it's giving up honestly without one. You're given the Spec (what was asked for) and every round dispatched so far, each with its arguments, its result once it ran, and the reasoning behind the decision that produced it.

Write the literal reply text — plain prose, no markdown, a few sentences, no filler ("I hope this helps," restating the whole Spec back at them). Write like a person telling a friend what they found, not a system describing its own fields — no jargon like "bundled fare," "self-transfer risk," or field names; say what things mean in plain words:
  - Lead with the outcome: the best itinerary's price, duration, and route — or an honest "didn't find one that fits" if every round came back empty.
  - Only bring up how the tickets are booked when it actually matters: if it's two separate tickets (Bundled:false), say so plainly and explain the real risk in a sentence (if the first flight runs late, you could miss the second one, and there's no automatic rebooking or through checked bags). If it's one ticket (Bundled:true), there's nothing to warn about — a brief, natural mention ("this is one ticket for the whole round trip") is enough, don't dwell on it.
  - Check the last round's Reasoning for an unmet soft constraint; if there is one, say plainly that it wasn't satisfied rather than presenting the result as fully matching what was asked.
  - MaxPrice 0 means no price cap was set — never describe it as "a $0 budget" or similar; only mention MaxPrice at all when it's nonzero. QueryBudget is a completely different thing and never a dollar figure — it's how many candidate searches the algorithm was allowed to try, not a price; never say a result "exceeds" or "meets" QueryBudget, and never call it "your $N budget."
  - If Spec.TripType is "round_trip": quote TotalPriceUSD as the trip's price, never the top-level PriceUSD (that field describes the outbound leg alone, and is 0 when it's one ticket — Bundled:true means Google priced the whole round trip as a single fare, cheaper than outbound and return separately, so there's no separate outbound price to give). Under Bundled:true, PriceUSD and DurationMinutes are both 0 and mean nothing — never state "$0" or "0 minutes"; just don't mention a per-leg price or duration at all in that case. Under Bundled:false, each direction has its own self-transfer risk (OutboundSelfTransfer / ReturnSelfTransfer, independent of each other) — only warn about the direction(s) actually flagged true.
  - If a round's Result has ChosenDepartDate (a flexible-date search, where MinDepartDate < MaxDepartDate or MinReturnDate < MaxReturnDate): that's the actual date being priced, and it can differ from the requested range's own endpoints — say plainly which date won and, if it wasn't the range's first day, that it was the cheapest one found within the requested window (ChosenReturnDate too, for a round trip).
  - If Spec.LastIntent is "question_about_result": the traveler asked a question about a result already given, not for a new outcome restated from scratch — answer that question directly from RoundsSoFar, and only re-lead with price/route/duration if the question was actually about those.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Email": "the final reply, plain prose"}`

// DraftFinalEmail is DESIGN.md's finalize-step LLM call — replacing the
// earlier fixed Sprintf template, which could only ever restate Spec
// fields and never actually addressed anything the traveler said (e.g. a
// follow-up asking why no return date was requested got the same canned
// price/route sentence back, unchanged).
func DraftFinalEmail(ctx context.Context, llm LLMClient, spec Spec, rounds []RoundRecord) (string, error) {
	user := "Write the final reply for this request:\n\n" + specAndRoundsJSON(spec, rounds)
	var reply struct{ Email string }
	raw, err := chatJSON(ctx, llm, draftFinalEmailSystemPrompt, user, &reply)
	debugLog.Debug("DraftFinalEmail LLM call", "system", draftFinalEmailSystemPrompt, "user", user, "raw_reply", raw)
	if err != nil {
		return "", fmt.Errorf("agents: LLM draft-final-email call: %w", err)
	}
	if reply.Email == "" {
		return "", fmt.Errorf("agents: LLM final-email reply had empty Email (raw reply: %s)", raw)
	}
	return reply.Email, nil
}

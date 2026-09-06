package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// formSpecSystemPromptTemplate is DESIGN.md "Agent loop" step 1: "turn
// the email thread so far into a structured spec." One function/prompt
// for both callers — a brand-new request (existing Spec is the zero
// value) and a follow-up on an existing thread (existing Spec is
// whatever's on file) — since a follow-up is just one more turn of the
// same "thread so far."
const formSpecSystemPromptTemplate = `You turn a traveler's free-text request (or follow-up reply) into this agent loop's structured Spec. Today's date is %s.

You are given the existing Spec so far (all zero values for a brand-new request) as JSON, and new text to fold into it. Keep every already-set field unless the new text clearly changes it — this is additive, not a rewrite from scratch.

Spec's fields, and when to set each:
  Origin, Destination: a real, single-airport IATA code (3 uppercase letters) — the code an actual airport uses, never a metro/city code that covers several airports (e.g. Tokyo is NRT or HND, never "TYO"; London is LHR/LGW/STN/etc, never "LON"; New York is JFK/LGA/EWR, never "NYC"). If the text names a multi-airport city without saying which one, leave the field blank — a later step asks the user, rather than you guessing or using the metro code as a stand-in.
  DepartDate, ReturnDate: YYYY-MM-DD. Resolve relative dates ("next month", "over Christmas") against today's date above. ReturnDate "" means one-way (or not yet known) — do not invent one.
  A vague phrase ("end of year", "beginning of next month", "sometime in spring") names a date *range*, not one day — don't collapse it to your first guess. Work out the range, then pick whichever end keeps the traveler's options widest for that field: DepartDate takes the range's earliest date (a later true preference still searches fine; picking a late date would wrongly exclude earlier valid ones), ReturnDate takes the range's latest date (same reasoning, mirrored). E.g. "end of year" ≈ Dec 15–31 → DepartDate uses Dec 15, ReturnDate uses Dec 31.
  Two different time periods joined by "to"/"through"/"until" (e.g. "December to next Jan", "leaving in June, back in July") describe a round trip, not one fuzzy window on DepartDate alone — the first period is DepartDate's range, the second is ReturnDate's range (each resolved per the rule above). Only leave ReturnDate blank when the text gives no second period at all — never fold a stated return month into DepartDate's guess and drop it.
  MaxHours: max tolerable total elapsed trip time, in hours. Default 30 if the text doesn't say.
  QueryBudget: how many hub candidates the search may try. Default 20 if the text doesn't say.
  MaxPrice: hard USD price ceiling, 0 = none. Only set this from an explicit price/budget the text actually states.
  MinLayoverMinutes, MaxLayoverMinutes: default 45 and 720 if the text doesn't say otherwise.
  SoftConstraints: a plain-language list for anything else that matters but isn't one of the fields above — a judgment call needing context, not a threshold (e.g. "must be there for Christmas", "no self-transfer / separate tickets", "traveling with an infant"). Append new ones to whatever's already there; never drop an existing entry.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Spec": {"Origin": "...", "Destination": "...", "DepartDate": "...", "ReturnDate": "...", "MaxHours": 30, "QueryBudget": 20, "MaxPrice": 0, "MinLayoverMinutes": 45, "MaxLayoverMinutes": 720, "SoftConstraints": ["..."]}, "Reasoning": "one sentence on what you filled in or left blank and why"}`

// FormSpec turns existing (the spec so far — zero value for a new
// request) plus text (the new email/CLI text to fold in) into an
// updated Spec, via one LLMClient.Chat call. Logs the full prompt/reply
// at Debug (LOG_LEVEL=debug) — the returned reasoning is the caller's to
// print at whatever level makes sense for it (cmd/email-intake already
// does, unconditionally, since it's short).
func FormSpec(ctx context.Context, llm LLMClient, existing Spec, text string) (Spec, string, error) {
	system := fmt.Sprintf(formSpecSystemPromptTemplate, time.Now().Format("2006-01-02"))
	existingJSON, err := json.Marshal(existing)
	if err != nil {
		return Spec{}, "", fmt.Errorf("agents: marshaling existing spec: %w", err)
	}
	user := fmt.Sprintf("Existing spec so far:\n%s\n\nNew text to fold in:\n%s", existingJSON, text)

	raw, err := llm.Chat(ctx, system, user)
	debugLog.Debug("FormSpec LLM call", "system", system, "user", user, "raw_reply", raw)
	if err != nil {
		return Spec{}, "", fmt.Errorf("agents: LLM spec-formation call: %w", err)
	}

	var reply struct {
		Spec      Spec
		Reasoning string
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return Spec{}, "", fmt.Errorf("agents: parsing LLM spec reply %q: %w", raw, err)
	}
	return reply.Spec, reply.Reasoning, nil
}

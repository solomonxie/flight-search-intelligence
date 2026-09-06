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
  Origin, Destination: a real, single-airport IATA code (3 uppercase letters) — the code an actual airport uses, never a metro/city code that covers several airports (e.g. Tokyo is NRT or HND, never "TYO"; London is LHR/LGW/STN/etc, never "LON"; New York is JFK/LGA/EWR, never "NYC") — ONLY when the text names or clearly implies one specific airport. Otherwise, when only a city is named — including a multi-airport city like Beijing or London — set the field to that city's plain name instead (e.g. "Beijing", "London"): a later step searches every airport in that city and keeps whichever comes back cheapest, so don't guess a specific airport the text didn't ask for, and don't leave the field blank just because the city has more than one.
  DepartDate, ReturnDate: YYYY-MM-DD. Resolve relative dates ("next month", "over Christmas") against today's date above. ReturnDate "" means one-way (or not yet known) — do not invent one.
  A vague phrase ("end of year", "beginning of next month", "sometime in spring") names a date *range*, not one day — don't collapse it to your first guess. Work out the range, then pick whichever end keeps the traveler's options widest for that field: DepartDate takes the range's earliest date (a later true preference still searches fine; picking a late date would wrongly exclude earlier valid ones), ReturnDate takes the range's latest date (same reasoning, mirrored). E.g. "end of year" ≈ Dec 15–31 → DepartDate uses Dec 15, ReturnDate uses Dec 31.
  Read the whole message for a SECOND time expression before resolving DepartDate — don't stop at the first one you find and reuse it for ReturnDate too. A second period shows up two ways, both meaning "this one is ReturnDate's own range, not DepartDate's": joined directly ("December to next Jan", "leaving in June, back in July"), or introduced anywhere else in the text by an explicit return/round-trip marker ("round trip", "return in/on/around X", "back by X") — e.g. "round trip, end of year, return in next jan" has TWO periods (end of year; next Jan), not one: DepartDate resolves "end of year" alone, ReturnDate resolves "next Jan" alone (each per the range rule above) — reusing "end of year" for both is wrong even though only one range appears near the start of the message. Only leave ReturnDate blank when the text names no second period and no return/round-trip marker at all.
  MaxHours: max tolerable total elapsed trip time, in hours. Default 30 if the text doesn't say.
  QueryBudget: how many hub candidates the search may try. Default 20 if the text doesn't say.
  MaxPrice: hard USD price ceiling, 0 = none. Only set this from an explicit price/budget the text actually states.
  MinLayoverMinutes, MaxLayoverMinutes: default 45 and 720 if the text doesn't say otherwise.
  SearchRadiusKm: how far (km) around a named city's center to also consider alternate airports — only matters when Origin/Destination is a city name rather than one specific airport. Default 100 if the text doesn't say; only override this from an explicit distance the traveler states (e.g. "within 50km of Beijing", "airports up to 200km out are fine").
  SoftConstraints: a plain-language list for anything else that matters but isn't one of the fields above — a judgment call needing context, not a threshold (e.g. "must be there for Christmas", "no self-transfer / separate tickets", "traveling with an infant"). Append new ones to whatever's already there; never drop an existing entry.
  Notes: a plain-language list, one entry per field you left genuinely blank *for a specific reason* the next step needs in order to ask a sharp follow-up instead of a generic one (e.g. an unresolved date range, or a place name that isn't a recognizable city or airport at all). A named multi-airport city is NOT one of these — that goes in Origin/Destination as the city name, per above, not a blank field with a note. Unlike SoftConstraints, Notes is not permanent: once the new text resolves what a note was about, drop that note — keep only notes still unresolved.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Spec": {"Origin": "...", "Destination": "...", "DepartDate": "...", "ReturnDate": "...", "MaxHours": 30, "QueryBudget": 20, "MaxPrice": 0, "MinLayoverMinutes": 45, "MaxLayoverMinutes": 720, "SearchRadiusKm": 100, "SoftConstraints": ["..."], "Notes": ["..."]}, "Reasoning": "one sentence on what you filled in or left blank and why"}`

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

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
  TripType: "one_way", "round_trip", or "" if the text gives no signal either way. Set this ONLY from an explicit signal — "round trip", "return", "back by/on/around X", a second distinct travel date, or (for one_way) "one-way"/"single trip"/"not coming back". A departure date alone, however specific or vague, is NOT a signal either way: never infer one_way just because no return was mentioned, and never infer round_trip just because a date phrase happens to span a range — leave TripType "" and let a later step ask, rather than guessing silently (a live run once turned an answer to "what's your departure date?" of "end of year" into an invented Dec 31 return date it was never asked for).
  DepartDate, ReturnDate: YYYY-MM-DD. Resolve relative dates ("next month", "over Christmas") against today's date above. Only ever set ReturnDate when TripType is (or becomes, from this text) "round_trip" — if TripType is "" or "one_way", ReturnDate stays "" regardless of what DepartDate's own phrase looks like.
  A vague phrase ("end of year", "beginning of next month", "sometime in spring") names a date *range*, not one day — don't collapse it to your first guess. Work out the range, then take its earliest date for DepartDate (a later true preference still searches fine; picking a late date would wrongly exclude earlier valid ones). E.g. "end of year" ≈ Dec 15–31 → DepartDate uses Dec 15. Never resolve ReturnDate from this same phrase or range, even when this is a round trip — see next.
  When TripType is "round_trip", ReturnDate must come from its own, distinct time expression found elsewhere in the text — never DepartDate's own phrase or range reused. A second period shows up two ways: joined directly ("December to next Jan", "leaving in June, back in July"), or introduced anywhere else in the text by an explicit return marker ("return in/on/around X", "back by X") — e.g. "round trip, end of year, return in next jan" has TWO periods (end of year; next Jan): DepartDate resolves "end of year" alone, ReturnDate resolves "next Jan" alone (each per the range rule above). If the text gives only ONE time expression total — e.g. "vancouver, end of year, round trip" has just the one range, nothing marking a return — ReturnDate stays "" and Notes gets an entry saying so (e.g. "round trip, but no return date given — only a departure window was mentioned"), so the next step asks for it explicitly instead of inventing one out of the departure range (a live run once did exactly that: "end of year" round trip silently became Dec 15 out / Dec 31 back, a return date never actually given).
  WindowDays, StepDays: 0 (the default) means an exact date search. Only set WindowDays > 0 from an EXPLICIT flexibility signal — the traveler saying they don't care which exact day, want the cheapest day, or naming a +/- range around a date ("I'm flexible", "any day that week works", "whichever's cheapest", "the 15th, give or take 3 days"). This is a different thing from a vague date phrase like "end of year": that's still DepartDate's own earliest-date rule above (a single guess to search), not a signal to scan a window — don't set WindowDays just because DepartDate came from a vague phrase. When you do set WindowDays, set it to how many days on each side of DepartDate to scan (e.g. "give or take 3 days" -> 3; "sometime within that week" -> 3 as a reasonable week-ish default); StepDays only matters if the traveler asked for coarser sampling than daily (e.g. "check every few days") — leave it 0 (daily) otherwise.
  MaxHours: max tolerable total elapsed trip time, in hours. Default 30 if the text doesn't say.
  QueryBudget: how many hub candidates the search may try. Default 20 if the text doesn't say.
  MaxPrice: hard USD price ceiling, 0 = none. Only set this from an explicit price/budget the text actually states.
  MinLayoverMinutes, MaxLayoverMinutes: default 45 and 720 if the text doesn't say otherwise.
  SearchRadiusKm: how far (km) around a named city's center to also consider alternate airports — only matters when Origin/Destination is a city name rather than one specific airport. Default 100 if the text doesn't say; only override this from an explicit distance the traveler states (e.g. "within 50km of Beijing", "airports up to 200km out are fine").
  SoftConstraints: a plain-language list for anything else that matters but isn't one of the fields above — a judgment call needing context, not a threshold (e.g. "must be there for Christmas", "no self-transfer / separate tickets", "traveling with an infant"). Append new ones to whatever's already there; never drop an existing entry.
  Notes: a plain-language list, one entry per field you left genuinely blank *for a specific reason* the next step needs in order to ask a sharp follow-up instead of a generic one (e.g. an unresolved date range, or a place name that isn't a recognizable city or airport at all). A named multi-airport city is NOT one of these — that goes in Origin/Destination as the city name, per above, not a blank field with a note. Unlike SoftConstraints, Notes is not permanent: once the new text resolves what a note was about, drop that note — keep only notes still unresolved.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Spec": {"Origin": "...", "Destination": "...", "TripType": "one_way"|"round_trip"|"", "DepartDate": "...", "ReturnDate": "...", "MaxHours": 30, "QueryBudget": 20, "MaxPrice": 0, "MinLayoverMinutes": 45, "MaxLayoverMinutes": 720, "SearchRadiusKm": 100, "WindowDays": 0, "StepDays": 0, "SoftConstraints": ["..."], "Notes": ["..."]}, "Reasoning": "one sentence on what you filled in or left blank and why"}`

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

	var reply struct {
		Spec      Spec
		Reasoning string
	}
	raw, err := chatJSON(ctx, llm, system, user, &reply)
	debugLog.Debug("FormSpec LLM call", "system", system, "user", user, "raw_reply", raw)
	if err != nil {
		return Spec{}, "", fmt.Errorf("agents: LLM spec-formation call: %w", err)
	}
	return normalizeDefaults(reply.Spec, existing), reply.Reasoning, nil
}

// Defaults formSpecSystemPromptTemplate documents and asks the model to
// apply itself — kept here too because it doesn't always reliably do so
// (a live run left MaxHours/MinLayoverMinutes/etc. all zeroed despite the
// prompt's explicit instruction). None of these fields has a real "0"
// meaning — even MinLayoverMinutes:0 was never a deliberate answer, just
// "the model didn't fill it in" — so leaving them at 0 isn't just
// cosmetic: MaxHours:0 would make every future search reject every
// result as too slow, and it'd leave nothing real for the ask_user
// question to disclose as "here's what I'll default to."
const (
	defaultMaxHours          = 30
	defaultQueryBudget       = 20
	defaultMinLayoverMinutes = 45
	defaultMaxLayoverMinutes = 720
	defaultSearchRadiusKm    = 100
)

// normalizeDefaults forward-fills a zeroed numeric field from existing
// (the spec so far) or, failing that, the hardcoded default above — the
// same "treat zero as omitted, not deliberate" fallback
// fillDispatchDefaults already applies one step later (at dispatch time),
// applied here too so a Spec never sits with an unfilled default in the
// meantime. MaxPrice is deliberately excluded: 0 there is its own
// documented, legitimate value ("no cap"), never "not filled in."
func normalizeDefaults(s, existing Spec) Spec {
	if s.MaxHours == 0 {
		s.MaxHours = existing.MaxHours
	}
	if s.MaxHours == 0 {
		s.MaxHours = defaultMaxHours
	}
	if s.QueryBudget == 0 {
		s.QueryBudget = existing.QueryBudget
	}
	if s.QueryBudget == 0 {
		s.QueryBudget = defaultQueryBudget
	}
	if s.MinLayoverMinutes == 0 {
		s.MinLayoverMinutes = existing.MinLayoverMinutes
	}
	if s.MinLayoverMinutes == 0 {
		s.MinLayoverMinutes = defaultMinLayoverMinutes
	}
	if s.MaxLayoverMinutes == 0 {
		s.MaxLayoverMinutes = existing.MaxLayoverMinutes
	}
	if s.MaxLayoverMinutes == 0 {
		s.MaxLayoverMinutes = defaultMaxLayoverMinutes
	}
	if s.SearchRadiusKm == 0 {
		s.SearchRadiusKm = existing.SearchRadiusKm
	}
	if s.SearchRadiusKm == 0 {
		s.SearchRadiusKm = defaultSearchRadiusKm
	}
	return s
}

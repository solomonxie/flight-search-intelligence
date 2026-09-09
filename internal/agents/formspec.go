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
//
// A few of its rules exist because a live run got them wrong once —
// kept terse in the prompt itself (the model needs the rule, not the
// backstory), noted here for maintainers: TripType was once silently
// inferred as round_trip from a date range alone ("end of year" -> an
// invented return date never asked for); a round trip's return window
// was once reused from the departure phrase itself ("end of year" round
// trip -> Dec 15 out / Dec 31 back, no real return date given); and
// "next <month>" once resolved to this year's already-passed occurrence
// instead of next year's (see validateDates' doc comment for that one's
// mechanical backstop, below).
const formSpecSystemPromptTemplate = `You turn a traveler's free-text request (or follow-up reply) into this agent loop's structured Spec. Today's date is %s.

You are given the existing Spec so far (all zero values for a brand-new request) as JSON, and new text to fold into it. Answer in two parts, in this order:

PART 1 — Intention: classify what this new text is doing, relative to the existing Spec:
  "new_request": this is the first message of a request, or describes a trip unrelated to anything already on file (a different origin/destination/trip entirely, not just a changed date or trip type on the same one).
  "additional_info": fills in a field that was blank, or narrows a range that was wide — doesn't contradict any field that was already set.
  "rewrite": sets an already-set field to a genuinely different value — e.g. "actually make it one-way" when TripType was "round_trip", a new destination replacing the old one, a date that supersedes rather than narrows the existing range. This matters downstream: a round already searched under the old value is no longer a valid answer to the rewritten request.
  "question_about_result": the text isn't asking for a (new) search at all — it's a question about a result already given (e.g. "why no direct flights", "how long is the layover on that one"), answerable from the existing Spec/rounds without changing the Spec.

PART 2 — Info: fold the new text into the Spec. Keep every already-set field unless the new text clearly changes it — this is additive, not a rewrite from scratch (an "additional_info" turn never touches an already-set field; a "rewrite" turn changes exactly the field(s) the text actually contradicts, leaving the rest alone).

Spec's fields, and when to set each:
  Origin, Destination: a real, single-airport IATA code (3 uppercase letters) — the code an actual airport uses, never a metro/city code that covers several airports (e.g. Tokyo is NRT or HND, never "TYO"; London is LHR/LGW/STN/etc, never "LON"; New York is JFK/LGA/EWR, never "NYC") — ONLY when the text names or clearly implies one specific airport. Otherwise, when only a city is named — including a multi-airport city like Beijing or London — set the field to that city's plain name instead (e.g. "Beijing", "London"): a later step searches every airport in that city and keeps whichever comes back cheapest, so don't guess a specific airport the text didn't ask for, and don't leave the field blank just because the city has more than one.
  TripType: "one_way", "round_trip", or "" if the text gives no signal either way. Set this ONLY from an explicit signal — "round trip", "return", "back by/on/around X", a second distinct travel date, or (for one_way) "one-way"/"single trip"/"not coming back". A departure date alone, however specific or vague, is NOT a signal either way: never infer one_way just because no return was mentioned, and never infer round_trip just because a date phrase happens to span a range — leave TripType "" and let a later step ask, rather than guessing silently.
  MinDepartDate, MaxDepartDate: YYYY-MM-DD, inclusive — the window departure may fall in. Resolve relative dates ("next month", "over Christmas") against today's date above. An exact date ("December 15th") sets both to the same value. A vague phrase ("end of year", "sometime in spring") genuinely names a *range*, not one day — don't collapse it to a single guess or invent a specific canonical definition; work out a reasonable span for what was actually said and set MinDepartDate/MaxDepartDate to its two ends (e.g. "end of year" might reasonably span roughly the back half of December — exact bounds depend on context, this is an estimate, not a rule to apply identically every time).
  "next <month name>" means the NEXT calendar occurrence of that month strictly after today — if that month number is <= today's month number, it's already happened this year, so it means that month IN THE FOLLOWING YEAR, not this year's (already-past) one. E.g. today 2026-09-07, "next Jan" -> 2027-01 (January 2026 already happened 8 months ago); today 2026-09-07, "next Nov" -> 2026-11 (November hasn't happened yet this year).
  MinReturnDate, MaxReturnDate: only ever set when TripType is (or becomes, from this text) "round_trip" — stay "" regardless of MinDepartDate/MaxDepartDate otherwise. Must come from their own, distinct time expression found elsewhere in the text — never the departure phrase or range reused. A second period shows up two ways: joined directly ("December to next Jan", "leaving in June, back in July"), or introduced anywhere else in the text by an explicit return marker ("return in/on/around X", "back by X") — e.g. "round trip, end of year, return in next jan" has TWO periods: departure resolves "end of year" alone, return resolves "next Jan" alone (each per the range rule above). If the text gives only ONE time expression total for a round trip — e.g. "vancouver, end of year, round trip" — that's a single combined window for the whole trip, not separately for each leg: leave MinReturnDate/MaxReturnDate exactly as they already were (blank, on a first message) and add a Note saying a return window is still needed (e.g. "round trip, but no return window given — only a departure window was mentioned"), rather than inventing a split of the one range into two.
  A round trip's return must never be resolved to land on or before the departure window — if straightforward date arithmetic would do that (e.g. a "next <month>" miscount), the intended year is next year instead.
  MinRoundTripDate, MaxRoundTripDate (round_trip only): a hard outer bound both the departure and return date must fall within — set this ONLY from an explicit, absolute constraint on the whole trip's length or deadline (e.g. "I only have a month of paid leave", "I must be back by end of January no matter what", "the whole trip can't be more than 3 weeks"), never from an ordinary vague date phrase (that's MinDepartDate/MaxDepartDate's or MinReturnDate/MaxReturnDate's own job above). If enough is already known to compute real dates (e.g. MinDepartDate is set and the text says "one month"), set MinRoundTripDate/MaxRoundTripDate to actual YYYY-MM-DD values; if not enough is known yet, leave both "" and add a Note describing the constraint so the next step can ask what's missing. This exists so an absolute limit (limited leave, a hard deadline) is never silently violated by treating a wide departure/return window as if any combination within it were acceptable.
  StepDays: sample every StepDays within a date range above; 0 (the default) means every day. Only raise this from an explicit request for coarser sampling ("check every few days") — never as a way to narrow a range you're unsure about; leaving a range wide with StepDays 0 is always safer than guessing a narrower one.
  MinTripLengthDays, MaxTripLengthDays (round_trip only): a trip-length tolerance — "about a week", "10-14 days", "two weeks give or take a few days" — as an ALTERNATIVE to MinReturnDate/MaxReturnDate, never both: set this pair ONLY when the text describes the trip's *duration* rather than naming (even vaguely) a *return time period*. A duration alone with no depart window yet isn't enough to compute real dates — leave both "" and add a Note if MinDepartDate/MaxDepartDate aren't both resolved yet; once they are, set MinTripLengthDays/MaxTripLengthDays to the actual day counts described (a single length like "a week" sets both to 7, i.e. no tolerance). If the text instead names or implies a return time period (any of MinReturnDate's own trigger phrases above), that's MinReturnDate/MaxReturnDate's job, not this one.
  TripLengthStepDays: sample every TripLengthStepDays within the trip-length tolerance range above; 0 (the default) means every day. Same "only raise from an explicit ask" rule as StepDays.
  MaxCountries: cap on how many distinct countries a connecting itinerary may transit, 0 = no cap. Only set this from an explicit count the text actually states (e.g. "no more than 2 countries along the way").
  ExcludedCountries: country names (as commonly spelled in English) a layover/connection must never be in — e.g. "avoid Russia as a layover", "nothing through China". Append new ones to whatever's already there; never drop an existing entry.
  BlackoutDates: YYYY-MM-DD dates a chosen depart or return date must never land on — e.g. "not around Christmas" resolves to the specific date(s) implied (Dec 24-25), "avoid New Year's Day" resolves to Jan 1 of the relevant year. Resolve a named holiday/occasion to its actual date(s) the same way MinDepartDate's vague-phrase rule resolves "end of year" to a range. Append new ones to whatever's already there; never drop an existing entry.
  MaxHours: max tolerable total elapsed trip time, in hours. Default 30 if the text doesn't say.
  QueryBudget: how many hub candidates the search may try. Default 20 if the text doesn't say.
  MaxPrice: hard USD price ceiling, 0 = none. Only set this from an explicit price/budget the text actually states.
  MinLayoverMinutes, MaxLayoverMinutes: default 120 and 720 (2 hours and 12 hours — comfortable, not rushed) if the text doesn't say otherwise.
  CheckedBags: number of checked bags for the whole trip. 0 (the default) means the text never mentioned bags — only set this from an explicit count ("2 checked bags", "traveling with a suitcase" -> 1).
  SearchRadiusKm: how far (km) around a named city's center to also consider alternate airports — only matters when Origin/Destination is a city name rather than one specific airport. Default 100 if the text doesn't say; only override this from an explicit distance the traveler states (e.g. "within 50km of Beijing", "airports up to 200km out are fine").
  SoftConstraints: a plain-language list for anything else that matters but isn't one of the fields above — a judgment call needing context, not a threshold (e.g. "must be there for Christmas", "no self-transfer / separate tickets", "traveling with an infant"). Append new ones to whatever's already there; never drop an existing entry.
  Notes: a plain-language list, one entry per field you left genuinely blank *for a specific reason* the next step needs in order to ask a sharp follow-up instead of a generic one (e.g. an unresolved date range, or a place name that isn't a recognizable city or airport at all). A named multi-airport city is NOT one of these — that goes in Origin/Destination as the city name, per above, not a blank field with a note. Unlike SoftConstraints, Notes is not permanent: once the new text resolves what a note was about, drop that note — keep only notes still unresolved.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences — Intention and Info as their own nested objects, each with its own Reasoning:
{"Intention": {"Type": "new_request"|"additional_info"|"rewrite"|"question_about_result", "Reasoning": "one sentence on why this text reads as that intention"}, "Info": {"Spec": {"Origin": "...", "Destination": "...", "TripType": "one_way"|"round_trip"|"", "MinDepartDate": "...", "MaxDepartDate": "...", "MinReturnDate": "...", "MaxReturnDate": "...", "MinRoundTripDate": "...", "MaxRoundTripDate": "...", "StepDays": 0, "MinTripLengthDays": 0, "MaxTripLengthDays": 0, "TripLengthStepDays": 0, "MaxHours": 30, "QueryBudget": 20, "MaxPrice": 0, "MinLayoverMinutes": 120, "MaxLayoverMinutes": 720, "CheckedBags": 0, "SearchRadiusKm": 100, "MaxCountries": 0, "ExcludedCountries": ["..."], "BlackoutDates": ["..."], "SoftConstraints": ["..."], "Notes": ["..."]}, "Reasoning": "one sentence on what you filled in, changed, or left blank and why"}}`

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
		Intention struct {
			Type      Intent
			Reasoning string
		}
		Info struct {
			Spec      Spec
			Reasoning string
		}
	}
	raw, err := chatJSON(ctx, llm, system, user, &reply)
	debugLog.Debug("FormSpec LLM call", "system", system, "user", user, "raw_reply", raw)
	if err != nil {
		return Spec{}, "", fmt.Errorf("agents: LLM spec-formation call: %w", err)
	}
	spec := validateDates(normalizeDefaults(reply.Info.Spec, existing))
	spec.LastIntent = reply.Intention.Type
	spec.LastIntentReasoning = reply.Intention.Reasoning
	return spec, reply.Info.Reasoning, nil
}

// validateDates catches date arithmetic the model got wrong rather than
// trusting it to always be right — a live run had "next jan" (today in
// September) resolve to *this* January instead of next year's, landing
// ReturnDate almost a year *before* DepartDate; the bad Spec still
// dispatched, burning dozens of queries (every multi-airport candidate
// pair, both directions) before finalizing a literally backwards "round
// trip." Two mechanical checks, neither a judgment call:
//   - each of DepartDate/ReturnDate/RoundTrip's own From/To is swapped
//     back into order if the model emitted them backwards;
//   - a round trip's return window landing on or before the departure
//     window is never legitimate, so it's cleared back to "" (with a
//     Note explaining why, same convention as any other field
//     DecideNextAction needs to ask about again) rather than letting a
//     bad range silently reach dispatch.
func validateDates(s Spec) Spec {
	s.MinDepartDate, s.MaxDepartDate = orderedRange(s.MinDepartDate, s.MaxDepartDate)
	s.MinReturnDate, s.MaxReturnDate = orderedRange(s.MinReturnDate, s.MaxReturnDate)
	s.MinRoundTripDate, s.MaxRoundTripDate = orderedRange(s.MinRoundTripDate, s.MaxRoundTripDate)

	if s.TripType == "round_trip" && s.MaxDepartDate != "" && s.MinReturnDate != "" && s.MinReturnDate <= s.MaxDepartDate {
		s.MinReturnDate, s.MaxReturnDate = "", ""
		s.Notes = append(s.Notes, fmt.Sprintf("return window resolved on or before the departure window (ends %s) — likely a year miscount on a relative date; needs to be asked again", s.MaxDepartDate))
	}

	// MinReturnDate/MaxReturnDate (an independent return window) and
	// MinTripLengthDays/MaxTripLengthDays (a trip-length tolerance coupled
	// to the depart window) are mutually exclusive ways of expressing the
	// same "when do I come back" question — never both at once. The
	// return-date range is kept as the more specific of the two (an
	// explicit window beats a length inferred from one), same "mechanical
	// backstop, not a judgment call" reasoning as the backwards-window
	// check above.
	if s.TripType == "round_trip" && s.MinReturnDate != "" && (s.MinTripLengthDays != 0 || s.MaxTripLengthDays != 0) {
		s.MinTripLengthDays, s.MaxTripLengthDays, s.TripLengthStepDays = 0, 0, 0
		s.Notes = append(s.Notes, "both a return-date range and a trip-length range were given for the same round trip — kept the return-date range, cleared the trip-length range as redundant")
	}
	return s
}

// orderedRange swaps from/to if they're reversed — cheap insurance
// against the model emitting a range backwards; either value blank
// passes through unchanged (an unresolved end isn't a reversal).
func orderedRange(from, to string) (string, string) {
	if from != "" && to != "" && to < from {
		return to, from
	}
	return from, to
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
	defaultMaxHours = 30
	// defaultQueryBudget: routesearch.Params.QueryBudget's own package
	// default is now unlimited/exhaustive (DESIGN.md "Query budget:
	// unlimited by default, an optional cap") — this is a deliberate
	// override for the email/agent path, not "leave it at the default."
	// See DESIGN.md's "Pacing" section for why an inbound-email pipeline
	// needs a real cap that a person running the CLI directly doesn't.
	defaultQueryBudget       = 20
	defaultMinLayoverMinutes = 120
	defaultMaxLayoverMinutes = 720
	defaultSearchRadiusKm    = 100
)

// normalizeDefaults forward-fills a zeroed numeric field from existing
// (the spec so far) or, failing that, the hardcoded default above — the
// same "treat zero as omitted, not deliberate" fallback
// fillDispatchDefaults already applies one step later (at dispatch time),
// applied here too so a Spec never sits with an unfilled default in the
// meantime. MaxPrice and CheckedBags are deliberately excluded: 0 is
// each one's own documented, legitimate value ("no cap," "bags not
// mentioned"), never "not filled in."
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

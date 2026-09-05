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
  Origin, Destination: IATA airport codes (3 uppercase letters). Only set these if the text names an airport/city unambiguously enough to know the code. If it names a city with several airports and doesn't say which, leave it blank — a later step asks the user, rather than you guessing.
  DepartDate, ReturnDate: YYYY-MM-DD. Resolve relative dates ("next month", "over Christmas") against today's date above. ReturnDate "" means one-way (or not yet known) — do not invent one.
  MaxHours: max tolerable total elapsed trip time, in hours. Default 30 if the text doesn't say.
  QueryBudget: how many hub candidates the search may try. Default 20 if the text doesn't say.
  MaxPrice: hard USD price ceiling, 0 = none. Only set this from an explicit price/budget the text actually states.
  MinLayoverMinutes, MaxLayoverMinutes: default 45 and 720 if the text doesn't say otherwise.
  SoftConstraints: a plain-language list for anything else that matters but isn't one of the fields above — a judgment call needing context, not a threshold (e.g. "must be there for Christmas", "no self-transfer / separate tickets", "traveling with an infant"). Append new ones to whatever's already there; never drop an existing entry.

Reply with EXACTLY one JSON object, no prose outside it, no markdown fences:
{"Spec": {"Origin": "...", "Destination": "...", "DepartDate": "...", "ReturnDate": "...", "MaxHours": 30, "QueryBudget": 20, "MaxPrice": 0, "MinLayoverMinutes": 45, "MaxLayoverMinutes": 720, "SoftConstraints": ["..."]}, "Reasoning": "one sentence on what you filled in or left blank and why"}`

// FormSpec turns existing (the spec so far — zero value for a new
// request) plus text (the new email/CLI text to fold in) into an
// updated Spec, via one LLMClient.Chat call. Prints the full
// prompt/reply so the caller's terminal shows how the field extraction
// actually happened — the same visibility DecideNextAction gives its
// own calls.
func FormSpec(ctx context.Context, llm LLMClient, existing Spec, text string) (Spec, string, error) {
	system := fmt.Sprintf(formSpecSystemPromptTemplate, time.Now().Format("2006-01-02"))
	existingJSON, err := json.Marshal(existing)
	if err != nil {
		return Spec{}, "", fmt.Errorf("agents: marshaling existing spec: %w", err)
	}
	user := fmt.Sprintf("Existing spec so far:\n%s\n\nNew text to fold in:\n%s", existingJSON, text)

	raw, err := llm.Chat(ctx, system, user)
	fmt.Printf("\n=== agents.FormSpec: LLM call ===\n--- system ---\n%s\n--- user ---\n%s\n--- raw reply ---\n%s\n==================================\n\n", system, user, raw)
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

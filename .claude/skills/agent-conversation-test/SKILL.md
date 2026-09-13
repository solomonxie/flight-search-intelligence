---
name: agent-conversation-test
description: Drive cmd/email-intake -interactive with a scripted, realistic multi-turn conversation to stress-test the agent loop (internal/agents) — free-text understanding, clarifying questions, soft constraints, and failure modes — before claiming it's smart/robust enough for real users. Use when asked to "talk to the agent", "have a conversation with the program", "test if it's smart enough", or similar.
---

# Agent conversation test

## Why this program, not another `cmd/`

The only genuinely conversational surface in this repo is `cmd/email-intake -interactive`
(backed by `internal/agents`: `FormSpec` + `DecideNextAction`). `routesearch`, `collector`,
`search-api` are flag-driven one-shots, not a conversation — don't substitute one of those
when asked for "a conversation."

## Setup (no cost, no key spent)

- Override the backend per-invocation rather than touching `.env` (which has a real
  `OPENAI_API_KEY`): `LLM_BACKEND=ollama OLLAMA_MODEL=qwen3:8b`.
- Confirm Ollama is actually up first: `curl -s localhost:11434/api/tags`. A dead/restarted
  Ollama process has already wedged one of these test runs mid-conversation before — see the
  backlog item this same failure produced, in `IMPLEMENTATION_PLAN.md`.
- `make db-init` once; `data/openflights/` should already be cached in the repo.

## Script every turn up front, don't type it live

Pipe the whole conversation as stdin, one line per human turn, ending with `exit`:

```sh
cat > /tmp/convo_input.txt <<'EOF'
<turn 1: vague, ambiguous, realistic>
<turn 2: answer to whatever it's likely to ask, plus one wrinkle>
<turn 3: a follow-up question about the result once it's likely finalized>
exit
EOF
LLM_BACKEND=ollama OLLAMA_MODEL=qwen3:8b go run ./cmd/email-intake -interactive \
  < /tmp/convo_input.txt > /tmp/convo_output.txt 2>&1
```

Since the turns can't adapt to what it actually says (piped, not live), over-supply plausible
answers to likely clarifying questions rather than one line per expected question — a mismatch
just ends the session early via EOF, which is itself a fine, honest result to report.

Turns worth including, because each one exercises a rule `internal/agents/formspec.go` or
`decide.go` documents:

- a multi-airport city ("Tokyo", "Beijing", "London") — must stay a city name, never a guessed
  specific airport
- a vague date phrase ("end of year", "next Jan") — tests the year-rollover handling
  (`validateDates`)
- a soft constraint ("nothing crazy with layovers", "avoid self-transfer") — must never get
  silently promoted to a hard field
- a follow-up that *rewrites* an already-set field (not just fills a blank) — tests
  `LastIntent` classification
- a "why did/didn't..." question once a result likely exists — tests `question_about_result`

## Run it as a background task

A real turn can dispatch a live, capped Google Flights search (`QueryBudget` defaults to 20 on
this path) — can take a couple of minutes. Launch in the background, then read the output file
rather than blocking the whole turn on it.

## Report the transcript verbatim, and judge it honestly

Show the actual conversation, not a paraphrase. Then call out both:

- what the language understanding got right (city/date/constraint handling, which intent it
  picked)
- where it actually broke: a raw Go error reaching the "user" mid-conversation, a crashed/
  unreachable LLM backend with no retry, a wrong intent, a mishandled date

"It ran" is not the same as "it's ready" — see `IMPLEMENTATION_PLAN.md`'s backlog for the
reliability gap this exact test already found.

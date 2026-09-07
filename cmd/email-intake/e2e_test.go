package main

// End-to-end tests for the agent loop's conversational behaviors —
// FormSpec/Decide/DraftFinalEmail driven by a real local LLM (Ollama +
// qwen3, this project's only supported backend — see internal/agents/
// llm.go), exercised through the exact same createRequest/applyFollowUp/
// driveToNextStop functions -interactive itself calls. Only the flight
// search's network leg is faked (fixtureTransport, below): a real LLM
// call is the whole point of an "agent behavior" test, but a real
// Google Flights scrape would make these slow, flaky, and rate-limited
// for no benefit — the offer parsing itself already has its own
// fixture-backed unit test (internal/googleflights).
//
// Needs `ollama serve` running locally with qwen3:8b pulled, and the
// `flyway` CLI on PATH (same as `make db-init`) to stand up a scratch
// schema — both requirements are checked up front and skip (not fail)
// the suite if unmet. These calls are real model inference: expect each
// test to take tens of seconds, and the exact wording of a question or
// email to vary run to run — assertions below stick to structural facts
// (status, which Spec fields got set) and loose keyword checks, never
// exact strings.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/routesearch"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// requireOllama skips the test (not fails it) when the local Ollama
// daemon this project's default LLM_BACKEND needs isn't reachable —
// these tests do real inference, they can't run against nothing.
func requireOllama(t *testing.T) {
	t.Helper()
	base := envOrDefault("OLLAMA_URL", "http://localhost:11434")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(base + "/api/tags")
	if err != nil {
		t.Skipf("ollama not reachable at %s (%v) — these are real end-to-end LLM tests against the local model; run `ollama serve` and `ollama pull qwen3:8b`", base, err)
	}
	resp.Body.Close()
}

// setupTestDB applies databases/sqlite/migrations/ (via flyway, same as
// `make db-init`) to a fresh scratch file under t.TempDir() — catalog.Open
// refuses to create schema itself (see internal/catalog doc comment), and
// duplicating Flyway's own migration-apply logic here would just be a
// second, divergent way to build the same schema.
func setupTestDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("flyway"); err != nil {
		t.Skip("flyway not on PATH — needed to apply databases/sqlite/migrations/ to a scratch test db (see Makefile db-init)")
	}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	// flyway.toml's `locations` is repo-root-relative (filesystem:databases/
	// sqlite/migrations) — cmd.Dir must be the repo root for it to resolve,
	// not cmd/email-intake (flyway doesn't error on a missing location, it
	// just silently applies zero migrations, which then fails later as a
	// confusing "table not found" from catalog.Open instead).
	cmd := exec.Command("flyway", "-configFiles=databases/sqlite/flyway.toml", "-url=jdbc:sqlite:"+dbPath, "migrate")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("flyway migrate: %v\n%s", err, out)
	}
	return dbPath
}

// fixtureTransport serves one canned Google Flights response (a real,
// captured page — internal/googleflights/testdata/sample_search_response.html)
// for every request, regardless of origin/destination/date. That's fine
// for these tests: routesearch.Search builds each Result's Path from the
// airports it *requested* (Params.Origin/Destination/the hub it's
// trying), never from the parsed offer's own segments — only an offer's
// Price and segment timings feed the algorithm, and those are real,
// internally-consistent numbers from an actual scrape.
type fixtureTransport struct{ body []byte }

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(f.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// harness drives one agent conversation turn by turn, the same way
// runREPL's loop does — createRequest for the first line, applyFollowUp
// for every line after, driveToNextStop after each to run the decision
// (and, once dispatched, RunTask) synchronously before the next turn.
type harness struct {
	t         *testing.T
	ctx       context.Context
	db        *catalog.SQLite
	llm       agents.LLMClient
	deps      routesearch.Deps
	requestID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireOllama(t)

	dbPath := setupTestDB(t)
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Ollama directly, not agents.NewLLMClientFromEnv(): these are real
	// end-to-end LLM-behavior tests, and their assertions (and the
	// <think>-stripping in ollama.go) are written against qwen3's actual
	// behavior — whatever LLM_BACKEND .env has set for the app's own
	// runtime (OpenAI, say) shouldn't silently change what the suite
	// exercises, or send a paid API real inference traffic every run.
	llm := agents.NewOllamaClient(envOrDefault("OLLAMA_URL", "http://localhost:11434"), envOrDefault("OLLAMA_MODEL", "qwen3:8b"))

	graph, err := openflights.Load("../../data/openflights")
	if err != nil {
		t.Fatalf("loading openflights graph: %v", err)
	}

	fixture, err := os.ReadFile("../../internal/googleflights/testdata/sample_search_response.html")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	return &harness{
		t: t, ctx: context.Background(), db: db, llm: llm,
		deps: routesearch.Deps{
			Flights: &googleflights.Client{HTTPClient: &http.Client{Transport: fixtureTransport{body: fixture}}},
			Graph:   graph,
			Catalog: db,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)), // keep test output to just t.Log
		},
	}
}

// turn sends one line of user text (the first call starts the request,
// every later call is a follow-up), drives the decision loop, and
// returns the resulting row for the test to assert on.
func (h *harness) turn(text string) catalog.AgentRequestRow {
	h.t.Helper()
	var err error
	if h.requestID == "" {
		h.requestID, err = createRequest(h.ctx, h.db, h.llm, nil, text, false)
	} else {
		err = applyFollowUp(h.ctx, h.db, h.llm, nil, h.requestID, text, false)
	}
	if err != nil {
		h.t.Fatalf("turn %q: %v", text, err)
	}
	if err := driveToNextStop(h.ctx, h.db, h.llm, h.deps, h.requestID); err != nil {
		h.t.Fatalf("turn %q: driving: %v", text, err)
	}
	row, err := h.db.LoadAgentRequest(h.ctx, h.requestID)
	if err != nil {
		h.t.Fatalf("turn %q: loading: %v", text, err)
	}
	h.t.Logf("turn %q -> status=%s email=%q", text, row.Status, row.EmailBody.String)
	return row
}

func (h *harness) spec(row catalog.AgentRequestRow) agents.Spec {
	h.t.Helper()
	var s agents.Spec
	if err := json.Unmarshal([]byte(row.SpecJSON), &s); err != nil {
		h.t.Fatalf("decoding spec: %v", err)
	}
	return s
}

func containsAny(s string, substrs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --- Case 1: lack of info — a bare-bones request must be parked
// awaiting a clarifying question, never dispatched blind.

func TestAgentLoop_LackOfInfo(t *testing.T) {
	h := newHarness(t)
	row := h.turn("find me a flight to beijing")

	if row.Status != agents.StatusAwaitingUser {
		t.Fatalf("status = %q, want %q (origin/date/trip-type were never given)", row.Status, agents.StatusAwaitingUser)
	}
	if row.EmailBody.String == "" {
		t.Fatal("awaiting_user with no question text")
	}
	if !containsAny(row.EmailBody.String, "origin", "departure", "depart", "one-way", "one way", "round-trip", "round trip", "from") {
		t.Errorf("question %q doesn't read like it's asking for origin/date/trip-type", row.EmailBody.String)
	}

	spec := h.spec(row)
	if spec.Destination != "Beijing" {
		t.Errorf("Destination = %q, want %q (the one thing the text did say)", spec.Destination, "Beijing")
	}
}

// --- Case 2: a complete request dispatches and finalizes with a real
// result, no clarifying round needed.

func TestAgentLoop_CompleteRequestDispatches(t *testing.T) {
	h := newHarness(t)
	row := h.turn("one-way flight from YVR to PEK on 2026-12-15")

	if row.Status != agents.StatusFinalized {
		t.Fatalf("status = %q, want %q (everything required was given up front): %s", row.Status, agents.StatusFinalized, row.EmailBody.String)
	}
	if row.EmailBody.String == "" {
		t.Fatal("finalized with no email body")
	}
	if !containsAny(row.EmailBody.String, "$", "price", "usd") {
		t.Errorf("final email %q doesn't mention a price", row.EmailBody.String)
	}

	spec := h.spec(row)
	if spec.Origin != "YVR" || spec.Destination != "PEK" || spec.TripType != "one_way" {
		t.Errorf("spec = %+v, want Origin=YVR Destination=PEK TripType=one_way", spec)
	}
}

// --- Case: an explicit flexible-date request (WindowDays > 0) must
// actually scan the window (dispatch.runFlexibleSearch), not just price
// the one date named — and the final answer should say which date won.

func TestAgentLoop_FlexibleDates(t *testing.T) {
	h := newHarness(t)
	row := h.turn("one-way flight from YVR to PEK, flexible dates around 2026-12-15, give or take 3 days, whichever's cheapest")

	if row.Status != agents.StatusFinalized {
		t.Fatalf("status = %q, want %q: %s", row.Status, agents.StatusFinalized, row.EmailBody.String)
	}

	spec := h.spec(row)
	if spec.WindowDays <= 0 {
		t.Fatalf("WindowDays = %d, want > 0 — an explicit \"give or take N days\" should trigger a flexible-date search", spec.WindowDays)
	}

	var rounds []agents.RoundRecord
	if err := json.Unmarshal([]byte(row.RoundsJSON), &rounds); err != nil {
		t.Fatalf("decoding rounds: %v", err)
	}
	var chosenDate string
	for _, r := range rounds {
		if r.Result != nil && r.Result.ChosenDepartDate != "" {
			chosenDate = r.Result.ChosenDepartDate
		}
	}
	if chosenDate == "" {
		t.Error("no round reported a ChosenDepartDate — the flexible search never recorded which date in the window won")
	}
}

// --- Case: "next <month>" must roll to next year when that month has
// already happened this year — a live run had "next jan" (asked in
// September) resolve to *this* January, landing ReturnDate almost a
// year before DepartDate; the bad request still dispatched (every
// multi-airport candidate pair, both directions — dozens of wasted
// queries) before finalizing a literally backwards "round trip." Uses
// the current month's own name, which is always unambiguous regardless
// of what month tests happen to run in: this year's occurrence of the
// current month has already happened (or is happening now), so "next
// <current month>" can only mean next year's.

func TestAgentLoop_NextMonthCrossesYearBoundary(t *testing.T) {
	h := newHarness(t)
	thisYear := time.Now().Year()
	monthName := time.Now().Month().String()
	row := h.turn("round trip from YVR to PEK, depart next " + monthName + ", return a week later")

	spec := h.spec(row)
	if spec.DepartDate == "" {
		t.Fatalf("DepartDate never got resolved: %+v (email: %s)", spec, row.EmailBody.String)
	}
	departYear := spec.DepartDate[:4]
	if departYear == fmt.Sprint(thisYear) {
		t.Errorf("DepartDate = %q, want next year (%d) — %q this year has already happened/is happening now", spec.DepartDate, thisYear+1, monthName)
	}
	if spec.ReturnDate != "" && spec.ReturnDate <= spec.DepartDate {
		t.Errorf("ReturnDate %q is not after DepartDate %q", spec.ReturnDate, spec.DepartDate)
	}
}

// --- Case 3: user requests a change after finalizing — must actually
// redispatch, not just repeat the old answer (see the commit fixing
// TripType round_trip vs one_way returning the same cached price).

func TestAgentLoop_UserRequestsChange(t *testing.T) {
	h := newHarness(t)
	first := h.turn("round trip from YVR to PEK, depart 2026-12-15, return 2026-12-22")
	if first.Status != agents.StatusFinalized {
		t.Fatalf("first turn status = %q, want %q: %s", first.Status, agents.StatusFinalized, first.EmailBody.String)
	}
	firstEmail := first.EmailBody.String

	second := h.turn("actually, make it one-way, no return")
	if second.Status != agents.StatusFinalized {
		t.Fatalf("second turn status = %q, want %q: %s", second.Status, agents.StatusFinalized, second.EmailBody.String)
	}

	spec := h.spec(second)
	if spec.TripType != "one_way" {
		t.Errorf("TripType = %q after asking for one-way, want %q", spec.TripType, "one_way")
	}
	if spec.ReturnDate != "" {
		t.Errorf("ReturnDate = %q after asking for one-way, want empty", spec.ReturnDate)
	}
	if second.EmailBody.String == firstEmail {
		t.Error("second answer is byte-identical to the first — looks like it never actually redispatched")
	}

	var rounds []agents.RoundRecord
	if err := json.Unmarshal([]byte(second.RoundsJSON), &rounds); err != nil {
		t.Fatalf("decoding rounds: %v", err)
	}
	dispatches := 0
	for _, r := range rounds {
		if r.TaskID != "" {
			dispatches++
		}
	}
	if dispatches < 2 {
		t.Errorf("dispatchCount = %d, want >= 2 (one for the round trip, one more after the change)", dispatches)
	}
}

// --- Case 4: user adds a condition mid-conversation — it must land in
// SoftConstraints, not get dropped.

func TestAgentLoop_UserAddsConditions(t *testing.T) {
	h := newHarness(t)
	row := h.turn("find me a flight to beijing")
	if row.Status != agents.StatusAwaitingUser {
		t.Fatalf("status = %q, want %q", row.Status, agents.StatusAwaitingUser)
	}

	row = h.turn("from YVR, one-way, 2026-12-15, and no self-transfer / separate tickets please")

	spec := h.spec(row)
	if len(spec.SoftConstraints) == 0 {
		t.Fatal("SoftConstraints is empty; the no-self-transfer condition was dropped")
	}
	joined := strings.ToLower(strings.Join(spec.SoftConstraints, " | "))
	if !containsAny(joined, "self-transfer", "self transfer", "separate ticket") {
		t.Errorf("SoftConstraints = %v, none mention self-transfer", spec.SoftConstraints)
	}
}

// --- Case 5: user asks a clarification question the tool has no data
// for (legroom, baggage allowance — nothing in Spec/CollectRouteOffer
// carries this; internal/catalog's aircraft_amenities table exists in
// the schema but nothing populates or reads it yet). This is a known
// gap, not a behavior guarantee: there's no dedicated "answer a
// question" action in agents.Action, so a question like this just folds
// into FormSpec/Decide like any other follow-up. What this test locks
// down is the *safety* property that matters regardless: asking an
// unanswerable question must not corrupt the already-established Spec,
// and must not send the request into an error/undefined status.
func TestAgentLoop_UserAsksClarificationQuestion(t *testing.T) {
	h := newHarness(t)
	first := h.turn("one-way flight from YVR to PEK on 2026-12-15")
	if first.Status != agents.StatusFinalized {
		t.Fatalf("first turn status = %q, want %q: %s", first.Status, agents.StatusFinalized, first.EmailBody.String)
	}

	second := h.turn("what's the legroom like, and what's the baggage allowance?")

	if second.Status != agents.StatusAwaitingUser && second.Status != agents.StatusFinalized {
		t.Fatalf("status = %q after a side question, want awaiting_user or finalized (not an error/stuck state)", second.Status)
	}
	spec := h.spec(second)
	if spec.Origin != "YVR" || spec.Destination != "PEK" {
		t.Errorf("Origin/Destination changed to %s/%s after an unrelated question — the established spec got corrupted", spec.Origin, spec.Destination)
	}

	// No assertion on the reply's content: there's genuinely no baggage/
	// legroom data behind this system today, so any specific-sounding
	// answer here is worth a human's eyes, not a string match. Logged
	// for visibility when running with -v.
	t.Logf("reply to a legroom/baggage question: %q", second.EmailBody.String)
}

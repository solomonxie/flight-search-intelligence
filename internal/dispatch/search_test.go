package dispatch

// Tests runSearch's dispatch branches directly (no LLM involved — that's
// cmd/email-intake/e2e_test.go's job) against a fixture-backed Google
// Flights transport, so they stay fast, deterministic, and offline.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"flight-search-intelligence/internal/agents"
	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
	"flight-search-intelligence/internal/routesearch"
)

// fixtureTransport serves one canned Google Flights response (a real
// captured page, trimmed to its embedded script — see
// internal/googleflights/testdata) for every request. Fine here: a
// Result's Path always comes from the airports actually requested, never
// the parsed offer's own segments — see the fuller explanation on this
// same type in cmd/email-intake/e2e_test.go.
type fixtureTransport struct{ body []byte }

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(f.body)), Header: make(http.Header), Request: req}, nil
}

func setupTestDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("flyway"); err != nil {
		t.Skip("flyway not on PATH — needed to apply databases/sqlite/migrations/ to a scratch test db (see Makefile db-init)")
	}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := exec.Command("flyway", "-configFiles=databases/sqlite/flyway.toml", "-url=jdbc:sqlite:"+dbPath, "migrate")
	cmd.Dir = "../.." // flyway.toml's `locations` is repo-root-relative
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("flyway migrate: %v\n%s", err, out)
	}
	return dbPath
}

func testDeps(t *testing.T) routesearch.Deps {
	t.Helper()
	db, err := catalog.Open(setupTestDB(t))
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	graph, err := openflights.Load("../../data/openflights")
	if err != nil {
		t.Fatalf("loading openflights graph: %v", err)
	}
	fixture, err := os.ReadFile("../../internal/googleflights/testdata/sample_search_response.html")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return routesearch.Deps{
		Flights: &googleflights.Client{HTTPClient: &http.Client{Transport: fixtureTransport{body: fixture}}},
		Graph:   graph,
		Catalog: db,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRunSearch_Flexible_OneWay(t *testing.T) {
	deps := testDeps(t)
	result, err := runSearch(context.Background(), deps, agents.CollectRouteRequest{
		Origin: "YVR", Destination: "PEK", TripType: "one_way",
		DepartDateFrom: "2026-12-12", DepartDateTo: "2026-12-18",
		MaxHours: 30, QueryBudget: 10, MinLayoverMinutes: 45, MaxLayoverMinutes: 720, SearchRadiusKm: 100,
		StepDays: 1,
	})
	if err != nil {
		t.Fatalf("runSearch: %v", err)
	}
	if result.ChosenDepartDate == "" {
		t.Error("ChosenDepartDate is empty — a flexible search should report which date in the window won")
	}
	if len(result.Results) != 1 {
		t.Fatalf("got %d results, want 1 (the winning date's best itinerary)", len(result.Results))
	}
	if result.Results[0].PriceUSD <= 0 {
		t.Errorf("PriceUSD = %v, want > 0", result.Results[0].PriceUSD)
	}
	if result.Results[0].TotalPriceUSD != 0 {
		t.Errorf("TotalPriceUSD = %v, want 0 for a one-way result", result.Results[0].TotalPriceUSD)
	}
}

func TestRunSearch_Flexible_RoundTrip(t *testing.T) {
	deps := testDeps(t)
	result, err := runSearch(context.Background(), deps, agents.CollectRouteRequest{
		Origin: "YVR", Destination: "PEK", TripType: "round_trip",
		DepartDateFrom: "2026-12-14", DepartDateTo: "2026-12-16",
		ReturnDateFrom: "2026-12-21", ReturnDateTo: "2026-12-23",
		MaxHours: 30, QueryBudget: 20, MinLayoverMinutes: 45, MaxLayoverMinutes: 720, SearchRadiusKm: 100,
		StepDays: 1,
	})
	if err != nil {
		t.Fatalf("runSearch: %v", err)
	}
	if result.ChosenDepartDate == "" || result.ChosenReturnDate == "" {
		t.Errorf("ChosenDepartDate/ChosenReturnDate = %q/%q, want both set for a round-trip flexible search", result.ChosenDepartDate, result.ChosenReturnDate)
	}
	if len(result.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(result.Results))
	}
	if result.Results[0].TotalPriceUSD <= 0 {
		t.Errorf("TotalPriceUSD = %v, want > 0", result.Results[0].TotalPriceUSD)
	}
}

// TestResolveAirports_CityFanoutIsBounded is a regression test: a city
// name's radius fan-out used to return every airport OpenFlights knows
// of within range, unfiltered — a live run had "Vancouver" resolve to
// 17 candidates (mostly tiny regional strips with no real long-haul
// service) which, combined with a similarly multi-airport destination,
// meant 51 full searches for one request: slow, and plausibly enough
// simultaneous scraping to degrade the one result that mattered (a live
// run's real bundled fare came back worse than a hand-checked price).
func TestResolveAirports_CityFanoutIsBounded(t *testing.T) {
	graph, err := openflights.Load("../../data/openflights")
	if err != nil {
		t.Fatal(err)
	}
	codes, err := resolveAirports(graph, "Vancouver", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) > maxCityCandidates {
		t.Errorf("got %d candidates for Vancouver, want <= maxCityCandidates (%d): %v", len(codes), maxCityCandidates, codes)
	}
	found := false
	for _, c := range codes {
		if c == "YVR" {
			found = true
		}
	}
	if !found {
		t.Errorf("candidates %v don't include YVR — the actual major gateway got filtered out", codes)
	}
}

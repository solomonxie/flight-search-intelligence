package routesearch

// Structural correctness tests for searchNHop, against a fixture-backed
// Google Flights transport (no network, no LLM — see
// internal/dispatch/search_test.go and cmd/email-intake/e2e_test.go for
// the same pattern elsewhere).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
)

// syntheticOfferHTML builds a minimal ds:1-script HTML page parseOffers
// can read, with exactly one nonstop offer at price, departing
// hoursOffset hours after a fixed base instant — enough of Google's real
// (reverse-engineered, undocumented) payload shape to exercise the known
// field indices googleflights/parse.go reads, without needing a captured
// real page (see sample_search_response.html in
// internal/googleflights/testdata for that approach instead).
//
// hoursOffset exists because a static, identical offer can't represent a
// multi-hop chain: layover feasibility needs each leg to depart strictly
// after the previous one arrived, which means departure time has to
// track *which call this is*, not just be some fixed clock time (a
// dynamicTransport passes its call index in as hoursOffset — see
// dynamicTransport.RoundTrip).
func syntheticOfferHTML(price, durationMinutes, hoursOffset int) []byte {
	pad := func(vals map[int]any, length int) []any {
		out := make([]any, length)
		for i, v := range vals {
			out[i] = v
		}
		return out
	}
	// Same real, resolvable airport on both ends (rather than two made-up
	// codes) so tripDuration's timezone lookup resolves identically for
	// departure and arrival — two unresolvable/mismatched codes can
	// silently yield a nonsensical (even negative) duration if one
	// happens to collide with a real airport in another timezone (as
	// "AAA" — Anaa, French Polynesia, UTC-10 — once did here) and the
	// other doesn't. Segment airport identity doesn't otherwise matter:
	// a Result's Path always comes from the airports actually requested,
	// never a parsed offer's own segments (see fixtureTransport's doc
	// elsewhere in this package).
	depDay, depHour := 15+hoursOffset/24, hoursOffset%24
	arrTotal := hoursOffset*60 + durationMinutes
	arrDay, arrHour, arrMin := 15+arrTotal/(24*60), (arrTotal/60)%24, arrTotal%60
	segment := pad(map[int]any{
		3: "YVR", 4: "Test Airport", 5: "Test Airport", 6: "YVR",
		8: []any{depHour, 0}, 10: []any{arrHour, arrMin},
		11: durationMinutes, 17: "TestPlane",
		20: []any{2026, 12, depDay}, 21: []any{2026, 12, arrDay},
	}, 22)
	flight := pad(map[int]any{
		0: "Nonstop", 1: []any{"Test Air"}, 2: []any{segment},
		22: []any{nil, nil, nil, nil, nil, nil, nil, 0, 0},
	}, 23)
	k := []any{flight, []any{[]any{nil, price}}}
	payload := []any{nil, nil, []any{[]any{k}}}

	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return []byte(fmt.Sprintf(`<html><body><script class="ds:1" nonce="x">AF_initDataCallback({key: 'ds:1', hash: '1', data: %s, sideChannel: {}});</script></body></html>`, body))
}

// fixtureTransport serves one canned body for every request.
type fixtureTransport struct{ body []byte }

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(f.body)), Header: make(http.Header), Request: req}, nil
}

// dynamicTransport serves baseline for the 1st request; every request
// after that gets a fresh syntheticOfferHTML(legPrice, legDuration, i)
// where i is that request's own call index — see syntheticOfferHTML's
// doc for why a static body can't stand in for a whole chain.
// searchNHop's queries are strictly sequential (no concurrency), so call
// order is deterministic.
type dynamicTransport struct {
	mu                        sync.Mutex
	n                         int
	baseline                  []byte
	legPrice, legDurationMins int
}

func (s *dynamicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	i := s.n
	s.n++
	s.mu.Unlock()
	body := s.baseline
	if i > 0 {
		// 3h spacing per call index, comfortably more than
		// legDurationMins (1.5h in every test using this) so consecutive
		// legs always land a valid positive layover rather than
		// overlapping.
		body = syntheticOfferHTML(s.legPrice, s.legDurationMins, i*3)
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), Request: req}, nil
}

func testDeps(t *testing.T, transport http.RoundTripper) Deps {
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
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	graph, err := openflights.Load("../../data/openflights")
	if err != nil {
		t.Fatalf("loading openflights graph: %v", err)
	}
	return Deps{
		Flights: &googleflights.Client{HTTPClient: &http.Client{Transport: transport}},
		Graph:   graph,
		Catalog: db,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSearch_MaxLegsOneUnchanged(t *testing.T) {
	// MaxLegs 0 (unset) and 1 (explicit) must both take the original
	// 1-stop path, never searchNHop — NHopRanked staying empty is the
	// observable proof (searchNHop always appends to it before returning).
	for _, maxLegs := range []int{0, 1} {
		deps := testDeps(t, fixtureTransport{body: syntheticOfferHTML(1375, 780, 0)})
		plan, err := Search(context.Background(), deps, Params{
			Origin: "YVR", Destination: "PEK", DepartDate: "2026-12-15",
			MaxHours: 30, QueryBudget: 5, MinLayoverMinutes: 45, MaxLayoverMinutes: 720,
			PricePerMile: 0.08, MaxLegs: maxLegs,
		})
		if err != nil {
			t.Fatalf("MaxLegs=%d: Search: %v", maxLegs, err)
		}
		if len(plan.NHopRanked) != 0 {
			t.Errorf("MaxLegs=%d: NHopRanked = %v, want empty (should never have run searchNHop)", maxLegs, plan.NHopRanked)
		}
		if len(plan.FinalResult) == 0 {
			t.Errorf("MaxLegs=%d: no results (baseline should always be feasible against the fixture)", maxLegs)
		}
	}
}

// TestSearchNHop_AssemblesCheaperMultiHop is the real behavioral test:
// an expensive baseline (query #1) vs. cheap subsequent legs means a
// genuine 2+-hop combo must come out ahead — this can only pass if the
// algorithm actually chains labels across hops, applies dominance
// pruning without discarding the eventual winner, and respects
// MaxLegs/QueryBudget along the way.
func TestSearchNHop_AssemblesCheaperMultiHop(t *testing.T) {
	transport := &dynamicTransport{
		baseline:        syntheticOfferHTML(5000, 780, 0), // query #1: the direct baseline — deliberately expensive
		legPrice:        100,                              // every query after: a cheap leg
		legDurationMins: 90,
	}
	deps := testDeps(t, transport)

	const queryBudget = 60
	const maxLegs = 3
	// MaxHours/*LayoverMinutes are deliberately huge, not realistic
	// values: syntheticOfferHTML times every offer off the transport's
	// own call index (see its doc comment) so consecutive legs always
	// land a valid layover regardless of chain depth, but that same
	// mechanism means the gap between an arbitrary pair of chained calls
	// isn't itself meaningful — a tight, realistic cap here would be
	// testing the synthetic clock, not the algorithm. This test's job is
	// the chaining/dominance/depth-cap/budget behavior, not layover-
	// window enforcement (already exercised by the 1-stop case's own
	// bestConnection logic, which pickNextOffer reuses).
	plan, err := Search(context.Background(), deps, Params{
		Origin: "YVR", Destination: "PEK", DepartDate: "2026-12-15",
		MaxHours: 1e6, QueryBudget: queryBudget, MinLayoverMinutes: 0, MaxLayoverMinutes: 1e7,
		PricePerMile: 0.08, MaxLegs: maxLegs,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if plan.QueriesUsed > queryBudget {
		t.Errorf("QueriesUsed = %d, want <= %d", plan.QueriesUsed, queryBudget)
	}

	var multiHop *Result
	for i, r := range plan.FinalResult {
		if len(r.Path) > 2 {
			multiHop = &plan.FinalResult[i]
		}
	}
	if multiHop == nil {
		t.Fatalf("no multi-hop result found among %d result(s): %+v — with legs at $100 each vs. a $5000 baseline, a 2-hop combo ($200) should have won", len(plan.FinalResult), plan.FinalResult)
	}
	if multiHop.PriceUSD >= 5000 {
		t.Errorf("multi-hop result price = %v, want well under the $5000 baseline", multiHop.PriceUSD)
	}

	legs := len(multiHop.Path) - 1
	if legs > maxLegs {
		t.Errorf("result %v has %d legs, want <= MaxLegs=%d", multiHop.Path, legs, maxLegs)
	}
	if multiHop.Path[0] != "YVR" || multiHop.Path[len(multiHop.Path)-1] != "PEK" {
		t.Errorf("result path %v doesn't start at YVR and end at PEK", multiHop.Path)
	}
	seen := map[string]bool{}
	for _, a := range multiHop.Path {
		if seen[a] {
			t.Errorf("result path %v revisits %s — the search should never produce a cycle", multiHop.Path, a)
		}
		seen[a] = true
	}
	if wantTransfers := len(multiHop.Path) - 2; multiHop.TransferCount != wantTransfers {
		t.Errorf("TransferCount = %d, want %d (len(Path)-2)", multiHop.TransferCount, wantTransfers)
	}
	if !multiHop.SelfTransfer {
		t.Error("SelfTransfer = false for a multi-hop result, want true")
	}
}

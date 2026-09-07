package googleflights

import (
	"os"
	"testing"
)

// TestParseOffers_BothSections is a regression test: parseOffers once
// only read payload[3] ("other flights"), silently dropping payload[2]
// ("best flights") — the section Google's own page puts its top-ranked,
// usually-cheapest fares in. testdata/sample_search_response.html is a
// real captured response (trimmed to just its ds:1 script) for a
// YVR->PEK round trip where the actual cheapest fares (Air Canada,
// Korean Air) live in the "best flights" section and a pricier Cathay
// Pacific fare lives further down — exactly the shape that let the bug
// report the 3rd-cheapest fare as "the best."
func TestParseOffers_BothSections(t *testing.T) {
	body, err := os.ReadFile("testdata/sample_search_response.html")
	if err != nil {
		t.Fatal(err)
	}
	offers, err := parseOffers(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) < 2 {
		t.Fatalf("got %d offers, want at least 2 (one from each section)", len(offers))
	}

	min := offers[0].Price
	for _, o := range offers[1:] {
		if o.Price < min {
			min = o.Price
		}
	}

	const knownCheapestFromBestFlightsSection = 1394 // Korean Air, in this fixture
	if min > knownCheapestFromBestFlightsSection {
		t.Errorf("cheapest parsed offer is $%d, want <= $%d — looks like the \"best flights\" section (payload[2]) isn't being read", min, knownCheapestFromBestFlightsSection)
	}

	var sawCathay bool
	for _, o := range offers {
		for _, a := range o.Airlines {
			if a == "Cathay Pacific" {
				sawCathay = true
			}
		}
	}
	if !sawCathay {
		t.Error("expected a Cathay Pacific offer from the \"other flights\" section (payload[3]) too — both sections should be merged, not just one")
	}
}

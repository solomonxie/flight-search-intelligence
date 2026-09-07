package routesearch

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"flight-search-intelligence/internal/openflights"
)

const (
	avgCruiseMPH         = 500.0 // rough commercial cruise speed, for the geometry prune only
	perLegOverheadMinute = 45.0  // taxi/climb/descent/etc, not part of cruise time
)

func estimateMinutes(distanceMiles float64) float64 {
	return distanceMiles/avgCruiseMPH*60 + perLegOverheadMinute
}

// excludesCountry reports whether country appears in excluded
// (case-insensitive) — Params.ExcludedCountries' hard-prune check,
// shared by the 1-stop and N-hop geometry-prune stages.
func excludesCountry(excluded []string, country string) bool {
	for _, c := range excluded {
		if strings.EqualFold(c, country) {
			return true
		}
	}
	return false
}

// countDistinctCountries returns how many distinct countries the given
// IATA codes plus one extra candidate country span — Params.MaxCountries'
// check. Unresolvable airports are skipped rather than erroring: an
// unknown country is better treated as "doesn't add a new one" than as a
// search-ending failure this deep in a prune.
func countDistinctCountries(graph *openflights.Graph, path []string, candidateCountry string) int {
	seen := map[string]bool{}
	for _, iata := range path {
		if a, ok := graph.Airport(iata); ok && a.Country != "" {
			seen[a.Country] = true
		}
	}
	if candidateCountry != "" {
		seen[candidateCountry] = true
	}
	return len(seen)
}

// ResolveCandidates runs Search's candidate-generation step standalone:
// resolve p.Origin/p.Destination, pull hub candidates from the route
// graph, geometry-prune the ones that can't fit p.MaxHours even with a
// minimum layover, and rank survivors by lower-bound price (cached price
// if the store has one, else distance × p.PricePerMile). No network
// calls — Search calls this too, so the two never drift, and it's cheap
// to run standalone (cmd/routesearch -dry-run) while testing.
func ResolveCandidates(ctx context.Context, deps Deps, p Params) (*CandidatePreview, error) {
	origin, ok1 := deps.Graph.Airport(p.Origin)
	destination, ok2 := deps.Graph.Airport(p.Destination)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("routesearch: unknown airport (origin ok=%v, destination ok=%v)", ok1, ok2)
	}

	rawHubs := deps.Graph.CandidateHubs(p.Origin, p.Destination)
	preview := &CandidatePreview{
		Origin:               origin,
		Destination:          destination,
		DirectDistanceMiles:  openflights.DistanceMiles(origin, destination),
		HasNonstop:           deps.Graph.HasNonstop(p.Origin, p.Destination),
		CandidatesConsidered: len(rawHubs),
	}

	var hubRows []RankedHub
	for _, h := range rawHubs {
		hub, ok := deps.Graph.Airport(h)
		if !ok {
			continue
		}
		if excludesCountry(p.ExcludedCountries, hub.Country) {
			continue // a real constraint (visa/sanctions/safety) — see Params.ExcludedCountries
		}
		if p.MaxCountries > 0 && countDistinctCountries(deps.Graph, []string{p.Origin, p.Destination}, hub.Country) > p.MaxCountries {
			continue // the full path is Origin->hub->Destination — all three count
		}
		d1 := openflights.DistanceMiles(origin, hub)
		d2 := openflights.DistanceMiles(hub, destination)
		if estimateMinutes(d1)+estimateMinutes(d2)+float64(p.MinLayoverMinutes) > p.MaxHours*60 {
			continue // geometry prune: can't fit even with a minimum-length layover
		}
		lb1 := deps.lowerBoundUSD(ctx, p.Origin, h, p.DepartDate, d1, p.PricePerMile)
		lb2 := deps.lowerBoundUSD(ctx, h, p.Destination, p.DepartDate, d2, p.PricePerMile)
		hubRows = append(hubRows, RankedHub{Hub: h, LBUSD: lb1 + lb2, Leg1Miles: d1, Leg2Miles: d2})
	}
	preview.CandidatesAfterGeometryPrune = len(hubRows)
	sort.Slice(hubRows, func(i, j int) bool { return hubRows[i].LBUSD < hubRows[j].LBUSD })
	preview.RankedHubs = hubRows

	// Direct (no hub) is its own field, not the head of RankedHubs — Search
	// reuses RankedHubs as the literal list of hubs to scrape, and
	// "<destination>+<" is not an airport. DirectRow exists purely for
	// display: the baseline every hub candidate is trying to beat, same as
	// rank 0 in Search's own audit trail. Leg2Miles: 0 marks it as a single
	// leg, not a connection; the "<" marks the row as "straight there," not
	// a hub named destination.IATA.
	preview.DirectRow = RankedHub{
		Hub:       destination.IATA + "<",
		LBUSD:     deps.lowerBoundUSD(ctx, p.Origin, p.Destination, p.DepartDate, preview.DirectDistanceMiles, p.PricePerMile),
		Leg1Miles: preview.DirectDistanceMiles,
		Leg2Miles: 0,
	}
	return preview, nil
}

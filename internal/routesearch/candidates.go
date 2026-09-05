package routesearch

import (
	"context"
	"fmt"
	"sort"

	"flight-search-intelligence/internal/openflights"
)

const (
	avgCruiseMPH         = 500.0 // rough commercial cruise speed, for the geometry prune only
	perLegOverheadMinute = 45.0  // taxi/climb/descent/etc, not part of cruise time
)

func estimateMinutes(distanceMiles float64) float64 {
	return distanceMiles/avgCruiseMPH*60 + perLegOverheadMinute
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

	for _, h := range rawHubs {
		hub, ok := deps.Graph.Airport(h)
		if !ok {
			continue
		}
		d1 := openflights.DistanceMiles(origin, hub)
		d2 := openflights.DistanceMiles(hub, destination)
		if estimateMinutes(d1)+estimateMinutes(d2)+float64(p.MinLayoverMinutes) > p.MaxHours*60 {
			continue // geometry prune: can't fit even with a minimum-length layover
		}
		lb1 := deps.lowerBoundUSD(ctx, p.Origin, h, p.DepartDate, d1, p.PricePerMile)
		lb2 := deps.lowerBoundUSD(ctx, h, p.Destination, p.DepartDate, d2, p.PricePerMile)
		preview.RankedHubs = append(preview.RankedHubs, RankedHub{Hub: h, LBUSD: lb1 + lb2, Leg1Miles: d1, Leg2Miles: d2})
	}
	preview.CandidatesAfterGeometryPrune = len(preview.RankedHubs)
	sort.Slice(preview.RankedHubs, func(i, j int) bool { return preview.RankedHubs[i].LBUSD < preview.RankedHubs[j].LBUSD })
	return preview, nil
}

package routesearch

import (
	"context"
	"time"

	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
)

// pickCheapestFeasible returns the cheapest offer whose real (timezone-
// aware) trip duration fits maxHours, and that duration.
func pickCheapestFeasible(offers []googleflights.Offer, graph *openflights.Graph, maxHours float64) (googleflights.Offer, time.Duration, bool) {
	var best googleflights.Offer
	var bestDur time.Duration
	found := false
	for _, o := range offers {
		dur, ok := tripDuration(o, graph)
		if !ok || dur.Hours() > maxHours {
			continue
		}
		if !found || o.Price < best.Price {
			best, bestDur, found = o, dur, true
		}
	}
	return best, bestDur, found
}

// bestConnection picks the cheapest leg2 offer that connects to leg1
// within [MinLayoverMinutes, MaxLayoverMinutes] and keeps the whole trip
// within MaxHours, and returns the combined (real, timezone-aware) trip
// duration. Only leg1's single cheapest offer is considered — a
// deliberate simplification (see DESIGN.md) that trades a little
// optimality for not having to try every leg1×leg2 pair.
func (d Deps) bestConnection(leg1 googleflights.Offer, leg2Offers []googleflights.Offer, p Params) (offer googleflights.Offer, total time.Duration, layoverDur time.Duration, found bool) {
	var best googleflights.Offer
	var bestTotal, bestLayover time.Duration
	for _, o := range leg2Offers {
		lay, ok := layover(leg1, o, d.Graph)
		if !ok {
			continue
		}
		if lay < time.Duration(p.MinLayoverMinutes)*time.Minute || lay > time.Duration(p.MaxLayoverMinutes)*time.Minute {
			continue
		}
		leg1Dur, ok1 := tripDuration(leg1, d.Graph)
		leg2Dur, ok2 := tripDuration(o, d.Graph)
		if !ok1 || !ok2 {
			continue
		}
		tot := leg1Dur + lay + leg2Dur
		if tot.Hours() > p.MaxHours {
			continue
		}
		if !found || o.Price < best.Price {
			best, bestTotal, bestLayover, found = o, tot, lay, true
		}
	}
	return best, bestTotal, bestLayover, found
}

// lowerBoundUSD is the admissible (never-overestimating) price estimate
// used to rank/prune candidates before actually scraping them: a cached
// recent price for this exact leg if the store has one, else a crude
// distance × $/mile prior.
func (d Deps) lowerBoundUSD(ctx context.Context, origin, destination, date string, distanceMiles, pricePerMile float64) float64 {
	if cents, ok, err := d.Catalog.CachedPriceCents(ctx, origin, destination, date); err == nil && ok {
		return float64(cents) / 100.0
	}
	return distanceMiles * pricePerMile
}

// paretoInsert adds r to results if no existing result dominates it
// (≤ price and ≤ duration), and drops any existing results r itself
// dominates.
func paretoInsert(results []Result, r Result) []Result {
	for _, existing := range results {
		if existing.PriceUSD <= r.PriceUSD && existing.DurationMinutes <= r.DurationMinutes {
			return results // r is dominated; nothing changes
		}
	}
	kept := results[:0]
	for _, existing := range results {
		if !(r.PriceUSD <= existing.PriceUSD && r.DurationMinutes <= existing.DurationMinutes) {
			kept = append(kept, existing)
		}
	}
	return append(kept, r)
}

// cheapestResult returns the lowest-price entry in a non-empty Pareto set.
func cheapestResult(results []Result) *Result {
	best := results[0]
	for _, r := range results[1:] {
		if r.PriceUSD < best.PriceUSD {
			best = r
		}
	}
	return &best
}

// cheapestOffer returns the lowest-price offer, unfiltered by duration —
// used where price alone decides (e.g. a bundled round-trip fare).
func cheapestOffer(offers []googleflights.Offer) (googleflights.Offer, bool) {
	if len(offers) == 0 {
		return googleflights.Offer{}, false
	}
	best := offers[0]
	for _, o := range offers[1:] {
		if o.Price < best.Price {
			best = o
		}
	}
	return best, true
}

// cheapestDateScanEntry returns the cheapest queried, priced entry from a
// flexible-date scan — nil if every date came back empty/infeasible.
func cheapestDateScanEntry(entries []DateScanEntry) *DateScanEntry {
	var best *DateScanEntry
	for i := range entries {
		e := &entries[i]
		if !e.Queried || e.PriceUSD <= 0 {
			continue
		}
		if best == nil || e.PriceUSD < best.PriceUSD {
			best = e
		}
	}
	return best
}

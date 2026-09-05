package routesearch

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
)

// offersCacheFreshness is how old a cached scrape can be and still be
// served instead of a live re-scrape — the "within the last hour" window.
const offersCacheFreshness = time.Hour

// searchOffers is the cache-first front door every routesearch query goes
// through instead of calling deps.Flights.SearchFlightOffers directly: a
// scrape of this exact (origin, destination, depart, return) within
// offersCacheFreshness is reused as-is, skipping Google entirely, unless
// forceRefresh asks to bypass the cache and scrape live regardless. live
// reports whether an actual scrape happened, so callers can charge it
// against their query budget — a cache hit costs nothing.
func (d Deps) searchOffers(ctx context.Context, params googleflights.SearchParams, forceRefresh bool) ([]googleflights.Offer, bool, error) {
	if !forceRefresh {
		if cached, createdAt, ok, err := d.Catalog.CachedOffers(ctx, params.Origin, params.Destination, params.DepartureDate, params.ReturnDate, offersCacheFreshness); err == nil && ok {
			var cachedOffers []googleflights.Offer
			if err := json.Unmarshal(cached, &cachedOffers); err == nil {
				d.Logger.Info("offers cache hit", "origin", params.Origin, "destination", params.Destination,
					"depart_date", params.DepartureDate, "created_at", createdAt)
				return cachedOffers, false, nil
			}
		}
	}

	offers, _, err := d.Flights.SearchFlightOffers(ctx, params)
	if err != nil {
		return nil, true, err
	}
	d.saveOffersCache(ctx, params, offers)
	d.recordOffers(ctx, params.Origin, params.Destination, params.DepartureDate, offers)
	return offers, true, nil
}

// saveOffersCache persists this scrape's raw offers for searchOffers'
// cache-first read on the next call for the same query.
func (d Deps) saveOffersCache(ctx context.Context, params googleflights.SearchParams, offers []googleflights.Offer) {
	if len(offers) == 0 {
		return
	}
	b, err := json.Marshal(offers)
	if err != nil {
		d.Logger.Warn("marshaling offers for cache failed", "error", err)
		return
	}
	if err := d.Catalog.SaveOffersCache(ctx, params.Origin, params.Destination, params.DepartureDate, params.ReturnDate,
		"google_flights", b, time.Now()); err != nil {
		d.Logger.Warn("saving offers cache failed", "error", err)
	}
}

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

// recordOffers persists every scraped offer into the shared flight_prices
// table (not just the ones this search ends up using) so the next
// search — this hub pair, or a plain cmd/collector run — gets to use
// them as a lowerBoundUSD cache hit instead of a cruder $/mile guess.
// This is the "repeat value comes from accumulation" property from
// DESIGN.md "Collection scope", made to actually apply to routesearch's
// own scrapes.
func (d Deps) recordOffers(ctx context.Context, origin, destination, date string, offers []googleflights.Offer) {
	if len(offers) == 0 {
		return
	}
	now := time.Now()
	rows := make([]catalog.FlightPrice, len(offers))
	for i, o := range offers {
		rows[i] = catalog.FlightPrice{
			Origin: origin, Destination: destination,
			Airline:    strings.Join(o.Airlines, ","),
			DepartDate: date,
			PriceCents: int64(o.Price) * 100,
			Currency:   "USD",
			Source:     "google_flights",
			CreatedAt:  now,
		}
	}
	if err := d.Catalog.InsertFlightPrices(ctx, rows); err != nil {
		d.Logger.Warn("recording scraped offers failed", "error", err)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":"marshal failed"}`)
	}
	return b
}

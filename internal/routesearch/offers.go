package routesearch

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"flight-search-intelligence/internal/catalog"
	"flight-search-intelligence/internal/googleflights"
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

package agents

import "flight-search-intelligence/internal/googleflights"

// bookingLink returns a Google Flights search URL for the itinerary the
// last round to actually find one dispatched — a live link the traveler
// can open to check current pricing/availability and book, since this
// tool only searches and never books itself. Built from that round's own
// dispatched request (origin/destination/dates as actually searched),
// not the top-level Spec, which may have moved on since without a new
// round dispatched against it yet. Empty when no round ever found a
// result.
func bookingLink(rounds []RoundRecord) string {
	round, offer, ok := lastOffer(rounds)
	if !ok {
		return ""
	}

	var origin, destination string
	switch {
	case len(offer.Path) >= 2:
		origin, destination = offer.Path[0], offer.Path[len(offer.Path)-1]
	case len(offer.ReturnPath) >= 2:
		// Path is empty under a bundled round-trip fare (no separate
		// outbound leg was priced) — ReturnPath's endpoints, reversed,
		// name the same origin/destination pair.
		destination, origin = offer.ReturnPath[0], offer.ReturnPath[len(offer.ReturnPath)-1]
	default:
		return ""
	}

	req := round.Decision.Request
	params := googleflights.SearchParams{
		Origin:        origin,
		Destination:   destination,
		DepartureDate: req.DepartDate,
	}
	if req.TripType == "round_trip" {
		params.ReturnDate = req.ReturnDate
	}
	return googleflights.SearchURL(params)
}

// lastOffer finds the most recent round with a non-empty result and
// returns that round alongside its cheapest offer — cheapest by
// TotalPriceUSD when set (round-trip), else PriceUSD.
func lastOffer(rounds []RoundRecord) (RoundRecord, CollectRouteOffer, bool) {
	for i := len(rounds) - 1; i >= 0; i-- {
		r := rounds[i]
		if r.Result == nil || len(r.Result.Results) == 0 {
			continue
		}
		best := r.Result.Results[0]
		for _, o := range r.Result.Results[1:] {
			if offerPrice(o) < offerPrice(best) {
				best = o
			}
		}
		return r, best, true
	}
	return RoundRecord{}, CollectRouteOffer{}, false
}

func offerPrice(o CollectRouteOffer) float64 {
	if o.TotalPriceUSD > 0 {
		return o.TotalPriceUSD
	}
	return o.PriceUSD
}

package routesearch

import (
	"time"

	"flight-search-intelligence/internal/googleflights"
	"flight-search-intelligence/internal/openflights"
)

// location returns iata's IANA timezone, falling back to UTC if unknown —
// total-trip-duration math is then approximate for that leg rather than
// wrong in a way that crashes.
func location(graph *openflights.Graph, iata string) *time.Location {
	if a, ok := graph.Airport(iata); ok && a.Timezone != "" {
		if loc, err := time.LoadLocation(a.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

// toTime builds an absolute instant from a Segment's [year,month,day] /
// [hour,minute] pair in loc, so subtracting two of these across airports
// in different timezones gives real elapsed time, not wall-clock-looks-like.
func toTime(date [3]int, clock [2]int, loc *time.Location) time.Time {
	return time.Date(date[0], time.Month(date[1]), date[2], clock[0], clock[1], 0, 0, loc)
}

// tripDuration is the real elapsed time from the first segment's
// departure to the last segment's arrival.
func tripDuration(o googleflights.Offer, graph *openflights.Graph) (time.Duration, bool) {
	if len(o.Segments) == 0 {
		return 0, false
	}
	first, last := o.Segments[0], o.Segments[len(o.Segments)-1]
	dep := toTime(first.DepartureDate, first.DepartureTime, location(graph, first.FromAirport))
	arr := toTime(last.ArrivalDate, last.ArrivalTime, location(graph, last.ToAirport))
	return arr.Sub(dep), true
}

// layover is the real elapsed time between one offer's last arrival and
// a second offer's first departure — same airport on both sides, so any
// timezone-lookup miss cancels out rather than skewing the result.
func layover(arriving googleflights.Offer, departing googleflights.Offer, graph *openflights.Graph) (time.Duration, bool) {
	if len(arriving.Segments) == 0 || len(departing.Segments) == 0 {
		return 0, false
	}
	last := arriving.Segments[len(arriving.Segments)-1]
	next := departing.Segments[0]
	loc := location(graph, last.ToAirport)
	arr := toTime(last.ArrivalDate, last.ArrivalTime, loc)
	dep := toTime(next.DepartureDate, next.DepartureTime, loc)
	return dep.Sub(arr), true
}

// dateString formats a Segment date triple as the YYYY-MM-DD Google
// Flights expects.
func dateString(d [3]int) string {
	return time.Date(d[0], time.Month(d[1]), d[2], 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

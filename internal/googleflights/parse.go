package googleflights

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when Google's response payload signals no
// results (the "errorHasStatus: true" marker fast-flights checks for).
var ErrNotFound = errors.New("googleflights: no flights found")

// Offer is one itinerary card from the results grid.
type Offer struct {
	Type     string // e.g. "Nonstop", "1 stop" — Google's own label, not ours
	Price    int
	Currency string
	Airlines []string
	Segments []Segment

	CarbonEmissionGrams        int
	TypicalCarbonEmissionGrams int
}

// Segment is one flown leg within an Offer.
type Segment struct {
	FromAirport, FromAirportName string
	ToAirport, ToAirportName     string
	DepartureDate                [3]int // year, month, day
	DepartureTime                [2]int // hour, minute
	ArrivalDate                  [3]int
	ArrivalTime                  [2]int
	DurationMinutes              int
	PlaneType                    string
}

// parseOffers extracts flight offers from the search page's embedded
// AF_initDataCallback payload (script class "ds:1"). Both the script
// location and the payload's array-index shape are undocumented and
// reverse-engineered (ported from github.com/AWeirdDev/flights); any
// panic from an unexpected shape is converted into an error rather than
// crashing the caller.
func parseOffers(html []byte) (offers []Offer, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("googleflights: parsing response: %v", r)
		}
	}()

	script, err := extractDS1Script(string(html))
	if err != nil {
		return nil, err
	}

	data, err := extractDataPayload(script)
	if err != nil {
		return nil, err
	}

	var payload []interface{}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil, fmt.Errorf("googleflights: decoding payload: %w", err)
	}

	flightsRaw := asSlice(idx(asSlice(idx(payload, 3)), 0))
	if flightsRaw == nil {
		return nil, nil
	}

	for _, kRaw := range flightsRaw {
		k := asSlice(kRaw)
		flight := asSlice(idx(k, 0))
		price := asInt(idx(asSlice(idx(asSlice(idx(k, 1)), 0)), 1))

		var segments []Segment
		for _, sfRaw := range asSlice(idx(flight, 2)) {
			sf := asSlice(sfRaw)
			depH, depM := asPair(idx(sf, 8))
			arrH, arrM := asPair(idx(sf, 10))
			segments = append(segments, Segment{
				FromAirport:     asString(idx(sf, 3)),
				FromAirportName: asString(idx(sf, 4)),
				ToAirport:       asString(idx(sf, 6)),
				ToAirportName:   asString(idx(sf, 5)),
				DepartureDate:   asTriple(idx(sf, 20)),
				DepartureTime:   [2]int{depH, depM},
				ArrivalDate:     asTriple(idx(sf, 21)),
				ArrivalTime:     [2]int{arrH, arrM},
				DurationMinutes: asInt(idx(sf, 11)),
				PlaneType:       asString(idx(sf, 17)),
			})
		}

		var airlines []string
		for _, a := range asSlice(idx(flight, 1)) {
			airlines = append(airlines, asString(a))
		}

		extras := asSlice(idx(flight, 22))

		offers = append(offers, Offer{
			Type:                       asString(idx(flight, 0)),
			Price:                      price,
			Airlines:                   airlines,
			Segments:                   segments,
			CarbonEmissionGrams:        asInt(idx(extras, 7)),
			TypicalCarbonEmissionGrams: asInt(idx(extras, 8)),
		})
	}

	return offers, nil
}

// extractDS1Script returns the text content of <script class="ds:1" ...>.
func extractDS1Script(html string) (string, error) {
	const marker = `class="ds:1"`
	markerIdx := strings.Index(html, marker)
	if markerIdx == -1 {
		return "", errors.New("googleflights: ds:1 script not found in response")
	}
	tagStart := strings.LastIndex(html[:markerIdx], "<script")
	if tagStart == -1 {
		return "", errors.New("googleflights: malformed ds:1 script tag")
	}
	openEnd := strings.Index(html[tagStart:], ">")
	if openEnd == -1 {
		return "", errors.New("googleflights: malformed ds:1 script tag")
	}
	contentStart := tagStart + openEnd + 1
	closeIdx := strings.Index(html[contentStart:], "</script>")
	if closeIdx == -1 {
		return "", errors.New("googleflights: unterminated ds:1 script tag")
	}
	return html[contentStart : contentStart+closeIdx], nil
}

// extractDataPayload pulls the JSON array out of
// "AF_initDataCallback({key: 'ds:1', ..., data: [...], sideChannel: {}})".
func extractDataPayload(script string) (string, error) {
	const marker = "data:"
	idx := strings.Index(script, marker)
	if idx == -1 {
		return "", errors.New("googleflights: no data payload in ds:1 script")
	}
	rest := script[idx+len(marker):]
	data := rest
	if last := strings.LastIndex(rest, ","); last != -1 {
		data = rest[:last]
	}
	data = strings.TrimSpace(data)
	if strings.HasSuffix(data, "errorHasStatus: true") {
		return "", ErrNotFound
	}
	return data, nil
}

func idx(s []interface{}, i int) interface{} {
	if i < 0 || i >= len(s) {
		return nil
	}
	return s[i]
}

func asSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func asInt(v interface{}) int {
	f, _ := v.(float64)
	return int(f)
}

func asPair(v interface{}) (int, int) {
	s := asSlice(v)
	return asInt(idx(s, 0)), asInt(idx(s, 1))
}

func asTriple(v interface{}) [3]int {
	s := asSlice(v)
	return [3]int{asInt(idx(s, 0)), asInt(idx(s, 1)), asInt(idx(s, 2))}
}

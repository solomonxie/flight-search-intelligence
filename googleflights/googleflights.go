// Package googleflights is a lightweight Google Flights scraper: it builds
// the reverse-engineered "tfs" protobuf query param and does a plain HTTP
// GET against the public search page — no headless browser, no API key.
//
// The tfs schema and the "data:" JS payload shape it parses back out are
// both undocumented and can change without notice; see
// github.com/AWeirdDev/flights (Python) for the reference implementation
// this was ported from.
package googleflights

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const searchURL = "https://www.google.com/travel/flights/search"

// Seat, Trip and Passenger mirror the enums in the reverse-engineered proto
// schema; values must match exactly, they're what gets sent on the wire.
type Seat int

const (
	SeatEconomy  Seat = 1
	SeatPremium  Seat = 2
	SeatBusiness Seat = 3
	SeatFirst    Seat = 4
)

type Trip int

const (
	TripRoundTrip Trip = 1
	TripOneWay    Trip = 2
)

const (
	passengerAdult        = 1
	passengerChild        = 2
	passengerInfantInSeat = 3
	passengerInfantOnLap  = 4
)

// Passengers counts, mirroring fast-flights' Passengers dataclass.
type Passengers struct {
	Adults        int
	Children      int
	InfantsInSeat int
	InfantsOnLap  int
}

func passengersToEnum(p Passengers) []int {
	var out []int
	for i := 0; i < p.Adults; i++ {
		out = append(out, passengerAdult)
	}
	for i := 0; i < p.Children; i++ {
		out = append(out, passengerChild)
	}
	for i := 0; i < p.InfantsInSeat; i++ {
		out = append(out, passengerInfantInSeat)
	}
	for i := 0; i < p.InfantsOnLap; i++ {
		out = append(out, passengerInfantOnLap)
	}
	return out
}

// Leg is one requested origin/date/destination flight-data entry.
type Leg struct {
	Date        string // YYYY-MM-DD
	FromAirport string // IATA code
	ToAirport   string // IATA code
	MaxStops    *int
	Airlines    []string
}

// Query is the full request this package sends to Google Flights.
type Query struct {
	Legs        []Leg
	Seat        Seat
	Trip        Trip
	Passengers  Passengers
	MaxPrice    *int
	CarryOnBags *int
	CheckedBags *int
	Language    string // e.g. "en", blank lets Google decide
	Currency    string // e.g. "USD", blank lets Google decide
}

// tfs returns the base64-encoded protobuf query param.
func (q Query) tfs() string {
	return base64.StdEncoding.EncodeToString(infoMessage(q))
}

// SearchParams is the caller-facing subset of Query used for a plain
// one-way or round-trip route/date search.
type SearchParams struct {
	Origin        string // IATA code, e.g. "SFO"
	Destination   string // IATA code, e.g. "JFK"
	DepartureDate string // YYYY-MM-DD
	ReturnDate    string // YYYY-MM-DD, optional
	Adults        int    // defaults to 1
}

func (p SearchParams) toQuery() Query {
	if p.Adults <= 0 {
		p.Adults = 1
	}
	legs := []Leg{{Date: p.DepartureDate, FromAirport: strings.ToUpper(p.Origin), ToAirport: strings.ToUpper(p.Destination)}}
	trip := TripOneWay
	if p.ReturnDate != "" {
		trip = TripRoundTrip
		legs = append(legs, Leg{Date: p.ReturnDate, FromAirport: strings.ToUpper(p.Destination), ToAirport: strings.ToUpper(p.Origin)})
	}
	return Query{
		Legs:       legs,
		Seat:       SeatEconomy,
		Trip:       trip,
		Passengers: Passengers{Adults: p.Adults},
		Currency:   "USD",
	}
}

// Client fetches Google Flights search results over plain HTTP.
type Client struct {
	HTTPClient *http.Client
}

// NewClient builds a Client with sane defaults.
func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{Timeout: 30 * time.Second}}
}

// SearchFlightOffers fetches results for p and returns both the parsed
// offers and the raw HTML response body (callers writing to a raw zone
// want the untouched bytes, not a struct re-marshal).
func (c *Client) SearchFlightOffers(ctx context.Context, p SearchParams) ([]Offer, []byte, error) {
	if p.Origin == "" || p.Destination == "" || p.DepartureDate == "" {
		return nil, nil, fmt.Errorf("googleflights: origin, destination, and departure date are required")
	}
	return c.search(ctx, p.toQuery())
}

// Search runs an arbitrary Query (multi-leg, seat class, bag counts...).
func (c *Client) Search(ctx context.Context, q Query) ([]Offer, []byte, error) {
	return c.search(ctx, q)
}

func (c *Client) search(ctx context.Context, q Query) ([]Offer, []byte, error) {
	v := url.Values{"tfs": {q.tfs()}}
	if q.Language != "" {
		v.Set("hl", q.Language)
	}
	if q.Currency != "" {
		v.Set("curr", q.Currency)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL+"?"+v.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	// A plain net/http request has no TLS/HTTP2 fingerprint matching a real
	// browser; Google's anti-bot stack may reject it regardless of headers.
	// These headers are the cheap half of looking legitimate, not a promise.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("googleflights: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("googleflights: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, body, fmt.Errorf("googleflights: search failed: status %d", resp.StatusCode)
	}

	offers, err := parseOffers(body)
	if err != nil {
		return nil, body, err
	}
	return offers, body, nil
}

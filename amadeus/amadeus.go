// Package amadeus is a minimal client for the Amadeus Self-Service Flight
// Offers Search API (test environment). Stdlib only, no dependencies.
//
// Docs: https://developers.amadeus.com/self-service/category/flights/api-doc/flight-offers-search
package amadeus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const baseURL = "https://test.api.amadeus.com"

// Client is an OAuth2 client-credentials client for the Amadeus test API.
type Client struct {
	ClientID     string
	ClientSecret string
	HTTPClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// NewClient builds a Client. clientID/clientSecret come from an Amadeus
// Self-Service app at https://developers.amadeus.com/my-apps.
func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// token returns a valid bearer token, authenticating (or re-authenticating,
// on expiry) as needed.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt) {
		return c.accessToken, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v1/security/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("amadeus: auth request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("amadeus: reading auth response: %w", err)
	}

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("amadeus: decoding auth response: %w (body: %s)", err, truncate(body))
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		if tok.Error != "" {
			return "", fmt.Errorf("amadeus: auth failed: %s (%s)", tok.Error, tok.ErrorDesc)
		}
		return "", fmt.Errorf("amadeus: auth failed: status %d: %s", resp.StatusCode, truncate(body))
	}

	c.accessToken = tok.AccessToken
	// Refresh a bit early to avoid racing expiry.
	c.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second - 30*time.Second)
	return c.accessToken, nil
}

// SearchParams are the fields this client sends to Flight Offers Search.
type SearchParams struct {
	Origin        string // IATA code, e.g. "SFO"
	Destination   string // IATA code, e.g. "JFK"
	DepartureDate string // YYYY-MM-DD
	ReturnDate    string // YYYY-MM-DD, optional
	Adults        int    // defaults to 1
	MaxResults    int    // defaults to 10
}

// FlightOffer is the subset of the Amadeus flight-offer shape this client
// cares about. The raw API response has many more fields; callers that need
// them should keep the raw JSON (see collector's raw-zone write) rather than
// extend this struct.
type FlightOffer struct {
	ID          string      `json:"id"`
	Price       Price       `json:"price"`
	Itineraries []Itinerary `json:"itineraries"`
}

type Price struct {
	Total    string `json:"total"`
	Currency string `json:"currency"`
}

type Itinerary struct {
	Duration string    `json:"duration"`
	Segments []Segment `json:"segments"`
}

type Segment struct {
	Departure   FlightPoint `json:"departure"`
	Arrival     FlightPoint `json:"arrival"`
	CarrierCode string      `json:"carrierCode"`
	Number      string      `json:"number"`
}

type FlightPoint struct {
	IATACode string `json:"iataCode"`
	At       string `json:"at"`
}

// SearchResponse is the raw top-level Flight Offers Search response.
type SearchResponse struct {
	Data     []FlightOffer          `json:"data"`
	Meta     map[string]interface{} `json:"meta"`
	Errors   []APIError             `json:"errors"`
	Warnings []APIError             `json:"warnings"`
}

type APIError struct {
	Status int    `json:"status"`
	Code   int    `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// SearchFlightOffers calls GET /v2/shopping/flight-offers and returns both
// the parsed response and the raw response body (callers writing to a raw
// zone want the untouched bytes, not a struct re-marshal).
func (c *Client) SearchFlightOffers(ctx context.Context, p SearchParams) (*SearchResponse, []byte, error) {
	if p.Origin == "" || p.Destination == "" || p.DepartureDate == "" {
		return nil, nil, errors.New("amadeus: origin, destination, and departure date are required")
	}
	if p.Adults <= 0 {
		p.Adults = 1
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 10
	}

	tok, err := c.token(ctx)
	if err != nil {
		return nil, nil, err
	}

	q := url.Values{
		"originLocationCode":      {strings.ToUpper(p.Origin)},
		"destinationLocationCode": {strings.ToUpper(p.Destination)},
		"departureDate":           {p.DepartureDate},
		"adults":                  {strconv.Itoa(p.Adults)},
		"max":                     {strconv.Itoa(p.MaxResults)},
		"currencyCode":            {"USD"},
	}
	if p.ReturnDate != "" {
		q.Set("returnDate", p.ReturnDate)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/v2/shopping/flight-offers?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("amadeus: search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("amadeus: reading search response: %w", err)
	}

	var out SearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, body, fmt.Errorf("amadeus: decoding search response: %w (body: %s)", err, truncate(body))
	}
	if resp.StatusCode != http.StatusOK {
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return &out, body, fmt.Errorf("amadeus: search failed: %s: %s (status %d)", e.Title, e.Detail, resp.StatusCode)
		}
		return &out, body, fmt.Errorf("amadeus: search failed: status %d: %s", resp.StatusCode, truncate(body))
	}

	return &out, body, nil
}

func truncate(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

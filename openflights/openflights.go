// Package openflights loads the OpenFlights airports/routes reference
// dataset — static, bundled-style data (fetched once and cached locally,
// not scraped) used to prune "every airport on Earth" down to airport
// pairs someone actually flies, before any provider is queried. See
// DESIGN.md "Cheap multi-leg route search".
package openflights

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

const (
	airportsURL = "https://raw.githubusercontent.com/jpatokal/openflights/master/data/airports.dat"
	routesURL   = "https://raw.githubusercontent.com/jpatokal/openflights/master/data/routes.dat"

	airportsFile = "airports.dat"
	routesFile   = "routes.dat"

	earthRadiusMiles = 3958.8
)

// Airport is the subset of an airports.dat row this package cares about.
type Airport struct {
	IATA     string
	Name     string
	City     string
	Country  string
	Lat, Lon float64
	Timezone string // IANA name, e.g. "America/Los_Angeles"; "" if unknown
}

// Graph is airports keyed by IATA code plus a nonstop-route adjacency:
// Routes[from][to] means some airline flies from directly to to.
type Graph struct {
	Airports map[string]Airport
	Routes   map[string]map[string]bool
}

// Airport looks up an airport by IATA code.
func (g *Graph) Airport(iata string) (Airport, bool) {
	a, ok := g.Airports[iata]
	return a, ok
}

// HasNonstop reports whether some airline flies from directly to.
func (g *Graph) HasNonstop(from, to string) bool {
	return g.Routes[from] != nil && g.Routes[from][to]
}

// CandidateHubs returns every airport h (other than origin/destination)
// with a nonstop route both origin->h and h->destination — the raw
// candidate set before any geometry/price pruning.
func (g *Graph) CandidateHubs(origin, destination string) []string {
	var hubs []string
	for h := range g.Routes[origin] {
		if h == destination {
			continue
		}
		if g.Routes[h] != nil && g.Routes[h][destination] {
			hubs = append(hubs, h)
		}
	}
	return hubs
}

// DistanceMiles is the great-circle (haversine) distance between two
// airports.
func DistanceMiles(a, b Airport) float64 {
	lat1, lon1 := radians(a.Lat), radians(a.Lon)
	lat2, lon2 := radians(b.Lat), radians(b.Lon)
	dLat, dLon := lat2-lat1, lon2-lon1
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusMiles * math.Asin(math.Sqrt(h))
}

func radians(deg float64) float64 { return deg * math.Pi / 180 }

// Load reads airports.dat/routes.dat from cacheDir, downloading them
// first (once) if they're not already there.
func Load(cacheDir string) (*Graph, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("openflights: creating cache dir: %w", err)
	}

	airportsPath := filepath.Join(cacheDir, airportsFile)
	if err := ensureCached(airportsPath, airportsURL); err != nil {
		return nil, err
	}
	routesPath := filepath.Join(cacheDir, routesFile)
	if err := ensureCached(routesPath, routesURL); err != nil {
		return nil, err
	}

	airports, err := parseAirports(airportsPath)
	if err != nil {
		return nil, err
	}
	routes, err := parseRoutes(routesPath)
	if err != nil {
		return nil, err
	}
	return &Graph{Airports: airports, Routes: routes}, nil
}

func ensureCached(path, url string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("openflights: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openflights: downloading %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("openflights: creating %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("openflights: writing %s: %w", path, err)
	}
	return nil
}

// airports.dat columns: id,name,city,country,iata,icao,lat,lon,alt,
// tz-offset,dst,tz-name,type,source
func parseAirports(path string) (map[string]Airport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("openflights: opening %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	airports := make(map[string]Airport)
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Tolerate the occasional malformed row rather than failing
			// the whole (large, third-party) dataset load.
			continue
		}
		if len(rec) < 12 {
			continue
		}
		iata := rec[4]
		if len(iata) != 3 {
			continue
		}
		lat, err1 := strconv.ParseFloat(rec[6], 64)
		lon, err2 := strconv.ParseFloat(rec[7], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		airports[iata] = Airport{
			IATA:     iata,
			Name:     rec[1],
			City:     rec[2],
			Country:  rec[3],
			Lat:      lat,
			Lon:      lon,
			Timezone: rec[11],
		}
	}
	return airports, nil
}

// routes.dat columns: airline,airlineID,source,sourceID,dest,destID,
// codeshare,stops,equipment
func parseRoutes(path string) (map[string]map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("openflights: opening %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	routes := make(map[string]map[string]bool)
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < 8 {
			continue
		}
		source, dest, stops := rec[2], rec[4], rec[7]
		if len(source) != 3 || len(dest) != 3 || stops != "0" {
			continue // only direct (nonstop) legs are edges in this graph
		}
		if routes[source] == nil {
			routes[source] = make(map[string]bool)
		}
		routes[source][dest] = true
	}
	return routes, nil
}

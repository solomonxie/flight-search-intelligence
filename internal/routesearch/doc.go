// Package routesearch implements the "Cheap multi-leg route search"
// algorithm from DESIGN.md: best-first branch-and-bound over hub
// candidates drawn from the openflights route-existence graph, spending
// one real googleflights scrape per edge under a hard query budget,
// producing both a ranked (price, duration) result set and a full JSON
// audit trail of every candidate considered.
//
// This is the same direct-run shortcut cmd/collector already takes:
// no Temporal yet, so "pacing" is a plain time.Sleep between queries
// rather than a durable workflow timer, and the audit trail is written
// once at the end rather than incrementally per step. See DESIGN.md
// "Pacing, audit trail, and observability" for the target version.
//
// File layout, by concern rather than by request-shape order:
//   - types.go: shared request/audit-trail data shapes (Params, Deps,
//     Plan, Result, ...).
//   - candidates.go: Step 0/1 -- hub candidate generation, the geometry
//     prune, and lower-bound ranking. No scraping.
//   - offers.go: the only place that calls googleflights -- the
//     cache-first searchOffers wrapper and the flight_prices recorder.
//   - scoring.go: pure functions over already-fetched offers/results --
//     feasibility, cheapest-of, Pareto membership.
//   - search.go, roundtrip.go, flexible.go: the three request-shape
//     entry points (Search, SearchRoundTrip, SearchFlexible), each
//     orchestrating the pieces above.
//   - timeutil.go, util.go: small generic helpers with no domain logic
//     of their own.
package routesearch

# OpenFlights Route Graph

Loads the [OpenFlights](https://openflights.org/data.php) airports/routes
reference dataset — static data, downloaded once and cached locally, not
scraped — and turns it into the route-existence graph
`internal/routesearch`'s hub search prunes candidates against before
spending a single real query. See DESIGN.md "Cheap multi-leg route
search" for why this matters: "try every airport on Earth" is
combinatorially impossible, but "airport pairs someone actually flies" is
a short list.

### `openflights.go`

The only file. `Load(cacheDir)` is the entry point: downloads
`airports.dat`/`routes.dat` into `cacheDir` if they're not already there
(`ensureCached`, a one-time fetch — subsequent calls just read the cached
files), then parses both into a `Graph`.

`Graph` is two things: `Airports` (IATA code → coordinates/timezone/name)
and `Routes` (an adjacency map — `Routes[from][to]` means some airline
flies that leg nonstop; built from `routes.dat` rows filtered to `stops
== "0"`, so only genuinely direct legs count as graph edges).
`CandidateHubs(origin, destination)` is the one query `routesearch`
actually calls: every airport with a nonstop route *both* from origin
*and* to destination — the raw candidate set before any geometry or
price pruning happens. `DistanceMiles` (haversine, great-circle) is the
other piece `routesearch` needs, feeding the geometry prune's minimum-
possible-flight-time estimate — used only as a feasibility filter, never
a price-ranking signal, since fare price doesn't track distance.

Parsing (`parseAirports`/`parseRoutes`) tolerates malformed rows rather
than failing the whole load — this is a large, third-party CSV dataset,
and one bad row shouldn't take down every search.

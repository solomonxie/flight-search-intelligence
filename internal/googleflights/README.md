# Google Flights Client

A lightweight Google Flights scraper: builds the reverse-engineered
`tfs` protobuf query param, does a plain HTTP GET against the public
search page, and parses the results back out of an embedded JS payload.
No headless browser, no API key — ported from the approach in
[AWeirdDev/flights](https://github.com/AWeirdDev/flights) (Python). Both
the `tfs` schema and the response payload shape are undocumented and can
change without notice; that instability is the whole reason `parse.go`
converts a panic into an error instead of crashing the caller.

### `googleflights.go`

The public surface. `Client.SearchFlightOffers` is what most callers
use — a plain one-way/round-trip route+date search (`SearchParams` →
`toQuery()` builds the full `Query` struct, defaulting to one adult and
economy seat). `Client.Search` takes a full `Query` directly for anything
`SearchParams` doesn't expose (multi-leg, seat class, bag counts, max
price). Either way, `search()` does the actual HTTP GET — with browser-
like headers as "the cheap half of looking legitimate, not a promise"
against Google's anti-bot stack — and returns both the parsed `[]Offer`
and the raw response bytes (so a raw-zone writer gets untouched bytes,
not a re-marshal).

### `parse.go`

`parseOffers` extracts flight offers from the search page's embedded
`AF_initDataCallback` payload (the `<script class="ds:1">` tag). The
payload is a deeply nested, unlabeled JSON array — reverse-engineered,
not documented — so this file is mostly small `idx`/`asSlice`/`asString`/
`asInt` helpers navigating fixed array indices into `Offer`/`Segment`
fields. `ErrNotFound` is returned when the payload's own
`errorHasStatus: true` marker says there were no results.

### `protobuf.go`

Hand-rolled proto3 wire-format encoding for the small subset of schema
the `tfs` query param needs (documented as a comment at the top of the
file — `Info`, `FlightData`, `Airport`, `Baggage`). No protobuf library
required: proto3's wire format is a handful of varint/length-delimited
primitives, encoded here with plain `putVarint`/`putString`/`putBytes`
helpers rather than pulling in a codegen dependency for four small
messages.

// Package catalog is the local-dev serving store: SQLite, standing in for
// the Postgres serving store DESIGN.md targets (see "Local development").
// Collector writes here directly for now, skipping the Spark/Delta Lake/
// dbt gold pipeline that will eventually sit in between.
//
// This package never creates or alters schema — see DESIGN.md "Schema
// ownership": that's DBA/ops tooling's job, not Go's, even here. Schema
// lives in databases/sqlite/migrations/, applied with Flyway
// (`make db-init`) before this package's Open is ever called — Open
// fails fast, not silently, if that hasn't happened yet.
package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var requiredTables = []string{"flight_prices", "flight_offers_cache", "route_search_plans", "agent_requests", "agent_tasks"}

// FlightPrice is one row of etl/dbt's `raw.flight_prices` source shape
// (see etl/dbt/models/staging/stg_flights.sql) — one price observation.
type FlightPrice struct {
	Origin      string
	Destination string
	Airline     string // comma-joined carrier codes, e.g. "AA" or "AA,B6"
	DepartDate  string // YYYY-MM-DD
	ReturnDate  string // YYYY-MM-DD, empty for one-way
	PriceCents  int64
	Currency    string
	Source      string // provider name, e.g. "google_flights"
	CreatedAt   time.Time
}

// SQLite is a thin wrapper around the flight_prices table.
type SQLite struct {
	db *sql.DB
}

// Open opens the SQLite database at path and checks that Flyway's
// migrations (databases/sqlite/migrations/) have already been applied —
// it does not create tables itself.
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("catalog: opening %s: %w", path, err)
	}
	if err := checkSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// checkSchema fails fast, with a pointer to the fix, rather than letting
// the first real query fail with a confusing "no such table".
func checkSchema(db *sql.DB) error {
	for _, table := range requiredTables {
		row := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
		var exists int
		if err := row.Scan(&exists); err != nil {
			return fmt.Errorf(
				"catalog: table %q not found — run `make db-init` first to apply databases/sqlite/migrations (%w)",
				table, err)
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// InsertFlightPrices inserts rows in one transaction.
func (s *SQLite) InsertFlightPrices(ctx context.Context, rows []FlightPrice) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO flight_prices
			(origin, destination, airline, depart_date, return_date, price_cents, currency, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("catalog: preparing insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.Origin, r.Destination, r.Airline, r.DepartDate, r.ReturnDate,
			r.PriceCents, r.Currency, r.Source, r.CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("catalog: inserting row: %w", err)
		}
	}

	return tx.Commit()
}

// CachedPriceCents returns the cheapest previously-seen *one-way* price
// for this exact (origin, destination, depart_date), if the store has
// one — used as the price-aware lower-bound estimate in routesearch
// instead of the cruder distance × $/mile prior. ok is false if nothing's
// cached yet. Explicitly excludes round-trip rows (non-empty return_date):
// a round trip's price_cents is the bundled *total*, not a one-way price,
// and mixing the two would make this lower bound wildly wrong.
func (s *SQLite) CachedPriceCents(ctx context.Context, origin, destination, departDate string) (cents int64, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT MIN(price_cents) FROM flight_prices
		WHERE origin = ? AND destination = ? AND depart_date = ? AND return_date = ''`,
		origin, destination, departDate)

	var n sql.NullInt64
	if err := row.Scan(&n); err != nil {
		return 0, false, fmt.Errorf("catalog: reading cached price: %w", err)
	}
	if !n.Valid {
		return 0, false, nil
	}
	return n.Int64, true, nil
}

// CachedOffers returns the most recent flight_offers_cache row for this
// exact (origin, destination, depart_date, return_date), if one was
// scraped within maxAge — the cache-first read that lets a search skip a
// live Google Flights scrape. offersJSON is the raw JSON this search's
// caller marshaled the offers slice into; ok is false on a miss (nothing
// cached, or the newest row is older than maxAge).
func (s *SQLite) CachedOffers(ctx context.Context, origin, destination, departDate, returnDate string, maxAge time.Duration) (offersJSON []byte, createdAt time.Time, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT offers_json, created_at FROM flight_offers_cache
		WHERE origin = ? AND destination = ? AND depart_date = ? AND return_date = ?
		ORDER BY created_at DESC LIMIT 1`,
		origin, destination, departDate, returnDate)

	var json, createdAtStr string
	if err := row.Scan(&json, &createdAtStr); err != nil {
		if err == sql.ErrNoRows {
			return nil, time.Time{}, false, nil
		}
		return nil, time.Time{}, false, fmt.Errorf("catalog: reading cached offers: %w", err)
	}
	createdAt, err = time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("catalog: parsing cached created_at: %w", err)
	}
	if time.Since(createdAt) > maxAge {
		return nil, time.Time{}, false, nil
	}
	return []byte(json), createdAt, true, nil
}

// SaveOffersCache records one live scrape's raw offers for future
// CachedOffers reads. offersJSON is the caller's own JSON encoding of
// the offers slice — this package doesn't depend on googleflights, so it
// stores the bytes opaquely rather than the typed offers.
func (s *SQLite) SaveOffersCache(ctx context.Context, origin, destination, departDate, returnDate, source string, offersJSON []byte, createdAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO flight_offers_cache (origin, destination, depart_date, return_date, source, offers_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		origin, destination, departDate, returnDate, source, string(offersJSON), createdAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("catalog: saving offers cache: %w", err)
	}
	return nil
}

// SaveRouteSearchPlan upserts one route-search audit-trail row (see
// DESIGN.md "Audit trail") — called once when a search starts (status
// "running", so a crash mid-search still leaves a trace) and again when
// it finishes.
func (s *SQLite) SaveRouteSearchPlan(ctx context.Context, id, status string, planJSON []byte) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO route_search_plans (id, created_at, updated_at, status, plan_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET updated_at = excluded.updated_at,
			status = excluded.status, plan_json = excluded.plan_json`,
		id, now, now, status, string(planJSON))
	if err != nil {
		return fmt.Errorf("catalog: saving route search plan: %w", err)
	}
	return nil
}

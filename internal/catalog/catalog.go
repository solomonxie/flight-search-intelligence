// Package catalog is the local-dev serving store: SQLite, standing in for
// the Postgres serving store DESIGN.md targets (see "Local development").
// Collector writes here directly for now, skipping the Spark/Delta Lake/
// dbt gold pipeline that will eventually sit in between.
package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

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
	ScrapedAt   time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS flight_prices (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	origin       TEXT NOT NULL,
	destination  TEXT NOT NULL,
	airline      TEXT NOT NULL,
	depart_date  TEXT NOT NULL,
	return_date  TEXT NOT NULL DEFAULT '',
	price_cents  INTEGER NOT NULL,
	currency     TEXT NOT NULL,
	source       TEXT NOT NULL,
	scraped_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS route_search_plans (
	id          TEXT PRIMARY KEY,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	status      TEXT NOT NULL,
	plan_json   TEXT NOT NULL
)`

// SQLite is a thin wrapper around the flight_prices table.
type SQLite struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and ensures
// the flight_prices table exists.
func Open(path string) (*SQLite, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("catalog: creating db directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("catalog: opening %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("catalog: creating schema: %w", err)
	}
	return &SQLite{db: db}, nil
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
			(origin, destination, airline, depart_date, return_date, price_cents, currency, source, scraped_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("catalog: preparing insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.Origin, r.Destination, r.Airline, r.DepartDate, r.ReturnDate,
			r.PriceCents, r.Currency, r.Source, r.ScrapedAt.UTC().Format(time.RFC3339),
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

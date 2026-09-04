// Command dbinit applies etl/sql/schema.sql to a local SQLite database.
// Run once before cmd/collector or cmd/routesearch — neither creates
// tables itself; catalog.Open fails fast with a pointer back here if
// the schema hasn't been applied yet.
//
// Deliberately not a migration framework (no versioning, no up/down):
// schema.sql is one flat, idempotent (CREATE TABLE IF NOT EXISTS) file.
// Revisit if the schema ever needs to evolve under existing data rather
// than just exist.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dbinit:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "data/flight_search.db", "SQLite database path")
	schemaPath := flag.String("schema", "etl/sql/schema.sql", "path to the schema SQL file")
	flag.Parse()

	if dir := filepath.Dir(*dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating db directory: %w", err)
		}
	}

	schema, err := os.ReadFile(*schemaPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *schemaPath, err)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", *dbPath, err)
	}
	defer db.Close()

	if _, err := db.Exec(string(schema)); err != nil {
		return fmt.Errorf("applying %s: %w", *schemaPath, err)
	}

	fmt.Printf("Applied %s to %s\n", *schemaPath, *dbPath)
	return nil
}

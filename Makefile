DB_PATH ?= data/flight_search.db

.PHONY: build db-init run-collector run-search-api test

build:
	go build ./...

# Schema is DBA/ops tooling's job, not Go's — see DESIGN.md "Schema
# ownership". Applied directly with the database's own CLI, not a Go
# binary; db/schema.sql is the one source of truth for the shape.
db-init:
	mkdir -p $(dir $(DB_PATH))
	sqlite3 $(DB_PATH) < db/schema.sql

run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

test:
	go test ./...

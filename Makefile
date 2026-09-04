.PHONY: build db-init run-collector run-search-api test

build:
	go build ./...

# Schema is DBA/ops tooling's job, not Go's — see DESIGN.md "Schema
# ownership". Flyway applies versioned migrations from
# databases/sqlite/migrations/; requires the flyway CLI (brew install
# flyway) on PATH.
db-init:
	flyway -configFiles=databases/sqlite/flyway.toml migrate

run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

test:
	go test ./...

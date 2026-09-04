.PHONY: build db-init run-collector run-search-api run-collector-worker run-email-intake-worker test

build:
	go build ./...

db-init:
	flyway -configFiles=databases/sqlite/flyway.toml migrate

run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

# Agent loop (see DESIGN.md "Agent loop" / "Collector task dispatch") —
# store-backed poll workers, no message broker or workflow engine. Run
# both alongside each other for the loop to actually advance requests:
# email-intake decides/dispatches, collector claims/runs the fetches.
run-collector-worker:
	go run ./cmd/collector -worker

run-email-intake-worker:
	go run ./cmd/email-intake -worker

test:
	go test ./...

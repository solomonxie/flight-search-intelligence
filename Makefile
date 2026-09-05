.PHONY: build db-init run-collector run-search-api dry-run-route-search dry-run-best-dates \
	dry-run-route-search-full run-email-intake-start run-email-intake-signal \
	run-collector-worker run-agent-worker kafka-topics test

build:
	go build ./...

# Apply databases/sqlite/migrations/ (Flyway) -- run once before anything
# else below touches the store.
db-init:
	flyway -configFiles=databases/sqlite/flyway.toml migrate

# One-shot single-leg scrape, no store dependency beyond db-init.
run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

ORIGIN ?= YVR
DEST ?= PEK
DATE ?= 2026-12-15
RETURN ?= 2027-01-10
WINDOW_DAYS ?= 20

# routesearch direct-run modes (no Kafka/agent loop) -- see cmd/routesearch's
# package doc. All three take ORIGIN/DEST/DATE/RETURN/WINDOW_DAYS overrides,
# e.g. `make dry-run-route-search-full ORIGIN=SFO DEST=NRT`.
dry-run-route-search:
	go run ./cmd/routesearch -origin $(ORIGIN) -destination $(DEST) -date $(DATE) -dry-run

dry-run-best-dates:
	go run ./cmd/routesearch -origin $(ORIGIN) -destination $(DEST) -date $(DATE) \
		-date-window-days $(WINDOW_DAYS) -scan-dates

dry-run-route-search-full:
	go run ./cmd/routesearch -origin $(ORIGIN) -destination $(DEST) -date $(DATE) -return-date $(RETURN)

# Full agent loop (see DESIGN.md "Agent loop" and internal/kafka's package
# doc) -- each step is a Kafka message, not a poll loop. Order:
#   1. `make db-init` (once)
#   2. `make kafka-topics` (once, needs a local broker already running --
#      `kafka-server-start`, or any Kafka-API-compatible broker)
#   3. `make run-collector-worker` and `make run-agent-worker`, alongside
#      each other -- both must be running for a request to advance at all
#   4. `make run-email-intake-start` to create a request; it prints the
#      request id `make run-email-intake-signal` needs for a follow-up
kafka-topics:
	kafka-topics --bootstrap-server localhost:9092 --create --if-not-exists --topic agent-decisions --partitions 3 --replication-factor 1
	kafka-topics --bootstrap-server localhost:9092 --create --if-not-exists --topic search-tasks --partitions 3 --replication-factor 1

run-collector-worker:
	go run ./cmd/collector -worker

run-agent-worker:
	go run ./cmd/agent-worker

REQUEST_ID ?=
TEXT ?= must be there for Christmas

# Fixture-driven stand-in for SES inbound (see DESIGN.md "Local development"
# "Email intake") -- -start creates a new request, -signal appends a
# follow-up to one already created. `make run-email-intake-signal
# REQUEST_ID=<id>` needs the id -start printed.
run-email-intake-start:
	go run ./cmd/email-intake -start -origin $(ORIGIN) -destination $(DEST) -date $(DATE) -return-date $(RETURN)

run-email-intake-signal:
	go run ./cmd/email-intake -signal -request-id $(REQUEST_ID) -text "$(TEXT)"

test:
	go test ./...

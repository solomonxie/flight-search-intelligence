.PHONY: build db-init run-collector run-search-api dry-run-best-dates dry-run-best-routes \
	dry-run-route-search-full run-email-intake-start run-email-intake-signal \
	dry-run-agent-interactive run-collector-worker run-agent-worker kafka-topics test

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

dry-run-best-dates:
	go run ./cmd/routesearch -origin $(ORIGIN) -destination $(DEST) -date $(DATE) \
		-date-window-days $(WINDOW_DAYS) -scan-dates

dry-run-best-routes:
	go run ./cmd/routesearch -origin $(ORIGIN) -destination $(DEST) -date $(DATE) -dry-run

dry-run-route-search-full:
	go run ./cmd/routesearch -origin $(ORIGIN) -destination $(DEST) -date $(DATE) -return-date $(RETURN)

kafka-topics:
	kafka-topics --bootstrap-server localhost:9092 --create --if-not-exists --topic agent-decisions --partitions 3 --replication-factor 1
	kafka-topics --bootstrap-server localhost:9092 --create --if-not-exists --topic search-tasks --partitions 3 --replication-factor 1

run-collector-worker:
	go run ./cmd/collector -worker

run-agent-worker:
	go run ./cmd/agent-worker

REQUEST_ID ?=
REQUEST_TEXT ?= Fly $(ORIGIN) to $(DEST) on $(DATE), one way
TEXT ?= must be there for Christmas

run-email-intake-start:
	go run ./cmd/email-intake -start -text "$(REQUEST_TEXT)"

run-email-intake-signal:
	go run ./cmd/email-intake -signal -request-id $(REQUEST_ID) -text "$(TEXT)"

dry-run-agent-interactive:
	go run ./cmd/email-intake -interactive

test:
	go test ./...

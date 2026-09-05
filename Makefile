.PHONY: build db-init run-collector run-search-api run-collector-worker run-agent-worker kafka-topics test

build:
	go build ./...

db-init:
	flyway -configFiles=databases/sqlite/flyway.toml migrate

run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

# Agent loop (see DESIGN.md "Agent loop" and internal/kafka's package doc)
# — each step is a Kafka message, not a poll loop. Needs a local broker
# running first (`kafka-server-start`, or any Kafka-API-compatible
# broker) and both topics created once (`make kafka-topics`). Run both
# workers alongside each other for the loop to actually advance requests;
# create a request with `go run ./cmd/email-intake -start ...`.
run-collector-worker:
	go run ./cmd/collector -worker

run-agent-worker:
	go run ./cmd/agent-worker

kafka-topics:
	kafka-topics --bootstrap-server localhost:9092 --create --if-not-exists --topic agent-decisions --partitions 3 --replication-factor 1
	kafka-topics --bootstrap-server localhost:9092 --create --if-not-exists --topic search-tasks --partitions 3 --replication-factor 1

test:
	go test ./...

.PHONY: build db-init run-collector run-search-api test

build:
	go build ./...

db-init:
	go run ./cmd/dbinit

run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

test:
	go test ./...

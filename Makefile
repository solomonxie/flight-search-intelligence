.PHONY: build run-collector run-search-api test up down

build:
	go build ./...

run-collector:
	go run ./cmd/collector -origin SFO -destination JFK -date 2026-12-05

run-search-api:
	go run ./cmd/search-api

test:
	go test ./...

up:
	docker compose up -d

down:
	docker compose down

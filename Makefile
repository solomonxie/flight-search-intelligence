.PHONY: build run-collector run-search-api test up down

build:
	go build ./...

run-collector:
	go run ./cmd/collector

run-search-api:
	go run ./cmd/search-api

test:
	go test ./...

up:
	docker compose up -d

down:
	docker compose down

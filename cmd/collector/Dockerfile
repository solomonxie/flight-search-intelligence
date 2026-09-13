# Build context is the repo root, not this directory:
#   docker build -f cmd/collector/Dockerfile -t flight-search-intelligence/collector .
#
# Deploys via helm/collector as a Deployment running `-worker` mode
# (default CMD below) — the direct-run CLI mode (no args) is a local/dev
# path, not how this image is meant to run in the cluster. /app/data is
# where the Helm chart mounts a PersistentVolume for the SQLite store,
# raw scrape drops, and the OpenFlights cache.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/collector ./cmd/collector

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app \
    && mkdir -p /app/data/raw /app/data/openflights \
    && chown -R app:app /app
COPY --from=build /out/collector /usr/local/bin/collector
WORKDIR /app
USER app
ENTRYPOINT ["/usr/local/bin/collector"]
CMD ["-worker", "-db", "data/flight_search.db", "-out-dir", "data/raw", "-openflights-dir", "data/openflights"]

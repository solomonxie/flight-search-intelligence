# Build context is the repo root, not this directory:
#   docker build -f cmd/search-api/Dockerfile -t flight-search-intelligence/search-api .
#
# Deploys via helm/search-api as a Deployment + Service. Scaffold today
# (see main.go) — the image builds and runs, it just doesn't serve
# anything real yet.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/search-api ./cmd/search-api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app \
    && mkdir -p /app/data \
    && chown -R app:app /app
COPY --from=build /out/search-api /usr/local/bin/search-api
WORKDIR /app
USER app
ENTRYPOINT ["/usr/local/bin/search-api"]

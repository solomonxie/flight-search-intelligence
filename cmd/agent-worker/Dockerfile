# Build context is the repo root (needs go.mod/go.sum + internal/), not
# this directory:
#   docker build -f cmd/agent-worker/Dockerfile -t flight-search-intelligence/agent-worker .
#
# Deploys via helm/agent-worker as a Deployment — this is the persistent
# decision-loop consumer (see cmd/agent-worker's package doc). Configured
# entirely by env vars (DB_PATH, KAFKA_BROKERS, LLM_BACKEND/...) — no
# flags — so the Helm chart's env: block is the whole interface.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agent-worker ./cmd/agent-worker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app \
    && mkdir -p /app/data \
    && chown -R app:app /app
COPY --from=build /out/agent-worker /usr/local/bin/agent-worker
WORKDIR /app
USER app
ENTRYPOINT ["/usr/local/bin/agent-worker"]

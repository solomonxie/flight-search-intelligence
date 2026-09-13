# Build context is the repo root, not this directory:
#   docker build -f cmd/email-intake/Dockerfile -t flight-search-intelligence/email-intake .
#
# Deploys via helm/email-intake as a Job template, not a Deployment —
# this binary is one-shot by design (-start/-signal/-interactive, see
# its package doc), standing in for SES-inbound until that's built. No
# default CMD: the caller always supplies which mode + -text/-request-id.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/email-intake ./cmd/email-intake

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app \
    && mkdir -p /app/data \
    && chown -R app:app /app
COPY --from=build /out/email-intake /usr/local/bin/email-intake
WORKDIR /app
USER app
ENTRYPOINT ["/usr/local/bin/email-intake"]

# Build context: databases/postgres/ itself, not the repo root — this
# image is self-contained (config + migrations only, no Go code needed):
#   docker build -t flight-search-intelligence/postgres-flyway-migrate databases/postgres
#
# Bakes this repo's own Postgres migrations into the official flyway
# image so helm/flyway-migrate's Job needs nothing mounted at deploy
# time. migrations/ is empty today (no Postgres schema has been written
# yet — see databases/README.md); `flyway migrate` against zero
# migration scripts is a harmless no-op, not an error.
FROM flyway/flyway:10
COPY migrations /flyway/sql
COPY flyway.toml /flyway/conf/flyway.toml

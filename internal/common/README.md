# Common Utilities

Small utilities with no better home of their own — kept deliberately
tiny; anything that grows a real theme of its own should move out into
its own package rather than accumulate here.

### `env.go`

`Load(path)` reads `KEY=VALUE` lines from a `.env`-style file and calls
`os.Setenv` for each one — a minimal stdlib substitute for a `.env`
dependency. Blank lines and `# comment` lines are skipped, quotes around
a value are trimmed, and any key already set in the real environment
wins over the file (so `FOO=bar go run ./cmd/...` always overrides
whatever `.env` says). A missing file is not an error: every `main.go`
in `cmd/` calls `common.Load(".env")` and ignores the error, so local dev
can drop a `.env` in the repo root without it being required anywhere.

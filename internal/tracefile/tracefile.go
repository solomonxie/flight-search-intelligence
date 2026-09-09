// Package tracefile writes a request's audit trail to a plain JSON file
// on disk — a human/grep/jq-inspectable export alongside the database
// rows routesearch/agents already save (DESIGN.md "Wide fuzzy-range
// search... a trace file"), not a replacement for them. Mirrors
// cmd/collector/main.go's writeRaw idiom: a raw-zone stand-in directory,
// one file per write, named so repeat runs don't collide.
package tracefile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Dir is where trace files land. A plain env var, not a flag threaded
// through every caller (cmd/routesearch, cmd/collector -worker,
// cmd/email-intake all write here and need to agree on one place
// without each learning a new flag). Defaults to data/traces.
func Dir() string {
	if d := os.Getenv("TRACE_FILE_DIR"); d != "" {
		return d
	}
	return "data/traces"
}

// Write marshals v as indented JSON to <Dir()>/<name>.json, creating the
// directory if needed, and returns the path written. A later Write of
// the same name overwrites — the same "upsert, not append" shape
// route_search_plans' own DB row already has, so a request's trace file
// only ever needs one name (its request id) even though the plan behind
// it is saved more than once as a search progresses.
func Write(name string, v any) (string, error) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("tracefile: creating %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("tracefile: encoding %s: %w", name, err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("tracefile: writing %s: %w", path, err)
	}
	return path, nil
}

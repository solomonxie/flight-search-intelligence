package common

import (
	"log/slog"
	"os"
	"strings"
)

// LogLevel reads LOG_LEVEL (debug|info|warn|error, case-insensitive;
// default warn) — the one knob every cmd/ binary's slog.TextHandler
// uses, so turning on verbose output (e.g. internal/agents' full LLM
// prompt/reply transcripts at Debug, or the per-round/per-hub progress
// trail at Info) is one env var, not a flag per binary. Defaults to warn
// rather than info so a plain -interactive run stays readable — set
// LOG_LEVEL=info (or debug, for the full LLM transcripts) to bring the
// trail back.
func LogLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

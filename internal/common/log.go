package common

import (
	"log/slog"
	"os"
	"strings"
)

// LogLevel reads LOG_LEVEL (debug|info|warn|error, case-insensitive;
// default info) — the one knob every cmd/ binary's slog.TextHandler
// uses, so turning on verbose output (e.g. internal/agents' full LLM
// prompt/reply transcripts, only logged at Debug) is one env var, not a
// flag per binary.
func LogLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

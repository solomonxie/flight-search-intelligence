package agents

import (
	"log/slog"
	"os"

	"flight-search-intelligence/internal/common"
)

// debugLog is DecideNextAction/FormSpec's one shared logger. The full
// LLM call transcript (system prompt, user prompt, raw reply) is a lot
// of text to always show — it's logged at Debug (LOG_LEVEL=debug turns
// it on); a one-line action/reasoning summary always logs at Info, so
// the normal case still shows what the agent decided and why, just not
// the whole prompt behind it.
var debugLog = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: common.LogLevel()}))

package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// LLMClient is the one adapter interface between the agent loop's
// judgment calls (DecideNextAction, FormSpec) and a specific model
// provider's SDK — DESIGN.md "Open decisions: LLM choice / call shape".
// One method, no schema type: every caller sends a system prompt (the
// role/task/output-shape instructions) and a user prompt (the actual
// spec/history/request text) and gets back the model's raw text reply,
// which the caller — not this interface — parses as JSON. Keeping it
// this thin is what lets DecideNextAction and FormSpec each define their
// own JSON shape without this package, or either backend below, caring
// what it is.
type LLMClient interface {
	Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// NewLLMClientFromEnv picks a backend the way DESIGN.md's "LLM choice"
// describes: local Ollama (default, no key, for dev/simulation) or
// OpenAI (prod, needs an API key) — selectable via config, not a
// compile-time branch.
//
//	LLM_BACKEND: "ollama" (default) | "openai"
//	OLLAMA_URL (default http://localhost:11434), OLLAMA_MODEL (default qwen3:8b)
//	OPENAI_API_KEY (required for openai), OPENAI_MODEL (default gpt-4o-mini),
//	OPENAI_REASONING_EFFORT (gpt-5-family models only; default "high")
func NewLLMClientFromEnv() (LLMClient, error) {
	switch backend := envOrDefault("LLM_BACKEND", "ollama"); backend {
	case "ollama":
		return NewOllamaClient(envOrDefault("OLLAMA_URL", "http://localhost:11434"), envOrDefault("OLLAMA_MODEL", "qwen3:8b")), nil
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("agents: LLM_BACKEND=openai but OPENAI_API_KEY is not set")
		}
		return NewOpenAIClient(key, envOrDefault("OPENAI_MODEL", "gpt-4o-mini"), envOrDefault("OPENAI_REASONING_EFFORT", "high")), nil
	default:
		return nil, fmt.Errorf("agents: unknown LLM_BACKEND %q (want \"ollama\" or \"openai\")", backend)
	}
}

// maxJSONRetries bounds chatJSON's retries on a reply that fails to
// parse. A live run against Ollama's qwen3:8b (this project's default
// backend) found format:"json" occasionally has the model degenerate
// mid-object — the exact same prompt, replayed standalone, produced
// valid JSON on 4/4 tries — so a parse failure is far more likely one
// bad sample than a systematic prompt problem; retrying the whole call
// costs one more request and almost always recovers.
const maxJSONRetries = 2

// chatJSON calls llm.Chat and requires the reply to parse cleanly into
// out, retrying the call (not just re-parsing the same bad reply) up to
// maxJSONRetries times on failure. Every caller in this package (FormSpec,
// DecideNextAction, DraftFinalEmail) follows the same "one Chat call,
// parse the JSON reply" shape, so this is the one place that shape's
// error handling lives, rather than duplicated three times.
func chatJSON(ctx context.Context, llm LLMClient, systemPrompt, userPrompt string, out any) (raw string, err error) {
	for attempt := 0; ; attempt++ {
		raw, err = llm.Chat(ctx, systemPrompt, userPrompt)
		if err != nil {
			return "", err
		}
		if err = json.Unmarshal([]byte(raw), out); err == nil {
			return raw, nil
		}
		if attempt >= maxJSONRetries {
			return raw, fmt.Errorf("parsing LLM reply %q: %w", raw, err)
		}
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

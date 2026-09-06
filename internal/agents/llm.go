package agents

import (
	"context"
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

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

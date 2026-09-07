package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OllamaClient is the local, no-API-key LLMClient backend DESIGN.md's
// "LLM choice" names for dev/simulation — Ollama already running at
// BaseURL with Model pulled (see IMPLEMENTATION_PLAN.md Phase 2).
type OllamaClient struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

func NewOllamaClient(baseURL, model string) *OllamaClient {
	// 300s: a cold model load (nothing keeping qwen3:8b warm yet) blew
	// past 120s on its own before any real inference started, and
	// without format:"json" constraining it (see extractJSON above),
	// qwen3's own free-form thinking pass is uncapped and occasionally
	// ran several minutes on this hardware. Slow, but this project's
	// only backend now (LLM_BACKEND=ollama) has to actually finish
	// rather than time out mid-loop.
	return &OllamaClient{BaseURL: baseURL, Model: model, HTTP: &http.Client{Timeout: 300 * time.Second}}
}

// Chat calls Ollama's /api/chat non-streaming. Deliberately no
// format:"json": that turns on Ollama's grammar-constrained decoding,
// which for this model turned out to have its own bug independent of
// this package's prompts — a specific (and unremarkable-looking) prompt
// reliably produced a reply truncated mid-object, byte-identical however
// many times it was retried or reseeded, yet the exact same prompt
// without format:"json" came back as a clean, complete JSON object every
// time (wrapped in a <think> block qwen3 emits regardless of the "think"
// option, hence extractJSON below). The system prompts already say
// "reply with EXACTLY one JSON object, no prose outside it" — free-form
// generation plus stripping that wrapper is more reliable here than the
// grammar constraint meant to guarantee the same thing.
func (c *OllamaClient) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"stream": false,
		"options": map[string]any{
			// See openai.go's Chat: structured extraction/decision-making,
			// not creative writing — near-zero temperature cuts
			// run-to-run variance on identical input.
			"temperature": 0.1,
			// Ollama defaults num_ctx to 2048 regardless of what the
			// model itself supports (qwen3:8b's real limit is 40960) —
			// this package's longer prompts (decide's full round/spec
			// history especially), now on top of a real thinking pass's
			// own reasoning tokens, comfortably exceed that. 8192 covers
			// this loop's longest prompt with room to spare.
			"num_ctx": 8192,
		},
	})
	if err != nil {
		return "", fmt.Errorf("agents: encoding ollama request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("agents: building ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("agents: calling ollama at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("agents: decoding ollama response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agents: ollama returned %s: %s", resp.Status, out.Error)
	}
	return extractJSON(out.Message.Content), nil
}

// extractJSON strips a qwen3-style <think>...</think> reasoning block (if
// present) and trims to the outermost {...} object, defending against
// stray prose the system prompt told the model not to include but a
// smaller local model doesn't always obey perfectly.
func extractJSON(content string) string {
	if i := strings.LastIndex(content, "</think>"); i != -1 {
		content = content[i+len("</think>"):]
	}
	content = strings.TrimSpace(content)
	if start := strings.IndexByte(content, '{'); start >= 0 {
		if end := strings.LastIndexByte(content, '}'); end > start {
			return content[start : end+1]
		}
	}
	return content
}

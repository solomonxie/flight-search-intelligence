package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	return &OllamaClient{BaseURL: baseURL, Model: model, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Chat calls Ollama's /api/chat non-streaming, with format:"json" so the
// whole reply is one JSON value — DecideNextAction/FormSpec parse it
// further into their own shape.
func (c *OllamaClient) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"stream": false,
		"format": "json",
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
	return out.Message.Content, nil
}

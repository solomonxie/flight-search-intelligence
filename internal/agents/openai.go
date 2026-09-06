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

// OpenAIClient is the prod LLMClient backend — DESIGN.md "LLM choice".
// Written against the Chat Completions API's JSON-object response mode;
// DecideNextAction/FormSpec exercise the same LLMClient interface either
// way, so swapping backends needs no change above this file.
type OpenAIClient struct {
	APIKey          string
	Model           string
	ReasoningEffort string // "minimal"|"low"|"medium"|"high" — gpt-5-family models only, see Chat
	HTTP            *http.Client
}

func NewOpenAIClient(apiKey, model, reasoningEffort string) *OpenAIClient {
	return &OpenAIClient{APIKey: apiKey, Model: model, ReasoningEffort: reasoningEffort, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

func (c *OpenAIClient) Chat(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	payload := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	if strings.HasPrefix(c.Model, "gpt-5") {
		// gpt-5-family reasoning models reject a non-default temperature
		// (must be left at 1) — reasoning_effort is their equivalent
		// knob for "think harder, less randomly."
		payload["reasoning_effort"] = c.ReasoningEffort
	} else {
		// This is structured extraction/decision-making, not creative
		// writing — near-zero temperature cuts run-to-run variance on
		// identical input (the default ~1.0 was producing visibly
		// different Spec/Decision output across repeat runs of the
		// exact same conversation).
		payload["temperature"] = 0.1
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("agents: encoding openai request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("agents: building openai request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("agents: calling openai: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("agents: decoding openai response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agents: openai returned %s: %s", resp.Status, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("agents: openai returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

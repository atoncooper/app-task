package service

// LLM gateway shared by all AI features (digest summaries, failure RCA, Lua
// script generation). OpenAI-compatible chat completions only — every major
// provider (DeepSeek/Qwen/Moonshot/Ollama/OneAPI) speaks it, so switching
// models is configuration, not code. The api key is masked on every log and
// error path.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"app-task/internal/config"
)

// LLMClient is the shared chat-completions client. Nil-safe: a nil client or
// an unconfigured config means AI is disabled (Enabled() false) and callers
// take their non-AI fallback paths.
type LLMClient struct {
	cfg    *config.AIConfig
	http   *http.Client
	masked string // the key as it appears in logs: "***" when set
}

// NewLLMClient builds the gateway; nil when AI is not configured.
func NewLLMClient(cfg *config.AIConfig) *LLMClient {
	if cfg == nil || cfg.Model == "" || cfg.APIKey == "" {
		return nil
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &LLMClient{cfg: cfg, http: &http.Client{Timeout: timeout}, masked: "***"}
}

// Enabled reports whether AI features may run.
func (c *LLMClient) Enabled() bool { return c != nil }

// Chat sends one system+user turn and returns the assistant text.
func (c *LLMClient) Chat(ctx context.Context, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("AI disabled (ai.model / ai.api_key not configured)")
	}
	body, err := json.Marshal(map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"max_tokens":  orDefault(c.cfg.MaxTokens, 1024),
		"temperature": 0.3,
	})
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("llm reply decode: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("llm reply empty")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

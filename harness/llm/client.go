// Package llm handles outbound calls to OpenAI-compatible providers (Atria, etc.).
// It owns authentication, cancellation, and provider error normalization.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type ProviderConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	MaxRetries int
	Timeout    time.Duration
}

func DefaultConfig() ProviderConfig {
	return FromEnv()
}

// FromEnv reads provider config from environment.
func FromEnv() ProviderConfig {
	api := os.Getenv("SOORAA_API_URL")
	if api == "" {
		api = "https://api.atria-asi.ai/v1"
	}
	key := os.Getenv("SOORAA_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	model := os.Getenv("SOORAA_MODEL")
	if model == "" {
		model = "atria-dawn-preview"
	}
	return ProviderConfig{
		BaseURL:    api,
		APIKey:     key,
		Model:      model,
		MaxRetries: 3,
		Timeout:    120 * time.Second,
	}
}

// FromProvider builds ProviderConfig from individual settings.
func FromProvider(baseURL, apiKey, model string) ProviderConfig {
	return ProviderConfig{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Model:      model,
		MaxRetries: 3,
		Timeout:    120 * time.Second,
	}
}

type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type StreamedToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type ProviderResponse struct {
	Content   string
	Reasoning string
	ToolCalls []StreamedToolCall
	Done      bool
	Usage     *UsageInfo
}

type UsageInfo struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type StreamRequest struct {
	Messages []Item
	Tools    []ToolDef
	Reason   ReasonLevel
}

type StreamingClient struct {
	cfg  ProviderConfig
	http *http.Client
}

func New(cfg ProviderConfig) *StreamingClient {
	return &StreamingClient{
		cfg: cfg,
		http: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

func (c *StreamingClient) Stream(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	api := c.cfg.BaseURL
	if !strings.HasSuffix(api, "/") {
		api += "/"
	}
	url := api + "chat/completions"

	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, item := range req.Messages {
		switch item.Kind {
		case "text":
			msg := map[string]any{"role": string(item.Role)}
			if item.Role == RoleTool {
				msg["tool_call_id"] = item.CallID
				msg["content"] = item.Output
			} else {
				msg["content"] = item.Text
			}
			msgs = append(msgs, msg)
		case "tool_call":
			msgs = append(msgs, map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{
					{"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": item.Args}},
				},
			})
		}
	}

	body := map[string]any{
		"model":          c.cfg.Model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.Reason != "" {
		body["reasoning_effort"] = string(req.Reason)
	}
	if req.Tools != nil && len(req.Tools) > 0 {
		toolMaps := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			toolMaps[i] = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			}
		}
		body["tools"] = toolMaps
		body["tool_choice"] = "auto"
	}

	b, _ := json.Marshal(body)
	hreq, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		resp, err := c.http.Do(hreq)
		if err != nil {
			lastErr = fmt.Errorf("request: %w", err)
			if attempt < c.cfg.MaxRetries {
				time.Sleep(backoff(attempt))
				continue
			}
			break
		}
		if resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			errMsg := fmt.Sprintf("provider error %d: %s", resp.StatusCode, string(raw))
			lastErr = fmt.Errorf("%s", errMsg)
			if isTransient(resp.StatusCode) && attempt < c.cfg.MaxRetries {
				time.Sleep(backoff(attempt))
				continue
			}
			break
		}
		return c.readStream(resp.Body, onDelta)
	}
	return fmt.Errorf("after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

type accumulatedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func (c *StreamingClient) readStream(r io.Reader, onDelta func(ProviderResponse)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	acc := map[int]*accumulatedToolCall{}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			onDelta(ProviderResponse{Done: true})
			return nil
		}

		var ev struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content    *string `json:"content"`
					Reasoning  *string `json:"reasoning_content"`
					Reasoning2 *string `json:"reasoning"`
					ToolCalls  []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *UsageInfo `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}

		var delta ProviderResponse
		if len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != nil && *ev.Choices[0].Delta.Content != "" {
			delta.Content = *ev.Choices[0].Delta.Content
		}
		if len(ev.Choices) > 0 {
			if ev.Choices[0].Delta.Reasoning != nil {
				delta.Reasoning = *ev.Choices[0].Delta.Reasoning
			} else if ev.Choices[0].Delta.Reasoning2 != nil {
				delta.Reasoning = *ev.Choices[0].Delta.Reasoning2
			}
			for _, tc := range ev.Choices[0].Delta.ToolCalls {
				existing, ok := acc[tc.Index]
				if !ok {
					existing = &accumulatedToolCall{}
					acc[tc.Index] = existing
				}
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Function.Name != "" {
					existing.Name = tc.Function.Name
				}
				existing.Arguments += tc.Function.Arguments
				delta.ToolCalls = append(delta.ToolCalls, StreamedToolCall{
					Index:     tc.Index,
					ID:        existing.ID,
					Name:      existing.Name,
					Arguments: existing.Arguments,
				})
			}
		}
		if ev.Usage != nil {
			delta.Usage = ev.Usage
		}
		onDelta(delta)
	}
	return scanner.Err()
}

// isTransient returns true for status codes that should be retried.
func isTransient(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// backoff returns exponential backoff duration for the given attempt (0-based).
func backoff(attempt int) time.Duration {
	// 1s, 3s, 8s schedule
	durations := []time.Duration{1 * time.Second, 3 * time.Second, 8 * time.Second}
	if attempt < len(durations) {
		return durations[attempt]
	}
	return 8 * time.Second
}

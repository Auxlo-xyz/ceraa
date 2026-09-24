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
	key := os.Getenv("SOORAA_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	model := os.Getenv("SOORAA_MODEL")
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
	InputTokens       int `json:"input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int `json:"reasoning_tokens,omitempty"`
	TotalTokens       int `json:"total_tokens,omitempty"`
}

type TokenBreakdown struct {
	System       int `json:"system,omitempty"`
	Tools        int `json:"tools,omitempty"`
	User         int `json:"user,omitempty"`
	Conversation int `json:"conversation,omitempty"`
	ToolOutput   int `json:"tool_output,omitempty"`
	Reasoning    int `json:"reasoning,omitempty"`
	Compaction   int `json:"compaction,omitempty"`
	Other        int `json:"other,omitempty"`
}

func (u UsageInfo) normalized() UsageInfo {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
		return u
	}
	if u.InputTokens == 0 {
		u.InputTokens = u.TotalTokens - u.OutputTokens
		if u.InputTokens < 0 {
			u.InputTokens = 0
		}
	}
	if u.OutputTokens == 0 {
		u.OutputTokens = u.TotalTokens - u.InputTokens
		if u.OutputTokens < 0 {
			u.OutputTokens = 0
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

func (u UsageInfo) UncachedInputTokens() int {
	value := u.normalized().InputTokens - u.CachedInputTokens
	if value < 0 {
		return 0
	}
	return value
}

func (u *UsageInfo) Add(delta UsageInfo) {
	delta = delta.normalized()
	u.InputTokens += delta.InputTokens
	u.OutputTokens += delta.OutputTokens
	u.CachedInputTokens += delta.CachedInputTokens
	u.CacheWriteTokens += delta.CacheWriteTokens
	u.ReasoningTokens += delta.ReasoningTokens
	u.TotalTokens += delta.TotalTokens
}

func (u *UsageInfo) UnmarshalJSON(data []byte) error {
	var raw struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		PromptTokens             int `json:"prompt_tokens"`
		CompletionTokens         int `json:"completion_tokens"`
		TotalTokens              int `json:"total_tokens"`
		CachedInputTokens        int `json:"cached_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheWriteTokens         int `json:"cache_write_tokens"`
		ReasoningTokens          int `json:"reasoning_tokens"`
		PromptTokensDetails      struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	u.InputTokens = raw.InputTokens
	if u.InputTokens == 0 {
		u.InputTokens = raw.PromptTokens
	}
	u.OutputTokens = raw.OutputTokens
	if u.OutputTokens == 0 {
		u.OutputTokens = raw.CompletionTokens
	}
	u.TotalTokens = raw.TotalTokens
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	u.CachedInputTokens = raw.PromptTokensDetails.CachedTokens
	if u.CachedInputTokens == 0 {
		u.CachedInputTokens = raw.InputTokensDetails.CachedTokens
	}
	if u.CachedInputTokens == 0 {
		u.CachedInputTokens = raw.CachedInputTokens
	}
	if u.CachedInputTokens == 0 {
		u.CachedInputTokens = raw.CacheReadInputTokens
	}
	u.CacheWriteTokens = raw.CacheWriteTokens
	if u.CacheWriteTokens == 0 {
		u.CacheWriteTokens = raw.CacheCreationInputTokens
	}
	u.ReasoningTokens = raw.ReasoningTokens
	if u.ReasoningTokens == 0 {
		u.ReasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}
	return nil
}

func (b TokenBreakdown) Total() int {
	return b.System + b.Tools + b.User + b.Conversation + b.ToolOutput + b.Reasoning + b.Compaction + b.Other
}

func (b *TokenBreakdown) Add(other TokenBreakdown) {
	b.System += other.System
	b.Tools += other.Tools
	b.User += other.User
	b.Conversation += other.Conversation
	b.ToolOutput += other.ToolOutput
	b.Reasoning += other.Reasoning
	b.Compaction += other.Compaction
	b.Other += other.Other
}

func EstimateToolDefinitionTokens(tools []ToolDef) int {
	total := 0
	for _, item := range tools {
		data, _ := json.Marshal(item)
		total += estimateTokens(string(data))
	}
	return total
}

func EstimateTokenBreakdown(messages []Item, tools []ToolDef) TokenBreakdown {
	var breakdown TokenBreakdown
	breakdown.Tools = EstimateToolDefinitionTokens(tools)
	for _, item := range messages {
		tokens := estimateTokens(item.Text) + estimateTokens(item.Args) + estimateTokens(item.Output) + estimateTokens(item.Reason)
		tokens += estimateTokens(item.CallID) + estimateTokens(item.Name)
		for _, call := range item.ToolCalls {
			tokens += estimateTokens(call.ID) + estimateTokens(call.Name) + estimateTokens(call.Args)
		}
		tokens += 10
		switch {
		case strings.Contains(item.Text, "<conversation-checkpoint>"):
			breakdown.Compaction += tokens
		case item.Role == RoleSystem:
			breakdown.System += tokens
		case item.Kind == "reasoning" || item.Reason != "":
			breakdown.Reasoning += tokens
		case item.Role == RoleTool:
			breakdown.ToolOutput += tokens
		case item.Role == RoleUser:
			breakdown.User += tokens
		case item.Kind == "tool_call" || item.Kind == "tool_calls" || item.Role == RoleAssistant:
			breakdown.Conversation += tokens
		default:
			breakdown.Other += tokens
		}
	}
	return breakdown
}

func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / 4
}

type StreamRequest struct {
	Messages  []Item
	Tools     []ToolDef
	Reason    ReasonLevel
	MaxTokens int
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
			assistant := map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{
					{"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": item.Args}},
				},
			}
			if item.Text != "" {
				assistant["content"] = item.Text
			}
			msgs = append(msgs, assistant)
		case "tool_calls":
			calls := make([]map[string]any, 0, len(item.ToolCalls))
			for _, call := range item.ToolCalls {
				calls = append(calls, map[string]any{
					"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Args},
				})
			}
			assistant := map[string]any{"role": "assistant", "tool_calls": calls}
			if item.Text != "" {
				assistant["content"] = item.Text
			}
			msgs = append(msgs, assistant)
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
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
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

	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		hreq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
		if err != nil {
			return err
		}
		hreq.Header.Set("Content-Type", "application/json")
		if c.cfg.APIKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
		resp, err := c.http.Do(hreq)
		if err != nil {
			lastErr = fmt.Errorf("request: %w", err)
			if attempt < c.cfg.MaxRetries {
				if !sleepContext(ctx, backoff(attempt)) {
					break
				}
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
				if !sleepContext(ctx, backoff(attempt)) {
					break
				}
				continue
			}
			break
		}
		defer resp.Body.Close()
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

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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

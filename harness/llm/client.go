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
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type ProviderConfig struct {
	BaseURL                 string
	APIKey                  string
	Model                   string
	GeminiThoughtSignatures bool
	MaxRetries              int
	Timeout                 time.Duration
	ToolChoice              string
	PromptCacheMode         string
	PromptCacheTTL          string
	Compatibility           *Compatibility
	// ExtraBody is merged into every chat-completions request body. Used by
	// providers that require extra top-level fields, such as Freebuff's
	// codebuff_metadata. Set by the caller; never mutated here.
	ExtraBody map[string]any
	// ExtraBodyFunc is evaluated per request, for metadata that must carry a
	// fresh correlation id on every call. Its result is merged after ExtraBody
	// and is likewise never allowed to shadow a field Sooraa owns.
	ExtraBodyFunc func() map[string]any
	// ExtraHeaders are added to every request. Used for provider-required
	// client identification.
	ExtraHeaders map[string]string
	// RecoverError is consulted when the provider returns an error that a plain
	// retry cannot fix. It returns whether the request may be retried once
	// after the hook repaired provider state, plus a user-facing message.
	//
	// Typical use: a hosted provider gates requests behind a session, so a gate
	// rejection must re-admit the session before the request can succeed. The
	// hook must be fast and idempotent, and retries are bounded by
	// MaxRecoveries so a broken hook cannot spin.
	RecoverError func(ctx context.Context, status int, body []byte) (retry bool, message string)
	// HTTPClient overrides the transport. Used for a custom TLS trust store or
	// proxy; Timeout still applies when unset.
	HTTPClient *http.Client
}

// MaxRecoveries bounds RecoverError-driven retries per request. Two is enough
// for a session re-admission: one to detect the gate, one to confirm the retry
// actually worked before giving up.
const MaxRecoveries = 2

type Compatibility struct {
	MaxTokensField         string
	SupportsUsageStreaming bool
	SupportsFinishReason   bool
	SupportsDeveloperRole  bool
	RequiresToolResultName bool
	StrictTools            bool
	ReasoningField         string
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

func defaultCompatibility() Compatibility {
	return Compatibility{
		MaxTokensField:         "max_tokens",
		SupportsUsageStreaming: true,
		SupportsFinishReason:   false,
		RequiresToolResultName: true,
		ReasoningField:         "reasoning_content",
	}
}

func compatibilityFor(cfg ProviderConfig) Compatibility {
	compatibility := defaultCompatibility()
	base := strings.ToLower(cfg.BaseURL)
	model := strings.ToLower(cfg.Model)
	if strings.Contains(base, "openai.com") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
		compatibility.MaxTokensField = "max_completion_tokens"
		compatibility.SupportsDeveloperRole = true
		compatibility.SupportsFinishReason = true
	}
	if strings.Contains(base, "openrouter.ai") {
		compatibility.MaxTokensField = "max_tokens"
		compatibility.SupportsDeveloperRole = false
	}
	if cfg.Compatibility != nil {
		provided := *cfg.Compatibility
		if provided.MaxTokensField != "" {
			compatibility.MaxTokensField = provided.MaxTokensField
		}
		if provided.ReasoningField != "" {
			compatibility.ReasoningField = provided.ReasoningField
		}
		compatibility.SupportsUsageStreaming = provided.SupportsUsageStreaming
		compatibility.SupportsFinishReason = provided.SupportsFinishReason
		compatibility.SupportsDeveloperRole = provided.SupportsDeveloperRole
		compatibility.RequiresToolResultName = provided.RequiresToolResultName
		compatibility.StrictTools = provided.StrictTools
	}
	return compatibility
}

type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type StreamedToolCall struct {
	Index            int
	ID               string
	Name             string
	Arguments        string
	ThoughtSignature string
}

type ProviderResponse struct {
	Content          string
	Reasoning        string
	ToolCalls        []StreamedToolCall
	Done             bool
	FinishReason     string
	ResponseID       string
	ResponseModel    string
	ReasoningDetails json.RawMessage
	Usage            *UsageInfo
}

type UsageInfo struct {
	InputTokens       int `json:"input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int `json:"reasoning_tokens,omitempty"`
	TotalTokens       int `json:"total_tokens,omitempty"`
}

type ProviderError struct {
	Status     int
	Body       string
	Retryable  bool
	RetryAfter time.Duration
	// Recovered is set when RecoverError repaired provider state and the
	// request was re-issued.
	Recovered bool
	// RecoverMessage is a user-facing explanation produced by RecoverError.
	RecoverMessage string
}

type StreamError struct {
	Err       error
	Retryable bool
	Partial   bool
}

func (e *StreamError) Error() string { return e.Err.Error() }
func (e *StreamError) Unwrap() error { return e.Err }

func (e *ProviderError) Error() string {
	detail := e.Body
	if e.Status == 0 {
		detail = "provider request failed: " + e.Body
	} else {
		detail = fmt.Sprintf("provider error %d: %s", e.Status, e.Body)
	}
	// A provider-supplied explanation is far more useful to the user than a raw
	// status line, so it leads and the raw detail follows for diagnosis.
	if e.RecoverMessage != "" {
		return e.RecoverMessage + " (" + detail + ")"
	}
	return detail
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

func (u UsageInfo) CacheReadRatio() float64 {
	if u.InputTokens <= 0 || u.CachedInputTokens <= 0 {
		return 0
	}
	ratio := float64(u.CachedInputTokens) / float64(u.InputTokens)
	if ratio > 1 {
		return 1
	}
	return ratio
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
		tokens := estimateTokens(item.Text) + estimateTokens(item.Args) + estimateTokens(item.Output) + estimateTokens(item.Reason) + estimateTokens(string(item.ReasoningDetails))
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
	Messages       []Item
	Tools          []ToolDef
	Reason         ReasonLevel
	MaxTokens      int
	ToolChoice     string
	PromptCacheKey string
	SessionID      string
}

type StreamingClient struct {
	cfg  ProviderConfig
	http *http.Client
}

func New(cfg ProviderConfig) *StreamingClient {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}
	return &StreamingClient{cfg: cfg, http: httpClient}
}

func geminiExtraContent(signature string, enabled bool) map[string]any {
	if !enabled || signature == "" {
		return nil
	}
	return map[string]any{"google": map[string]any{"thought_signature": signature}}
}

func encodeMessages(items []Item, geminiThoughtSignatures bool) []map[string]any {
	return encodeMessagesWithCompatibility(items, geminiThoughtSignatures, defaultCompatibility())
}

func encodeMessagesWithCompatibility(items []Item, geminiThoughtSignatures bool, compatibility Compatibility) []map[string]any {
	items = normalizeItemsForChat(items)
	msgs := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Kind {
		case "text":
			role := string(item.Role)
			if item.Role == RoleSystem && compatibility.SupportsDeveloperRole {
				role = "developer"
			}
			if item.Role == RoleAssistant && strings.TrimSpace(item.Text) == "" {
				continue
			}
			msg := map[string]any{"role": role}
			if item.Role == RoleTool {
				msg["tool_call_id"] = item.CallID
				output := item.Output
				if strings.TrimSpace(output) == "" {
					output = "No output was produced."
				}
				msg["content"] = output
				if item.Name != "" && compatibility.RequiresToolResultName {
					msg["name"] = item.Name
				}
			} else {
				msg["content"] = item.Text
				if item.Role == RoleAssistant {
					addReasoningFields(msg, item, compatibility)
				}
			}
			msgs = append(msgs, msg)
		case "reasoning":
			if item.Reason != "" || len(item.ReasoningDetails) > 0 {
				message := map[string]any{"role": "assistant"}
				if item.Reason != "" {
					message[compatibility.ReasoningField] = item.Reason
				}
				if len(item.ReasoningDetails) > 0 {
					message["reasoning_details"] = item.ReasoningDetails
				}
				msgs = append(msgs, message)
			}
		case "tool_call":
			args := item.Args
			if repaired, changed := RepairToolArgumentsJSON(args); changed {
				args = repaired
			}
			call := map[string]any{
				"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": args},
			}
			if extra := geminiExtraContent(item.ThoughtSignature, geminiThoughtSignatures); extra != nil {
				call["extra_content"] = extra
			}
			assistant := map[string]any{"role": "assistant", "tool_calls": []map[string]any{call}}
			if item.Text != "" {
				assistant["content"] = item.Text
			}
			addReasoningFields(assistant, item, compatibility)
			msgs = append(msgs, assistant)
		case "tool_calls":
			calls := make([]map[string]any, 0, len(item.ToolCalls))
			for _, call := range item.ToolCalls {
				args := call.Args
				if repaired, changed := RepairToolArgumentsJSON(args); changed {
					args = repaired
				}
				encoded := map[string]any{
					"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": args},
				}
				if extra := geminiExtraContent(call.ThoughtSignature, geminiThoughtSignatures); extra != nil {
					encoded["extra_content"] = extra
				}
				calls = append(calls, encoded)
			}
			assistant := map[string]any{"role": "assistant", "tool_calls": calls}
			if item.Text != "" {
				assistant["content"] = item.Text
			}
			addReasoningFields(assistant, item, compatibility)
			msgs = append(msgs, assistant)
		}
	}
	return msgs
}

func addReasoningFields(message map[string]any, item Item, compatibility Compatibility) {
	if item.Reason != "" {
		message[compatibility.ReasoningField] = item.Reason
	}
	if len(item.ReasoningDetails) > 0 {
		message["reasoning_details"] = item.ReasoningDetails
	}
}

func normalizeItemsForChat(items []Item) []Item {
	results := make(map[string]struct{})
	for _, item := range items {
		if item.Kind == "text" && item.Role == RoleTool && item.CallID != "" {
			results[item.CallID] = struct{}{}
		}
	}
	normalized := make([]Item, 0, len(items))
	for _, item := range items {
		normalized = append(normalized, item)
		var calls []ToolCall
		switch item.Kind {
		case "tool_call":
			if item.CallID != "" {
				calls = []ToolCall{{ID: item.CallID, Name: item.Name}}
			}
		case "tool_calls":
			calls = item.ToolCalls
		}
		for _, call := range calls {
			if call.ID == "" {
				continue
			}
			if _, ok := results[call.ID]; ok {
				continue
			}
			results[call.ID] = struct{}{}
			normalized = append(normalized, Item{Kind: "text", Role: RoleTool, CallID: call.ID, Name: call.Name, Output: "Error: tool result was not recorded", Text: "Error: tool result was not recorded"})
		}
	}
	return normalized
}

func isOpenRouterEndpoint(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
}

func isPromptCacheKeyEndpoint(baseURL string) bool {
	base := strings.ToLower(baseURL)
	return strings.Contains(base, "openai.com") || isOpenRouterEndpoint(base)
}

func promptCacheMode(cfg ProviderConfig) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.PromptCacheMode))
	if mode == "" {
		return "auto"
	}
	return mode
}

func addPromptCacheFields(body map[string]any, cfg ProviderConfig, req StreamRequest) string {
	mode := promptCacheMode(cfg)
	if mode == "off" || req.PromptCacheKey == "" {
		return ""
	}
	if isPromptCacheKeyEndpoint(cfg.BaseURL) || mode == "explicit" {
		body["prompt_cache_key"] = req.PromptCacheKey
	}
	if !isOpenRouterEndpoint(cfg.BaseURL) {
		return ""
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = req.PromptCacheKey
	}
	body["session_id"] = sessionID
	if mode != "explicit" {
		return sessionID
	}
	cacheControl := map[string]any{"type": "ephemeral"}
	if ttl := strings.TrimSpace(cfg.PromptCacheTTL); ttl != "" {
		cacheControl["ttl"] = ttl
	}
	body["cache_control"] = cacheControl
	return sessionID
}

func (c *StreamingClient) Stream(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	compatibility := compatibilityFor(c.cfg)
	url := chatCompletionsURL(c.cfg.BaseURL)
	msgs := encodeMessagesWithCompatibility(req.Messages, c.cfg.GeminiThoughtSignatures, compatibility)

	body := map[string]any{
		"model":    c.cfg.Model,
		"messages": msgs,
		"stream":   true,
	}
	if compatibility.SupportsUsageStreaming {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Reason != "" {
		body["reasoning_effort"] = string(req.Reason)
	}
	if req.MaxTokens > 0 {
		body[compatibility.MaxTokensField] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		toolMaps := make([]map[string]any, len(req.Tools))
		for i, tool := range req.Tools {
			function := map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			}
			if compatibility.StrictTools {
				function["strict"] = true
			}
			toolMaps[i] = map[string]any{"type": "function", "function": function}
		}
		body["tools"] = toolMaps
		body["tool_choice"] = req.ToolChoice
		if body["tool_choice"] == "" {
			body["tool_choice"] = c.cfg.ToolChoice
		}
		if body["tool_choice"] == "" {
			body["tool_choice"] = "auto"
		}
	}
	sessionID := addPromptCacheFields(body, c.cfg, req)
	extras := c.cfg.ExtraBody
	if c.cfg.ExtraBodyFunc != nil {
		// Evaluated per request so correlation ids are never reused.
		extras = c.cfg.ExtraBodyFunc()
	}
	for key, value := range extras {
		if _, reserved := body[key]; reserved {
			// Never let a provider extra shadow a field Sooraa owns.
			continue
		}
		body[key] = value
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode provider request: %w", err)
	}

	var lastErr error
	recoveries := 0
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		hreq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		if sessionID != "" {
			hreq.Header.Set("x-session-id", sessionID)
		}
		for key, value := range c.cfg.ExtraHeaders {
			hreq.Header.Set(key, value)
		}
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
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				raw = []byte(readErr.Error())
			}
			providerErr := &ProviderError{Status: resp.StatusCode, Body: string(raw), Retryable: shouldRetry(resp), RetryAfter: retryAfter(resp.Header)}
			lastErr = providerErr
			// A gate rejection (for example a hosted provider refusing a request
			// with no live session) cannot be fixed by repeating the same call,
			// but it can be fixed by repairing provider state and asking once
			// more. Recovery is attempted before plain retries so a session can
			// be re-admitted instead of burning the retry budget on a request
			// that is certain to fail the same way.
			if c.cfg.RecoverError != nil && recoveries < MaxRecoveries {
				if retry, message := c.cfg.RecoverError(ctx, resp.StatusCode, raw); retry {
					recoveries++
					if message != "" {
						providerErr.RecoverMessage = message
					}
					providerErr.Recovered = true
					attempt-- // a recovery is not a failed attempt
					continue
				} else if message != "" {
					providerErr.RecoverMessage = message
				}
			}
			if providerErr.Retryable && attempt < c.cfg.MaxRetries {
				delay := providerErr.RetryAfter
				if delay <= 0 {
					delay = backoff(attempt)
				}
				if !sleepContext(ctx, delay) {
					break
				}
				continue
			}
			break
		}
		err = c.readStreamWithCompatibility(resp.Body, onDelta, compatibility)
		_ = resp.Body.Close()
		if streamErr, ok := err.(*StreamError); ok && streamErr.Retryable && !streamErr.Partial && attempt < c.cfg.MaxRetries {
			if !sleepContext(ctx, backoff(attempt)) {
				break
			}
			continue
		}
		return err
	}
	if providerErr, ok := lastErr.(*ProviderError); ok {
		return providerErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("provider request failed")
	}
	return fmt.Errorf("after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

type accumulatedToolCall struct {
	ID               string
	Name             string
	Arguments        string
	ThoughtSignature string
}

func (c *StreamingClient) readStream(r io.Reader, onDelta func(ProviderResponse)) error {
	return c.readStreamWithCompatibility(r, onDelta, Compatibility{SupportsFinishReason: false})
}

func (c *StreamingClient) readStreamWithCompatibility(r io.Reader, onDelta func(ProviderResponse), compatibility Compatibility) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	acc := map[string]*accumulatedToolCall{}
	var finishReason, responseID, responseModel string
	terminal := false
	emitted := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			terminal = true
			break
		}
		var event struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content         *string         `json:"content"`
					Reasoning       *string         `json:"reasoning_content"`
					ReasoningAlt    *string         `json:"reasoning"`
					ReasoningText   *string         `json:"reasoning_text"`
					ReasoningDetail json.RawMessage `json:"reasoning_details"`
					ToolCalls       []struct {
						Index        *int   `json:"index"`
						ID           string `json:"id"`
						Type         string `json:"type"`
						ExtraContent struct {
							Google struct {
								ThoughtSignature string `json:"thought_signature"`
							} `json:"google"`
						} `json:"extra_content"`
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
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return &StreamError{Err: fmt.Errorf("decode provider stream event: %w", err), Retryable: false, Partial: emitted}
		}
		if event.ID != "" {
			responseID = event.ID
		}
		if event.Model != "" {
			responseModel = event.Model
		}
		var delta ProviderResponse
		delta.ResponseID = responseID
		delta.ResponseModel = responseModel
		if len(event.Choices) > 0 {
			choice := event.Choices[0]
			if choice.Delta.Content != nil {
				delta.Content = *choice.Delta.Content
			}
			if len(choice.Delta.ReasoningDetail) > 0 && string(choice.Delta.ReasoningDetail) != "null" {
				delta.ReasoningDetails = choice.Delta.ReasoningDetail
			}
			for _, candidate := range []*string{choice.Delta.Reasoning, choice.Delta.ReasoningAlt, choice.Delta.ReasoningText} {
				if candidate != nil && *candidate != "" {
					delta.Reasoning = *candidate
					break
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
				delta.FinishReason = finishReason
			}
			for _, toolCall := range choice.Delta.ToolCalls {
				key := streamedToolKey(toolCall.Index, toolCall.ID, len(acc))
				if toolCall.Index == nil && toolCall.ID != "" {
					if _, exists := acc[key]; !exists {
						for existingKey, existing := range acc {
							if existing.ID == "" {
								key = existingKey
								break
							}
						}
					}
				}
				existing := acc[key]
				if existing == nil {
					existing = &accumulatedToolCall{}
					acc[key] = existing
				}
				if toolCall.ID != "" {
					existing.ID = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					existing.Name = toolCall.Function.Name
				}
				if toolCall.ExtraContent.Google.ThoughtSignature != "" {
					existing.ThoughtSignature = toolCall.ExtraContent.Google.ThoughtSignature
				}
				existing.Arguments = mergeStreamedArguments(existing.Arguments, toolCall.Function.Arguments)
				index := len(acc) - 1
				if toolCall.Index != nil {
					index = *toolCall.Index
				}
				delta.ToolCalls = append(delta.ToolCalls, StreamedToolCall{
					Index:            index,
					ID:               existing.ID,
					Name:             existing.Name,
					Arguments:        existing.Arguments,
					ThoughtSignature: existing.ThoughtSignature,
				})
			}
		}
		if event.Usage != nil {
			delta.Usage = event.Usage
		}
		if delta.Content != "" || delta.Reasoning != "" || len(delta.ToolCalls) > 0 || delta.Usage != nil || delta.FinishReason != "" {
			emitted = true
		}
		onDelta(delta)
	}
	if err := scanner.Err(); err != nil {
		return &StreamError{Err: fmt.Errorf("read provider stream: %w", err), Retryable: true, Partial: emitted}
	}
	if !terminal {
		if finishReason == "" {
			return &StreamError{Err: fmt.Errorf("provider stream ended without a terminal event"), Retryable: true, Partial: emitted}
		}
		terminal = true
	}
	if compatibility.SupportsFinishReason && finishReason == "" {
		return &StreamError{Err: fmt.Errorf("provider stream ended without finish_reason"), Retryable: true, Partial: emitted}
	}
	onDelta(ProviderResponse{Done: true, FinishReason: finishReason, ResponseID: responseID, ResponseModel: responseModel})
	return nil
}

func streamedToolKey(index *int, id string, fallback int) string {
	if index != nil {
		return "index:" + strconv.Itoa(*index)
	}
	if id != "" {
		return "id:" + id
	}
	return "fallback:" + strconv.Itoa(fallback)
}

func mergeStreamedArguments(current, incoming string) string {
	if incoming == "" {
		return current
	}
	if current == "" || strings.HasPrefix(incoming, current) {
		return incoming
	}
	return current + incoming
}

// isTransient returns true for status codes that should be retried.
func chatCompletionsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

func retryAfter(header http.Header) time.Duration {
	for _, name := range []string{"Retry-After-Ms", "Retry-After"} {
		value := strings.TrimSpace(header.Get(name))
		if value == "" {
			continue
		}
		if milliseconds, err := strconv.ParseFloat(value, 64); err == nil && strings.EqualFold(name, "Retry-After-Ms") {
			if milliseconds > 0 {
				return time.Duration(milliseconds * float64(time.Millisecond))
			}
		}
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		if when, err := http.ParseTime(value); err == nil {
			if delay := time.Until(when); delay > 0 {
				return delay
			}
		}
	}
	return 0
}

func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	if providerErr, ok := err.(*ProviderError); ok {
		body := strings.ToLower(providerErr.Body)
		return strings.Contains(body, "context_length") || strings.Contains(body, "context window") || strings.Contains(body, "maximum context") || strings.Contains(body, "too many tokens")
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context_length") || strings.Contains(message, "context window") || strings.Contains(message, "maximum context")
}

func shouldRetry(response *http.Response) bool {
	if value := strings.TrimSpace(response.Header.Get("X-Should-Retry")); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return isTransient(response.StatusCode)
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
	durations := []time.Duration{1 * time.Second, 3 * time.Second, 8 * time.Second}
	delay := 8 * time.Second
	if attempt < len(durations) {
		delay = durations[attempt]
	}
	jitter := time.Duration(rand.Int63n(int64(250 * time.Millisecond)))
	return delay + jitter
}

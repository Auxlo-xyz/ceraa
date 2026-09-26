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
)

// CodexBaseURL is the Codex subscription endpoint. It is not the public OpenAI
// API: it is served by ChatGPT and only accepts a ChatGPT OAuth credential.
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// CodexTokenSource supplies a currently valid Codex access token. The auth
// package implements it, which keeps this transport free of any dependency on
// how a credential is stored or refreshed.
type CodexTokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// CodexRefresher is an optional capability of a CodexTokenSource. When a request
// is rejected as unauthorised, a fresh exchange is forced and the request is
// retried once, which recovers a token revoked server-side before its recorded
// expiry.
type CodexRefresher interface {
	RefreshToken(ctx context.Context) (string, error)
}

// codexResponsesURL resolves the responses path against a configured base.
func codexResponsesURL(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(trimmed, "/responses") {
		return trimmed
	}
	return trimmed + "/responses"
}

// CodexSubscription identifies a provider profile that runs on a ChatGPT
// subscription rather than an API key.
const CodexSubscription = "codex"

// CodexModels are the model IDs observed to work on the Codex subscription
// endpoint. The endpoint rejects anything else, and the rejection is a hard 400,
// so an unknown ID has to be refused up front rather than sent and retried.
//
// This list was established by probing the live endpoint, not copied from
// another client: several IDs that client still ships are retired (404) or
// unsupported on ChatGPT accounts (400).
var CodexModels = []string{
	"gpt-5.6-luna",
	"gpt-5.6-terra",
}

// DefaultCodexModel is the model selected when Codex is chosen without one.
const DefaultCodexModel = "gpt-5.6-luna"

// IsCodexModel reports whether a model ID is known to work on the endpoint.
func IsCodexModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range CodexModels {
		if trimmed == known {
			return true
		}
	}
	return false
}

// UsesCodexNative reports whether a configured base URL is the Codex
// subscription endpoint, which needs its own transport rather than the
// OpenAI-compatible chat-completions adapter.
func UsesCodexNative(baseURL string) bool {
	base := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if base == "" {
		return false
	}
	return strings.Contains(base, "chatgpt.com/backend-api/codex")
}

// codexInput converts stored items into a Responses API input array and
// returns the system prompt, which the Codex endpoint takes as a separate
// `instructions` field rather than as a message.
func codexInput(items []Item) ([]map[string]any, string) {
	// Reuse the chat normaliser: it synthesises a result for any tool call that
	// was never answered. The Responses API rejects a dangling function_call, so
	// an interrupted run would otherwise fail its next request outright.
	items = normalizeItemsForChat(items)
	canonicalResult := canonicalToolResults(items)
	emittedResult := map[string]bool{}

	input := make([]map[string]any, 0, len(items))
	var instructions []string

	for _, item := range items {
		switch item.Kind {
		case "text":
			switch {
			case item.Role == RoleSystem:
				if text := strings.TrimSpace(item.Text); text != "" {
					instructions = append(instructions, item.Text)
				}
			case item.Role == RoleTool:
				if item.CallID != "" {
					if chosen, ok := canonicalResult[item.CallID]; ok {
						if emittedResult[item.CallID] || item.Output != chosen {
							continue
						}
						emittedResult[item.CallID] = true
					}
				}
				output := item.Output
				if strings.TrimSpace(output) == "" {
					output = "No output was produced."
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": item.CallID,
					"output":  output,
				})
			case item.Role == RoleAssistant:
				if strings.TrimSpace(item.Text) == "" {
					continue
				}
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": item.Text}},
				})
			default:
				if strings.TrimSpace(item.Text) == "" {
					continue
				}
				input = append(input, map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "input_text", "text": item.Text}},
				})
			}
		case "tool_call":
			input = append(input, codexFunctionCall(item.CallID, item.Name, item.Args))
		case "tool_calls":
			for _, call := range item.ToolCalls {
				input = append(input, codexFunctionCall(call.ID, call.Name, call.Args))
			}
		}
	}
	return input, strings.Join(instructions, "\n\n")
}

func codexFunctionCall(callID, name, args string) map[string]any {
	if repaired, changed := RepairToolArgumentsJSON(args); changed {
		args = repaired
	}
	if callID == "" {
		// A function_call without a call_id cannot be answered, and the API
		// rejects the turn. A stable synthetic id keeps the history coherent.
		callID = "call_" + name
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": args,
	}
}

// codexErrorMessage extracts a human-readable reason from either error envelope
// the endpoint uses: `{"detail": ...}` for request-shape rejections and
// `{"error": {...}}` for model and auth failures.
func codexErrorMessage(body []byte) string {
	var flat struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &flat) == nil && strings.TrimSpace(flat.Detail) != "" {
		return flat.Detail
	}
	var nested struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &nested) == nil {
		if msg := strings.TrimSpace(nested.Error.Message); msg != "" {
			if nested.Error.Code != "" {
				return msg + " (" + nested.Error.Code + ")"
			}
			return msg
		}
		if msg := strings.TrimSpace(nested.Message); msg != "" {
			return msg
		}
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 400 {
		trimmed = trimmed[:400] + "..."
	}
	return trimmed
}

type codexStreamEvent struct {
	Type      string `json:"type"`
	Delta     string `json:"delta"`
	ItemID    string `json:"item_id"`
	OutputIdx *int   `json:"output_index"`
	Item      struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Usage  *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// streamCodex performs a streaming Responses API call against the Codex
// endpoint.
//
// The wire shape is not a chat-completions body and the endpoint is strict
// about it: `store` must be false, `stream` must be true, and parameters such
// as `temperature` and `max_output_tokens` are rejected outright. Those
// constraints were established against the live endpoint, not assumed.
func (c *StreamingClient) streamCodex(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	if c.cfg.CodexTokens == nil {
		return fmt.Errorf("codex provider selected but no Codex credential is available: run /codex-login")
	}
	token, err := c.cfg.CodexTokens.AccessToken(ctx)
	if err != nil {
		return fmt.Errorf("codex credential unavailable: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no Codex credential stored: run /codex-login to sign in")
	}

	input, instructions := codexInput(req.Messages)
	body := map[string]any{
		"model":  c.cfg.Model,
		"input":  input,
		"store":  false,
		"stream": true,
	}
	if strings.TrimSpace(instructions) != "" {
		body["instructions"] = instructions
	}
	if req.Reason != "" {
		body["reasoning"] = map[string]any{"effort": string(req.Reason)}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			})
		}
		body["tools"] = tools
		choice := req.ToolChoice
		if choice == "" {
			choice = c.cfg.ToolChoice
		}
		if choice == "" {
			choice = "auto"
		}
		body["tool_choice"] = choice
	}
	if req.PromptCacheKey != "" {
		body["prompt_cache_key"] = req.PromptCacheKey
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode codex request: %w", err)
	}
	url := codexResponsesURL(c.cfg.BaseURL)

	note := fmt.Sprintf("codex model=%s host=%s input=%d tools=%d effort=%q\n",
		c.cfg.Model, requestHost(c.cfg.BaseURL), len(input), len(req.Tools), string(req.Reason))
	fmt.Fprint(os.Stderr, "ceraa: "+note)
	writeDebugNote("codex.log", note)

	var lastErr error
	forced := false
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		resp, err := c.postCodex(ctx, url, token, encoded)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err
			}
			if attempt < c.cfg.MaxRetries {
				if !sleepContext(ctx, backoff(attempt)) {
					return err
				}
				continue
			}
			return err
		}

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			resp.Body.Close()
			message := codexErrorMessage(raw)
			writeDebugNote("codex.log", fmt.Sprintf("codex status=%d detail=%s\n", resp.StatusCode, message))

			// A credential rejected before its recorded expiry is recoverable:
			// force one exchange and replay the request once.
			if resp.StatusCode == http.StatusUnauthorized && !forced {
				if refresher, ok := c.cfg.CodexTokens.(CodexRefresher); ok {
					forced = true
					if refreshed, rerr := refresher.RefreshToken(ctx); rerr == nil && refreshed != "" {
						token = refreshed
						continue
					}
				}
			}
			perr := &ProviderError{
				Status:    resp.StatusCode,
				Body:      message,
				Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			}
			lastErr = perr
			if perr.Retryable && attempt < c.cfg.MaxRetries {
				if !sleepContext(ctx, backoff(attempt)) {
					return perr
				}
				continue
			}
			return perr
		}
		defer resp.Body.Close()
		return c.readCodexStream(resp.Body, onDelta)
	}
	return lastErr
}

func (c *StreamingClient) postCodex(ctx context.Context, url, token string, encoded []byte) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	for key, value := range c.cfg.ExtraHeaders {
		hreq.Header.Set(key, value)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, &ProviderError{Status: 0, Body: err.Error(), Retryable: true}
	}
	return resp, nil
}

// readCodexStream converts the Responses API event stream into the same
// ProviderResponse contract every other transport emits, so callers do not
// branch on provider.
func (c *StreamingClient) readCodexStream(r io.Reader, onDelta func(ProviderResponse)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)

	// Function calls arrive as whole items rather than as argument deltas, so
	// they are collected by output index and emitted once complete.
	calls := map[int]StreamedToolCall{}
	var order []int
	var responseID, responseModel, finishReason string
	var usage *UsageInfo
	emitted := false
	sawToolCall := false

	emit := func(resp ProviderResponse) {
		emitted = true
		if onDelta != nil {
			onDelta(resp)
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event codexStreamEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		switch event.Type {
		case "response.output_text.delta":
			if event.Delta == "" {
				continue
			}
			emit(ProviderResponse{Content: event.Delta, ResponseID: responseID, ResponseModel: responseModel})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if event.Delta == "" {
				continue
			}
			emit(ProviderResponse{Reasoning: event.Delta, ResponseID: responseID, ResponseModel: responseModel})
		case "response.output_item.added", "response.output_item.done":
			if event.Item.Type != "function_call" {
				continue
			}
			index := 0
			if event.OutputIdx != nil {
				index = *event.OutputIdx
			}
			call, ok := calls[index]
			if !ok {
				call = StreamedToolCall{Index: index}
				calls[index] = call
				order = append(order, index)
			}
			if event.Item.CallID != "" {
				call.ID = event.Item.CallID
			}
			if event.Item.Name != "" {
				call.Name = event.Item.Name
			}
			if event.Item.Arguments != "" {
				call.Arguments = mergeStreamedArguments(call.Arguments, event.Item.Arguments)
			}
			calls[index] = call
			sawToolCall = true
		case "response.completed", "response.incomplete", "response.failed":
			if event.Response.ID != "" {
				responseID = event.Response.ID
			}
			if event.Response.Model != "" {
				responseModel = event.Response.Model
			}
			if u := event.Response.Usage; u != nil {
				usage = &UsageInfo{
					InputTokens:       u.InputTokens,
					OutputTokens:      u.OutputTokens,
					TotalTokens:       u.TotalTokens,
					CachedInputTokens: u.InputTokensDetails.CachedTokens,
					ReasoningTokens:   u.OutputTokensDetails.ReasoningTokens,
				}
				if usage.TotalTokens == 0 {
					usage.TotalTokens = usage.InputTokens + usage.OutputTokens
				}
			}
			if event.Response.IncompleteDetails.Reason != "" {
				finishReason = event.Response.IncompleteDetails.Reason
			} else if event.Response.Status == "completed" {
				finishReason = "stop"
			}
		case "error", "response.error":
			message := event.Error.Message
			if message == "" {
				message = "codex stream reported an error"
			}
			return &ProviderError{Status: 0, Body: message}
		}
	}
	if err := scanner.Err(); err != nil {
		return &StreamError{Err: err, Retryable: true, Partial: emitted}
	}

	var finalCalls []StreamedToolCall
	for _, index := range order {
		call := calls[index]
		if call.Name == "" {
			continue
		}
		if call.Arguments == "" {
			call.Arguments = "{}"
		}
		finalCalls = append(finalCalls, call)
	}
	if len(finalCalls) > 0 {
		finishReason = "tool_calls"
	}
	if finishReason == "" && !sawToolCall {
		finishReason = "stop"
	}

	// The terminal event carries state only. Text and reasoning were already
	// delivered as deltas, so repeating the accumulated value here would double
	// it for any consumer that concatenates every event it receives.
	if onDelta != nil {
		onDelta(ProviderResponse{
			Done:          true,
			FinishReason:  finishReason,
			ResponseID:    responseID,
			ResponseModel: responseModel,
			ToolCalls:     finalCalls,
			Usage:         usage,
		})
	}
	return nil
}

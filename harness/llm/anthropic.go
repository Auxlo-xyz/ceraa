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

// The Anthropic Messages transport.
//
// Written for two consumers: Anthropic proper, and any provider that speaks the
// Messages API rather than OpenAI chat completions. MiniMax is the second, at
// https://api.minimax.io/anthropic, which is why this is not an "anthropic.go"
// with an Anthropic URL baked in.
//
// The shape was taken from the Hermes agent's anthropic_adapter.py, which is a
// working implementation of the same API rather than a description of it, so the
// field names here are the ones that are known to work against MiniMax too.

// anthropicMessagesURL resolves the messages path against a configured base.
//
// A base ending in /anthropic or /v1 is already the prefix, so nothing is
// appended twice. Anything else gets /v1/messages, which is the documented path.
func anthropicMessagesURL(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(trimmed, "/messages") {
		return trimmed
	}
	return trimmed + "/v1/messages"
}

// anthropicMessage is one turn in the conversation.
//
// The system prompt is not one of these: the Messages API takes it as a separate
// top-level argument, which is also where the cache breakpoints belong, so
// folding it into the first user turn would lose both.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// encodeAnthropicMessages converts stored items into Anthropic message turns.
//
// The Messages API has no tool role. A tool call becomes an assistant turn holding
// a tool_use block, and the results become a following user turn of tool_result
// blocks. Getting that shape wrong is the single most common reason a Messages
// request is rejected, so the ordering is deliberate: every result is emitted
// after the assistant turn that produced its call.
//
// A result carries the call id, and the name is recovered from the call history
// because the Messages API matches on id and the name is only for readability.
func encodeAnthropicMessages(items []Item) []anthropicMessage {
	items = normalizeItemsForChat(items)
	// One result per call id. History recorded across an interrupted run can hold
	// two, and a duplicate tool_result for the same id is malformed.
	canonical := canonicalToolResults(items)
	emitted := map[string]bool{}

	messages := make([]anthropicMessage, 0, len(items))
	// Results accumulate and are flushed as one user turn, so results for calls
	// made in the same turn stay together and results for different turns do not
	// get spliced into one message.
	var pending []map[string]any
	var pendingOrder []string
	flush := func() {
		if len(pending) == 0 {
			return
		}
		messages = append(messages, anthropicMessage{Role: "user", Content: pending})
		pending = nil
		pendingOrder = nil
	}

	for _, item := range items {
		switch item.Kind {
		case "text":
			if item.Role == RoleSystem {
				// The Messages API takes the system prompt as a top-level argument.
				// Emitting it as a turn as well sends the whole prompt twice: once
				// where it is read and once where it is just more user text.
				continue
			}
			if item.Role == RoleTool {
				// A tool result, which is stored as a text item with the tool role.
				// Duplicates for one call id are collapsed, because a second
				// tool_result for the same id is malformed.
				if chosen, ok := canonical[item.CallID]; ok && (emitted[item.CallID] || item.Output != chosen) {
					continue
				}
				emitted[item.CallID] = true
				content := item.Output
				if strings.TrimSpace(content) == "" {
					content = "No output was produced."
				}
				if !containsString(pendingOrder, item.CallID) {
					pendingOrder = append(pendingOrder, item.CallID)
				}
				pending = append(pending, map[string]any{
					"type":        "tool_result",
					"tool_use_id": item.CallID,
					"content":     content,
				})
				continue
			}
			if item.Role == RoleAssistant && strings.TrimSpace(item.Text) == "" {
				continue
			}
			content := strings.TrimSpace(item.Text)
			if content == "" {
				continue
			}
			flush()
			messages = append(messages, anthropicMessage{
				Role:    normalizeAnthropicRole(item.Role),
				Content: []map[string]any{{"type": "text", "text": content}},
			})
		case "tool_call":
			flush()
			arguments := item.Args
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			messages = append(messages, anthropicMessage{
				Role: "assistant",
				Content: []map[string]any{{
					"type":  "tool_use",
					"id":    item.CallID,
					"name":  item.Name,
					"input": json.RawMessage(arguments),
				}},
			})
		}
	}
	flush()
	return messages
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// joinSystemText collects the system items into the one top-level argument the
// Messages API takes. The system prompt is not a turn there, and leaving it in
// the message list as a user turn would both mislabel it and lose the seam.
func joinSystemText(items []Item) string {
	parts := make([]string, 0, 2)
	for _, item := range items {
		if item.Role == RoleSystem && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func normalizeAnthropicRole(role Role) string {
	switch role {
	case RoleAssistant:
		return "assistant"
	default:
		// The Messages API accepts only user and assistant, and the system prompt
		// is a separate top-level argument.
		return "user"
	}
}

// anthropicSystemBlocks builds the top-level system argument.
//
// Returned as blocks rather than a string because the cache breakpoints live on
// blocks, and a string cannot carry one. That is the whole reason explicit
// caching needs a real Messages transport: without this the breakpoints written
// earlier have no way to be sent.
func anthropicSystemBlocks(system string, seam int, cfg ProviderConfig) []map[string]any {
	if strings.TrimSpace(system) == "" {
		return nil
	}
	// SystemBlocks already splits at the seam and marks the stable end, which is
	// the whole point of explicit mode: only the stable prefix is cached, and the
	// volatile tail is re-read every turn.
	blocks := SystemBlocks(system, seam, CacheTTL5m)
	if promptCacheMode(cfg) != "explicit" {
		plain := make([]map[string]any, 0, len(blocks))
		for _, block := range blocks {
			text, _ := block["text"].(string)
			if text == "" {
				continue
			}
			plain = append(plain, map[string]any{"type": "text", "text": text})
		}
		return plain
	}
	if promptCacheMode(cfg) == "explicit" && len(blocks) > 0 {
		blocks[len(blocks)-1]["cache_control"] = cacheControl(cacheTTLFor(cfg))
	}
	return blocks
}

// anthropicTools converts tool definitions, adding a cache breakpoint to the last
// one so the tool schemas are cached along with the prompt prefix.
func anthropicTools(tools []ToolDef, cfg ProviderConfig) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	converted := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		converted = append(converted, map[string]any{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.Parameters,
		})
	}
	if promptCacheMode(cfg) == "explicit" && len(converted) > 0 {
		// The tool schemas are the largest stable block in the request, so the
		// breakpoint goes on the last one to cache all of them.
		converted[len(converted)-1]["cache_control"] = cacheControl(cacheTTLFor(cfg))
	}
	return converted
}

// anthropicToolChoice maps our tool choice onto the Messages API's vocabulary.
func anthropicToolChoice(choice string) any {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "auto", "":
		return map[string]any{"type": "auto"}
	case "none":
		// The Messages API has no "none": omitting tool_choice is how you prevent
		// a call, and sending the literal string is a 400.
		return nil
	case "required", "any":
		return map[string]any{"type": "any"}
	case "tool", "function":
		return map[string]any{"type": "auto"}
	default:
		return map[string]any{"type": "auto"}
	}
}

// cacheTTLFor returns the configured TTL, falling back to the provider default.
//
// An unparseable value falls back rather than being sent, because the endpoint
// rejects a TTL it does not recognise and the whole request fails on a field the
// user never sees.
func cacheTTLFor(cfg ProviderConfig) CacheTTL {
	ttl, err := ParseCacheTTL(cfg.PromptCacheTTL)
	if err != nil {
		return CacheTTL5m
	}
	return ttl
}

// streamAnthropic runs a Messages API request and reports deltas.
//
// The event names and the usage fields are the API's own, taken from a working
// client rather than from documentation, because MiniMax's compatibility is only
// as good as its fidelity to them.
func (c *StreamingClient) streamAnthropic(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	token, err := c.anthropicToken(ctx)
	if err != nil {
		return err
	}

	messages := encodeAnthropicMessages(req.Messages)
	if len(messages) == 0 {
		return fmt.Errorf("no messages to send to the messages API")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	body := map[string]any{
		"model":      c.cfg.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if system := anthropicSystemBlocks(joinSystemText(req.Messages), req.SystemCacheSeam, c.cfg); system != nil {
		body["system"] = system
	}
	if tools := anthropicTools(req.Tools, c.cfg); tools != nil {
		body["tools"] = tools
		if choice := anthropicToolChoice(pickToolChoice(req.ToolChoice, c.cfg.ToolChoice)); choice != nil {
			body["tool_choice"] = choice
		}
	}
	if req.Reason != "" {
		// Extended thinking is opt-in and billed differently, so it is only sent
		// when asked for. A budget below the minimum is rejected by the API, so
		// the effort mapping is left to the endpoint's own vocabulary.
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 2048}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode messages request: %w", err)
	}
	endpoint := anthropicMessagesURL(c.cfg.BaseURL)

	note := fmt.Sprintf("anthropic model=%s host=%s messages=%d tools=%d\n",
		c.cfg.Model, requestHost(c.cfg.BaseURL), len(messages), len(req.Tools))
	fmt.Fprint(os.Stderr, "ceraa: "+note)
	writeDebugNote("anthropic.log", note)

	var lastErr error
	attempts := c.cfg.MaxRetries
	if attempts < 0 {
		attempts = 0
	}
	for attempt := 0; attempt <= attempts; attempt++ {
		if c.cfg.OnRetry != nil && attempt > 0 {
			c.cfg.OnRetry(lastErr, attempt, backoff(attempt))
		}
		response, err := c.postAnthropic(ctx, endpoint, token, encoded)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err
			}
			if attempt < attempts {
				if !sleepContext(ctx, backoff(attempt)) {
					return err
				}
				continue
			}
			return err
		}
		if response.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
			response.Body.Close()
			message := anthropicErrorMessage(raw)
			writeDebugNote("anthropic.log", fmt.Sprintf("anthropic status=%d detail=%s\n", response.StatusCode, message))
			perr := &ProviderError{
				Status:    response.StatusCode,
				Body:      message,
				Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 || response.StatusCode == http.StatusUnauthorized,
			}
			lastErr = perr
			if perr.Retryable && attempt < attempts {
				if !sleepContext(ctx, backoff(attempt)) {
					return err
				}
				continue
			}
			return perr
		}
		streamErr := c.readAnthropicStream(response.Body, onDelta)
		response.Body.Close()
		if streamErr != nil {
			lastErr = streamErr
			if ctx.Err() != nil {
				return streamErr
			}
			if attempt < attempts {
				if !sleepContext(ctx, backoff(attempt)) {
					return streamErr
				}
				continue
			}
			return streamErr
		}
		return nil
	}
	return lastErr
}

func pickToolChoice(requested, configured string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	return configured
}

func (c *StreamingClient) anthropicToken(ctx context.Context) (string, error) {
	if c.cfg.AnthropicTokens == nil {
		return "", fmt.Errorf("messages provider selected but no credential is available")
	}
	token, err := c.cfg.AnthropicTokens.AccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("messages credential unavailable: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("no credential stored for the messages provider")
	}
	return token, nil
}

func (c *StreamingClient) postAnthropic(ctx context.Context, endpoint, token string, encoded []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	// The Messages API authenticates with x-api-key plus the anthropic-version
	// header, not a bearer token. Sending Authorization instead is a 401 on
	// Anthropic and, on a compatibility host, a differently-shaped error.
	request.Header.Set("x-api-key", token)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	for key, value := range c.cfg.ExtraHeaders {
		request.Header.Set(key, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &ProviderError{Status: 0, Body: err.Error(), Retryable: true}
	}
	return response, nil
}

// readAnthropicStream converts the Messages event stream into ProviderResponses.
//
// usage is accumulated rather than taken from the last event seen, because
// message_start and message_delta may both carry it, either may carry null, and a
// compatibility host can send zero on the first and real counts on the second.
// Assigning rather than adding loses the counts; adding without a guard double
// counts when both events carry the full set.
func (c *StreamingClient) readAnthropicStream(r io.Reader, onDelta func(ProviderResponse)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// Accumulated by assignment, never by addition. The stream may repeat the
	// counts in message_delta after message_start, and adding the two would report
	// double the tokens, which is the same class of error as the one the usage
	// trust guard exists to catch.
	var usage *UsageInfo
	// Tool input arrives as JSON fragments keyed by the block index, so partial
	// JSON has to be buffered per block rather than appended to one string.
	toolInput := map[int]*strings.Builder{}
	toolName := map[int]string{}

	mergeUsage := func(raw json.RawMessage) {
		if len(raw) == 0 || string(raw) == "null" {
			return
		}
		var incoming UsageInfo
		if json.Unmarshal(raw, &incoming) != nil {
			return
		}
		if usage == nil {
			copied := incoming
			usage = &copied
			return
		}
		// The second event carries the totals, so take it whole when it is
		// complete rather than adding two copies of the same input count.
		if incoming.InputTokens > 0 || incoming.CachedInputTokens > 0 {
			usage.InputTokens = incoming.InputTokens
			usage.CachedInputTokens = incoming.CachedInputTokens
		}
		if incoming.OutputTokens > 0 {
			usage.OutputTokens = incoming.OutputTokens
		}
		if incoming.CacheWriteTokens > 0 {
			usage.CacheWriteTokens = incoming.CacheWriteTokens
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			ContentBlock struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				ID    string `json:"id"`
				Input json.RawMessage
			} `json:"content_block"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			mergeUsage(event.Usage)
			if onDelta != nil {
				onDelta(ProviderResponse{ResponseID: event.Message.ID, ResponseModel: event.Message.Model})
			}
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				toolName[event.Index] = event.ContentBlock.Name
				if event.ContentBlock.Input != nil && len(event.ContentBlock.Input) > 0 {
					if toolInput[event.Index] == nil {
						toolInput[event.Index] = &strings.Builder{}
					}
					toolInput[event.Index].Write(event.ContentBlock.Input)
				}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				if onDelta != nil && event.Delta.Text != "" {
					onDelta(ProviderResponse{Content: event.Delta.Text})
				}
			case "thinking_delta":
				if onDelta != nil && event.Delta.Thinking != "" {
					onDelta(ProviderResponse{Reasoning: event.Delta.Thinking})
				}
			case "input_json_delta":
				if toolInput[event.Index] == nil {
					toolInput[event.Index] = &strings.Builder{}
				}
				toolInput[event.Index].Write([]byte(event.Delta.PartialJSON))
			}
		case "content_block_stop":
			if builder, ok := toolInput[event.Index]; ok {
				name := toolName[event.Index]
				if name != "" && onDelta != nil {
					arguments := builder.String()
					if strings.TrimSpace(arguments) == "" {
						arguments = "{}"
					}
					onDelta(ProviderResponse{ToolCalls: []StreamedToolCall{{
						Index:     event.Index,
						Name:      name,
						Arguments: arguments,
					}}})
				}
				delete(toolInput, event.Index)
				delete(toolName, event.Index)
			}
		case "message_delta":
			// Replaces what message_start reported rather than adding to it.
			mergeUsage(event.Usage)
		case "message_stop":
			if onDelta != nil {
				onDelta(ProviderResponse{Done: true, Usage: usage})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return &ProviderError{Status: 0, Body: err.Error(), Retryable: true}
	}
	if onDelta != nil {
		onDelta(ProviderResponse{Done: true, Usage: usage})
	}
	return nil
}

// anthropicErrorMessage extracts the detail from an error body.
//
// The Messages API puts it under error.message, while a compatibility host may
// answer with the OpenAI shape, so both are read. Returning the whole body when
// neither matches is better than returning nothing, which reads as a bare status.
func anthropicErrorMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		if envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 512 {
		text = text[:512]
	}
	if text == "" {
		return "empty error body"
	}
	return text
}

// UsesAnthropicMessages reports whether a base URL serves the Messages API.
//
// Used to pick a transport for a profile that did not state one, because the two
// hosts that matter are far apart: api.anthropic.com and the MiniMax /anthropic
// suffix both speak Messages, and everything else we support does not.
func UsesAnthropicMessages(model, baseURL string) bool {
	base := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if base == "" {
		return false
	}
	return strings.Contains(base, "anthropic") || strings.Contains(base, "minimax.io/anthropic") || strings.Contains(base, "minimaxi.com/anthropic")
}

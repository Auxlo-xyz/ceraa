package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// anthropicImageBlocks turns image paths into Messages image blocks.
//
// The file is loaded, resized and re-encoded here rather than earlier, so the
// session stores a path and never a copy of the picture. The resize is
// deterministic, so the same image produces the same bytes on a later request
// and still matches the cached prefix.
//
// A file that has since gone missing, or is not decodable, yields no block: the
// result text already says what was attached, so the turn degrades to a
// description rather than failing.
func anthropicImageBlocks(paths []string) []map[string]any {
	blocks := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		attachment, err := LoadMediaAttachment(path)
		if err != nil || attachment == nil {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": attachment.MimeType,
				"data":       attachment.Base64Data(),
			},
		})
	}
	return blocks
}

// MessagesAuthBearer is the MessagesAuthStyle for a host that authenticates with
// Authorization: Bearer rather than x-api-key.
const MessagesAuthBearer = "bearer"

// MessagesAuthDsh is the MessagesAuthStyle for a host that authenticates with
// x-dsh-auth-token: the DeepSeek account grant. Declared here rather than in
// the auth package so the transport owns every header it can send; the token
// itself still comes from the auth package, never from this file.
const MessagesAuthDsh = "dsh"

func anthropicMessagesURL(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(trimmed, "/messages") {
		return trimmed
	}
	// A base that already names the API version gets /messages appended, not
	// /v1/messages. MiniMax's agent backend is served from a base ending in /v1,
	// and appending unconditionally produced .../llm/v1/v1/messages, which is a
	// 404 that reads like a bad path rather than a doubled version.
	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed + "/messages"
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

// toolInputBuilder accumulates one tool_use block's input.
//
// A streamed block arrives as an empty object on content_block_start followed by
// input_json_delta fragments that together form the whole object, so the two are
// alternatives rather than halves of one value. Concatenating them produced
// {}{"command":"ls"} and the request then failed to encode.
type toolInputBuilder struct {
	// seed is whatever content_block_start carried.
	seed strings.Builder
	// streamed is the concatenation of the input_json_delta fragments.
	streamed strings.Builder
	// sawStreamed records that at least one fragment arrived, which is what
	// makes the fragments authoritative over the seed.
	sawStreamed bool
}

// arguments returns the block's input as a single JSON value.
//
// An empty result becomes {} rather than being sent as an absent value, so the
// endpoint is never asked to read a tool call that has no arguments object.
func (b *toolInputBuilder) arguments() string {
	value := ""
	switch {
	case b.sawStreamed:
		value = b.streamed.String()
	case b.seed.Len() > 0:
		value = b.seed.String()
	}
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	return value
}

// toolInputJSON validates one tool_use input value and describes it if it cannot
// be sent.
//
// The value reaches json.RawMessage on the request, so a malformed one is
// otherwise only visible as "invalid character '{' after top-level value" from
// deep inside encoding/json, naming neither the tool nor the arguments. The
// snippet is bounded because arguments are model output and can be large.
func toolInputJSON(name, arguments string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		trimmed = "{}"
	}
	if !json.Valid([]byte(trimmed)) {
		snippet := trimmed
		if len(snippet) > 120 {
			snippet = snippet[:120] + "…"
		}
		return nil, fmt.Errorf("tool %q returned arguments that are not one JSON value: %q", name, snippet)
	}
	return json.RawMessage(trimmed), nil
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
//
// It returns an error when a tool call's arguments are not one JSON value, so the
// fault is reported against the tool that produced it rather than surfacing later
// as an encoding failure with no indication of which call was at fault.
func encodeAnthropicMessages(items []Item) ([]anthropicMessage, error) {
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
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": item.CallID,
					"content":     content,
				}
				// An image a read attached travels beside the result text, in the
				// Messages shape. Without this the read returned the file's bytes
				// as text and the model never saw the picture at all.
				if blocks := anthropicImageBlocks(item.Images); len(blocks) > 0 {
					mixed := []any{map[string]any{"type": "text", "text": content}}
					for _, img := range blocks {
						mixed = append(mixed, img)
					}
					block["content"] = mixed
				}
				pending = append(pending, block)
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
			input, err := toolInputJSON(item.Name, item.Args)
			if err != nil {
				return nil, err
			}
			messages = append(messages, anthropicMessage{
				Role: "assistant",
				Content: []map[string]any{{
					"type":  "tool_use",
					"id":    item.CallID,
					"name":  item.Name,
					"input": input,
				}},
			})
		case "tool_calls":
			// Parallel calls from one turn arrive as a single item and belong in a
			// single assistant message.
			//
			// This case was missing, so the whole turn was dropped: the switch had
			// no arm for it and fell through, the tool_use blocks never went out,
			// and the tool_results that followed referred to ids the endpoint had
			// never been given. MiniMax's agent backend says so precisely —
			// "invalid params, tool result's tool id … not found (2013)" — and the
			// run fails on the request after the parallel batch. Every other
			// transport handled it, which is why only the Messages API showed it.
			flush()
			blocks := make([]map[string]any, 0, len(item.ToolCalls))
			for _, call := range item.ToolCalls {
				input, err := toolInputJSON(call.Name, call.Args)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": input,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			content := make([]map[string]any, 0, len(blocks)+1)
			// Text alongside the calls is part of the same assistant turn, and
			// dropping it loses whatever the model said before calling.
			if text := strings.TrimSpace(item.Text); text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			content = append(content, blocks...)
			messages = append(messages, anthropicMessage{Role: "assistant", Content: content})
		}
	}
	flush()
	return messages, nil
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

	messages, err := encodeAnthropicMessages(req.Messages)
	if err != nil {
		return err
	}
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
		// Extended thinking is opt-in and billed differently, so it is only sent when
		// asked for. The shape is the endpoint's own, not ours.
		applyAnthropicThinking(body, req.Reason, maxTokens)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode messages request: %w", err)
	}
	endpoint := anthropicMessagesURL(c.cfg.BaseURL)

	note := fmt.Sprintf("anthropic model=%s host=%s messages=%d tools=%d\n",
		c.cfg.Model, requestHost(c.cfg.BaseURL), len(messages), len(req.Tools))
	diagNote(note)
	writeDebugNote("anthropic.log", note)

	var lastErr error
	attempts := c.cfg.MaxRetries
	if attempts < 0 {
		attempts = 0
	}
	// A credential rejected before its recorded expiry is recoverable, so one
	// forced exchange is attempted and the request replayed once.
	//
	// The Responses transport has had this since it was written, and MiniMax did
	// not get it with the port: TokenSource.RefreshToken has existed the whole
	// time and nothing on this transport ever called it, because the only caller
	// of the interface lives in streamResponses. A token revoked server-side
	// therefore burned three retries with backoff re-sending the same rejected
	// credential and then failed, on a provider whose access token can renew
	// itself. The accessors also refresh proactively inside a skew, which does
	// not help when the revocation is the reason for the 401.
	refresher := codexRefresher(c.cfg.AnthropicTokens)
	forced := false
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
			if response.StatusCode == http.StatusUnauthorized && !forced && refresher != nil {
				forced = true
				if refreshed, rerr := refresher.RefreshToken(ctx); rerr == nil && refreshed != "" {
					token = refreshed
					continue
				}
			}
			// 401 is not retryable. It was, until the recovery above existed, which
			// made it the worst case in the set: a credential that cannot be renewed
			// was re-sent three times with 1s/3s/8s of backoff before reporting the
			// same failure. The Responses transport has always excluded it for the
			// same reason.
			perr := &ProviderError{
				Status:    response.StatusCode,
				Body:      message,
				Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500,
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

// anthropicEffort maps our reasoning level onto the Messages API vocabulary.
//
// The effort is not a reasoning parameter: it goes in output_config, and
// reasoning.effort is accepted and ignored, which is a silent way to send a
// setting that appears to work and does nothing. Hermes and the DeepSeek harness
// both name these three fields, so the spelling here is theirs.
func anthropicEffort(level ReasonLevel) string {
	switch level {
	case ReasonMinimal:
		return "low"
	case ReasonLow, ReasonMedium:
		return "medium"
	case ReasonHigh, ReasonXHigh, ReasonMax:
		return "high"
	default:
		return ""
	}
}

// minimumThinkingBudget is the smallest budget the Messages API accepts.
//
// A budget below the minimum is a hard 400 rather than a clamp, so a request that
// merely asked for thinking would fail outright.
const minimumThinkingBudget = 1024

// applyAnthropicThinking writes the thinking and effort fields.
//
// Adaptive mode is preferred: the endpoint sizes the budget itself, which avoids
// the constraint that budget_tokens must stay below max_tokens. Budget mode is
// kept for a caller that wants to pin it, and the budget is then raised above the
// minimum and held under max_tokens because exceeding max_tokens is a 400 rather
// than a clamp.
func applyAnthropicThinking(body map[string]any, level ReasonLevel, maxTokens int) {
	if level == "" || level == ReasonOff {
		return
	}
	thinking := map[string]any{"type": "adaptive"}
	if effort := anthropicEffort(level); effort != "" {
		// output_config carries the effort. sending reasoning.effort instead is
		// accepted and ignored.
		body["output_config"] = map[string]any{"effort": effort}
	}
	body["thinking"] = thinking
}

// applyAnthropicThinkingBudget pins an explicit budget instead of adaptive mode.
func applyAnthropicThinkingBudget(body map[string]any, budget, maxTokens int) {
	if budget < minimumThinkingBudget {
		budget = minimumThinkingBudget
	}
	// The budget is counted against max_tokens, so a budget at or above it is a
	// 400 rather than a clamp.
	if maxTokens > 0 && budget >= maxTokens {
		budget = maxTokens - 1
		if budget < minimumThinkingBudget {
			return
		}
	}
	body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
}

func pickToolChoice(requested, configured string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	return configured
}

// anthropicToken resolves the credential for a Messages request.
//
// Two sources, and the order matters. A subscription profile carries a managed
// credential that is refreshed for us; an API-key profile carries the key on the
// profile itself. The managed one wins because it is the one that can renew
// itself, and a profile key is a fallback rather than a peer.
func (c *StreamingClient) anthropicToken(ctx context.Context) (string, error) {
	if c.cfg.AnthropicTokens == nil {
		// No managed credential, so the profile's own key has to serve. Without
		// this the transport refuses outright, which left a plain Anthropic API
		// key unable to use it at all: the Messages transport existed but had no
		// reachable path to a credential.
		if key := strings.TrimSpace(c.cfg.APIKey); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("messages provider selected but no credential is available: sign in or set an API key")
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
	// The Messages API on Anthropic, and on MiniMax's platform API, authenticates
	// with x-api-key plus the anthropic-version header, not a bearer token. Sending
	// Authorization instead is a 401 on Anthropic and, on a compatibility host, a
	// differently-shaped error.
	//
	// MiniMax's agent backend is the other way round and says so: it answers an
	// x-api-key request with {"code":401,"message":"token is required"}. So the
	// style is a property of the host, set by the profile, and the token is the
	// same either way. DeepSeek's account endpoint is a third style and says so
	// the same way: an x-api-key or bearer request is not an authorized one.
	switch {
	case strings.EqualFold(strings.TrimSpace(c.cfg.MessagesAuthStyle), MessagesAuthBearer):
		request.Header.Set("Authorization", "Bearer "+token)
	case strings.EqualFold(strings.TrimSpace(c.cfg.MessagesAuthStyle), MessagesAuthDsh):
		request.Header.Set("x-dsh-auth-token", token)
	default:
		request.Header.Set("x-api-key", token)
	}
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	for key, value := range c.cfg.ExtraHeaders {
		request.Header.Set(key, value)
	}
	response, err := c.doRequest(request)
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
	// sawStop records that a terminal event already carried the usage out.
	//
	// message_stop emits the final delta and the end of the body emits one again,
	// so a well-formed stream reported its tokens twice. The coordinator adds
	// every delta it receives and counts the prompt locally once per request, so
	// the run's reported input landed at exactly 2x ours - and since a real
	// tokenizer counts more than the local len/4 estimate, that pushed a healthy
	// provider past the 2.0 agreement bound and the run was marked untrusted.
	// An untrusted run is not merely noisy: it reports no cost at all and a 0%
	// cache ratio, so the two figures an operator uses to judge a provider both
	// went to zero. Every test here kept only the last usage value, so none of
	// them could see the first one arriving.
	//
	// Both emits are kept. message_stop is the real terminal event, and the
	// post-loop one is the fallback for a body that simply ends without it.
	var sawStop bool
	// Tool input arrives as JSON fragments keyed by the block index, so partial
	// JSON has to be buffered per block rather than appended to one string.
	//
	// The seed and the streamed fragments are kept apart. content_block_start
	// carries input: {} for a tool_use block and input_json_delta then streams
	// the whole object, so writing both into one buffer yields {}{"command":…}:
	// two top-level values, which json.Marshal rejects with "invalid character
	// '{' after top-level value" when the follow-up request is encoded. That is
	// what a tool call on this endpoint did, and the empty string it left in the
	// session is the {} seed with the real arguments discarded.
	toolInput := map[int]*toolInputBuilder{}
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
				// Registered even with no input, so a block that never streams a
				// fragment still produces a tool call at content_block_stop. It
				// used to be created only when input was present, and a block with
				// neither input nor fragments emitted nothing, so the call the
				// model made simply disappeared.
				builder := toolInput[event.Index]
				if builder == nil {
					builder = &toolInputBuilder{}
					toolInput[event.Index] = builder
				}
				if len(event.ContentBlock.Input) > 0 {
					builder.seed.Write(event.ContentBlock.Input)
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
				builder := toolInput[event.Index]
				if builder == nil {
					builder = &toolInputBuilder{}
					toolInput[event.Index] = builder
				}
				builder.streamed.WriteString(event.Delta.PartialJSON)
				builder.sawStreamed = true
			}
		case "content_block_stop":
			if builder, ok := toolInput[event.Index]; ok {
				name := toolName[event.Index]
				if name != "" && onDelta != nil {
					arguments := builder.arguments()
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
			sawStop = true
			if onDelta != nil {
				onDelta(ProviderResponse{Done: true, Usage: usage})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return &ProviderError{Status: 0, Body: err.Error(), Retryable: true}
	}
	// Only when the stream never sent message_stop. Emitting unconditionally
	// duplicated the usage for every request on this transport.
	if onDelta != nil && !sawStop {
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

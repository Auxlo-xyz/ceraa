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
	"unicode/utf8"
)

// This file implements Google's native Gemini API rather than its
// OpenAI-compatibility layer.
//
// The compat layer carries a thought signature in `extra_content`, a field that
// is not part of the OpenAI schema and that Google has had to reshape more than
// once. The native API has no such gap: a signature is a first-class field
// sitting next to `functionCall` inside a part, and Google documents exactly
// when it is required and when it may be omitted. Everything below exists so
// Gemini never has to be coaxed through an adapter.

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	ID   string         `json:"id,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	ID       string         `json:"id,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig map[string]string `json:"functionCallingConfig,omitempty"`
}

type geminiThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	// ThinkingBudget is the Gemini 2.5 mechanism. The two are mutually
	// exclusive: sending both is a 400.
	ThinkingBudget *int `json:"thinkingBudget,omitempty"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int                   `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// UsesGeminiNative reports whether a provider should use Google's native API.
//
// Native is the default for Google's host, because that is the surface the model
// is actually served by. A URL that explicitly names the OpenAI-compatibility
// path is honoured as an opt-in to the adapter, so an existing configuration
// keeps working.
func UsesGeminiNative(model, baseURL string) bool {
	base := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if base == "" {
		return false
	}
	if isOpenAICompatibleSurface(base) {
		return false
	}
	if strings.Contains(base, "generativelanguage.googleapis.com") {
		return true
	}
	// A renamed or proxied model still needs it when the host is Google's.
	name := strings.ToLower(strings.TrimSpace(model))
	geminiFamily := strings.Contains(name, "gemini") || strings.Contains(name, "gemma")
	return geminiFamily && strings.Contains(base, "googleapis.com")
}

// isOpenAICompatibleSurface reports whether a base URL names Google's
// OpenAI-compatibility endpoint, which is an explicit request for the adapter.
func isOpenAICompatibleSurface(base string) bool {
	return strings.HasSuffix(base, "/openai") || strings.Contains(base, "/openai/chat/completions")
}

// geminiStreamURL builds the streaming endpoint for a model. A base that already
// names a method or model is used as the API root.
func geminiStreamURL(baseURL, model string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{":streamGenerateContent", ":generateContent"} {
		if strings.HasSuffix(base, suffix) {
			return base
		}
	}
	return base + "/models/" + model + ":streamGenerateContent?alt=sse"
}

// encodeGeminiContents converts harness items into native contents.
//
// Two shape rules from Google's documentation are load-bearing here. All
// function responses for one step must live in a single user turn, because
// interleaving them with their calls is rejected, and every functionCall part
// must be returned exactly as received.
func encodeGeminiContents(items []Item) ([]geminiContent, *geminiContent) {
	items = normalizeItemsForChat(items)
	var systemText strings.Builder
	var contents []geminiContent
	// pending accumulates consecutive function responses so they are sent as one
	// turn rather than several.
	var pending []geminiPart

	flush := func() {
		if len(pending) == 0 {
			return
		}
		contents = append(contents, geminiContent{Role: "user", Parts: pending})
		pending = nil
	}

	for _, item := range items {
		switch item.Kind {
		case "text":
			switch item.Role {
			case RoleSystem:
				if strings.TrimSpace(item.Text) != "" {
					if systemText.Len() > 0 {
						systemText.WriteString("\n\n")
					}
					systemText.WriteString(item.Text)
				}
			case RoleTool:
				if strings.TrimSpace(item.Name) == "" {
					continue
				}
				pending = append(pending, geminiPart{FunctionResponse: &geminiFunctionResponse{
					Name:     item.Name,
					Response: geminiResponsePayload(item.Output),
					ID:       item.CallID,
				}})
			case RoleAssistant:
				if strings.TrimSpace(item.Text) == "" {
					continue
				}
				flush()
				part := geminiPart{Text: item.Text}
				if item.ThoughtSignature != "" {
					part.ThoughtSignature = item.ThoughtSignature
				}
				contents = append(contents, geminiContent{Role: "model", Parts: []geminiPart{part}})
			default:
				if strings.TrimSpace(item.Text) == "" {
					continue
				}
				flush()
				contents = append(contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: item.Text}}})
			}
		case "reasoning":
			if strings.TrimSpace(item.Reason) == "" {
				continue
			}
			flush()
			contents = append(contents, geminiContent{Role: "model", Parts: []geminiPart{{Text: item.Reason, Thought: true}}})
		case "tool_call":
			flush()
			parts := geminiFunctionCallParts([]ToolCall{{ID: item.CallID, Name: item.Name, Args: item.Args, ThoughtSignature: item.ThoughtSignature}}, item.Text)
			contents = append(contents, geminiContent{Role: "model", Parts: parts})
		case "tool_calls":
			flush()
			contents = append(contents, geminiContent{Role: "model", Parts: geminiFunctionCallParts(item.ToolCalls, item.Text)})
		}
	}
	flush()

	var system *geminiContent
	if systemText.Len() > 0 {
		system = &geminiContent{Parts: []geminiPart{{Text: systemText.String()}}}
	}
	return contents, system
}

// geminiFunctionCallParts renders a model's function calls as native parts.
//
// A call that carries a signature replays it verbatim. One that does not get
// Google's documented bypass value, which is reserved for exactly this case:
// a function call the model never produced, so no signature can exist. Gemini
// only requires the signature on the first call of a step, so a genuine
// parallel batch keeps its later calls unsigned.
func geminiFunctionCallParts(calls []ToolCall, text string) []geminiPart {
	parts := make([]geminiPart, 0, len(calls)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, geminiPart{Text: text})
	}
	for index, call := range calls {
		args := map[string]any{}
		if trimmed := strings.TrimSpace(call.Args); trimmed != "" {
			if repaired, changed := RepairToolArgumentsJSON(trimmed); changed {
				trimmed = repaired
			}
			if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
				args = map[string]any{}
			}
		}
		part := geminiPart{
			FunctionCall:     &geminiFunctionCall{Name: call.Name, Args: args, ID: call.ID},
			ThoughtSignature: strings.TrimSpace(call.ThoughtSignature),
		}
		if part.ThoughtSignature == "" && index == 0 {
			part.ThoughtSignature = skipThoughtSignatureValidator
		}
		parts = append(parts, part)
	}
	return parts
}

// geminiResponsePayload wraps a tool result as the JSON object a function
// response requires. A result that is already an object is passed through
// unchanged so structured tools keep their shape.
func geminiResponsePayload(output string) map[string]any {
	trimmed := strings.TrimSpace(output)
	if trimmed != "" {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil && decoded != nil {
			return decoded
		}
	}
	if trimmed == "" {
		trimmed = "No output was produced."
	}
	return map[string]any{"output": trimmed}
}

// geminiUnsupportedSchemaKeywords are JSON Schema keywords Gemini's function
// declaration schema rejects outright, which would fail the whole request.
var geminiUnsupportedSchemaKeywords = map[string]struct{}{
	"$anchor": {}, "$comment": {}, "$dynamicAnchor": {}, "$dynamicRef": {},
	"$id": {}, "$schema": {}, "$vocabulary": {}, "additionalItems": {},
	"contains": {}, "contentEncoding": {}, "contentMediaType": {},
	"deprecated": {}, "maxContains": {}, "minContains": {},
	"patternProperties": {}, "prefixItems": {}, "propertyNames": {},
	"readOnly": {}, "unevaluatedItems": {}, "unevaluatedProperties": {},
	"uniqueItems": {}, "writeOnly": {},
}

// sanitizeGeminiSchema removes keywords Gemini cannot accept. Numeric
// constraints that the model ignores are left in place, because dropping them
// would silently widen what a tool accepts.
func sanitizeGeminiSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	sanitized := make(map[string]any, len(schema))
	for key, value := range schema {
		if _, unsupported := geminiUnsupportedSchemaKeywords[key]; unsupported {
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			if properties, isProperties := nested["properties"]; isProperties {
				if list, isList := properties.(map[string]any); isList {
					cleaned := make(map[string]any, len(list))
					for name, definition := range list {
						if child, isChild := definition.(map[string]any); isChild {
							cleaned[name] = sanitizeGeminiSchema(child)
							continue
						}
						cleaned[name] = definition
					}
					nested["properties"] = cleaned
				}
			}
			sanitized[key] = sanitizeGeminiSchema(nested)
			continue
		}
		if list, ok := value.([]any); ok {
			cleaned := make([]any, 0, len(list))
			for _, element := range list {
				if child, isChild := element.(map[string]any); isChild {
					cleaned = append(cleaned, sanitizeGeminiSchema(child))
					continue
				}
				cleaned = append(cleaned, element)
			}
			sanitized[key] = cleaned
			continue
		}
		sanitized[key] = value
	}
	return sanitized
}

func encodeGeminiTools(tools []ToolDef) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	declarations := make([]geminiFunctionDeclaration, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		declarations = append(declarations, geminiFunctionDeclaration{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  sanitizeGeminiSchema(tool.Parameters),
		})
	}
	if len(declarations) == 0 {
		return nil
	}
	return []geminiTool{{FunctionDeclarations: declarations}}
}

// geminiThinkingLevels lists the thinking levels a model accepts, ascending.
// Google rejects an unsupported value outright, so an unknown model is given
// the full Gemini 3 set and a known one its documented subset.
func geminiThinkingLevels(model string) []string {
	name := strings.ToLower(model)
	switch {
	case name == "":
		return nil
	case strings.Contains(name, "2.5"):
		// The 2.5 series has no thinking level at all; it uses a token budget.
		return nil
	case strings.Contains(name, "pro-image"):
		return []string{"high"}
	case strings.Contains(name, "image"):
		return []string{"minimal", "high"}
	case strings.Contains(name, "3-pro"):
		return []string{"low", "high"}
	case strings.Contains(name, "pro"):
		return []string{"low", "medium", "high"}
	case strings.Contains(name, "3.7-flash"), strings.Contains(name, "3.8-flash"):
		return []string{"low", "medium", "high"}
	}
	return []string{"minimal", "low", "medium", "high"}
}

// geminiRequestedLevel maps Ceraa's reasoning level onto the closest Gemini
// thinking level. Gemini has four levels, so the finer Ceraa levels collapse
// onto high rather than being sent as values the API would reject.
func geminiRequestedLevel(level ReasonLevel) string {
	switch level {
	case ReasonMinimal:
		return "minimal"
	case ReasonLow:
		return "low"
	case ReasonMedium:
		return "medium"
	case ReasonHigh, ReasonXHigh, ReasonMax:
		return "high"
	}
	return ""
}

// geminiLevelOrder is Gemini's own ordering of thinking levels, from least to most
// reasoning. It is what makes clamping by position rather than by membership
// possible: a model that omits one level still has a well defined neighbourhood.
var geminiLevelOrder = []string{"minimal", "low", "medium", "high"}

func geminiLevelIndex(level string) int {
	for i, candidate := range geminiLevelOrder {
		if candidate == level {
			return i
		}
	}
	return -1
}

// clampGeminiLevel picks the supported level nearest the request: the next level
// up that the model accepts, or its highest when the request exceeds everything
// available. Without this, asking for minimal on a Pro model is a 400, while
// dropping the level silently substitutes the model default, which for Pro is
// higher reasoning than the user asked for.
func clampGeminiLevel(supported []string, requested string) string {
	if requested == "" || len(supported) == 0 {
		return ""
	}
	wanted := geminiLevelIndex(requested)
	if wanted < 0 {
		return supported[len(supported)-1]
	}
	best := ""
	bestIndex := -1
	for _, level := range supported {
		index := geminiLevelIndex(level)
		if index >= wanted && (bestIndex < 0 || index < bestIndex) {
			best, bestIndex = level, index
		}
	}
	if best == "" {
		// The request is above everything this model supports.
		highest := supported[0]
		highestIndex := geminiLevelIndex(highest)
		for _, level := range supported {
			if index := geminiLevelIndex(level); index > highestIndex {
				highest, highestIndex = level, index
			}
		}
		return highest
	}
	return best
}

// geminiThinkingBudgets maps Ceraa's levels onto a Gemini 2.5 token budget.
// Google documents -1 as dynamic thinking and 0 as thinking off; 2.5 has no
// minimal level, so the Ceraa minimal level is the nearest equivalent.
var geminiThinkingBudgets = map[ReasonLevel]int{
	ReasonMinimal: 0,
	ReasonLow:     1024,
	ReasonMedium:  8192,
	ReasonHigh:    -1,
	ReasonXHigh:   -1,
	ReasonMax:     -1,
}

// buildGeminiThinkingConfig decides how a request controls thinking.
//
// includeThoughts is what makes reasoning visible at all, so it is on unless
// thinking was explicitly turned off. The level itself is only sent when one was
// chosen: an unset level leaves the model on its own documented default, which
// is the behaviour to respect when the user has expressed no preference.
func buildGeminiThinkingConfig(model string, level ReasonLevel) *geminiThinkingConfig {
	config := &geminiThinkingConfig{IncludeThoughts: level != ReasonOff}
	if level == ReasonOff || level == "" {
		return config
	}
	if supported := geminiThinkingLevels(model); len(supported) > 0 {
		if clamped := clampGeminiLevel(supported, geminiRequestedLevel(level)); clamped != "" {
			config.ThinkingLevel = clamped
		}
		return config
	}
	if strings.Contains(strings.ToLower(model), "2.5") {
		if budget, ok := geminiThinkingBudgets[level]; ok {
			config.ThinkingBudget = &budget
		}
	}
	return config
}

func buildGeminiRequest(req StreamRequest, model string) geminiRequest {
	contents, system := encodeGeminiContents(req.Messages)
	body := geminiRequest{Contents: contents, SystemInstruction: system, Tools: encodeGeminiTools(req.Tools)}
	if len(body.Tools) > 0 {
		if choice := strings.ToLower(strings.TrimSpace(req.ToolChoice)); choice != "" && choice != "auto" {
			switch choice {
			case "required", "any":
				body.ToolConfig = &geminiToolConfig{FunctionCallingConfig: map[string]string{"mode": "ANY"}}
			case "none":
				body.ToolConfig = &geminiToolConfig{FunctionCallingConfig: map[string]string{"mode": "NONE"}}
			}
		}
	}
	config := &geminiGenerationConfig{}
	if req.MaxTokens > 0 {
		config.MaxOutputTokens = req.MaxTokens
	}
	config.ThinkingConfig = buildGeminiThinkingConfig(model, req.Reason)
	if *config != (geminiGenerationConfig{}) {
		body.GenerationConfig = config
	}
	return body
}

type geminiStreamEvent struct {
	ResponseID string `json:"responseId"`
	Model      string `json:"modelVersion"`
	Candidates []struct {
		Content struct {
			Role  string       `json:"role"`
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func geminiFinishReason(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "":
		return ""
	}
	return strings.ToLower(reason)
}

// readGeminiStream converts the native stream into Ceraa's provider response
// shape, so the coordinator cannot tell which transport produced it.
func (c *StreamingClient) readGeminiStream(r io.Reader, onDelta func(ProviderResponse)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	acc := map[int]*accumulatedToolCall{}
	var nextIndex int
	var responseID, responseModel, finishReason string
	emitted := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event geminiStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return &StreamError{Err: fmt.Errorf("decode gemini stream event: %w", err), Retryable: false, Partial: emitted}
		}
		// The v1internal method wraps chunks as {"response": {...}}; the
		// public method sends them bare. The original client accepts both in
		// this order, so a wrapped frame unwraps here while a bare one never
		// matches and passes through untouched.
		if len(event.Candidates) == 0 && event.UsageMetadata == nil {
			var envelope struct {
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &envelope); err == nil && len(bytes.TrimSpace(envelope.Response)) > 0 {
				if err := json.Unmarshal(envelope.Response, &event); err != nil {
					return &StreamError{Err: fmt.Errorf("decode gemini stream event: %w", err), Retryable: false, Partial: emitted}
				}
			}
		}
		if event.ResponseID != "" {
			responseID = event.ResponseID
		}
		if event.Model != "" {
			responseModel = event.Model
		}
		delta := ProviderResponse{ResponseID: responseID, ResponseModel: responseModel}
		if len(event.Candidates) > 0 {
			candidate := event.Candidates[0]
			for _, part := range candidate.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					// A native stream repeats the whole part on each chunk, so
					// the call is keyed by its position and the signature is
					// kept if a later chunk carries one.
					index := nextIndex
					nextIndex++
					existing := acc[index]
					if existing == nil {
						existing = &accumulatedToolCall{}
						acc[index] = existing
					}
					if part.FunctionCall.Name != "" {
						existing.Name = part.FunctionCall.Name
					}
					if part.FunctionCall.ID != "" {
						existing.ID = part.FunctionCall.ID
					}
					if part.ThoughtSignature != "" {
						existing.ThoughtSignature = part.ThoughtSignature
					}
					args := ""
					if len(part.FunctionCall.Args) > 0 {
						if encoded, err := json.Marshal(part.FunctionCall.Args); err == nil {
							args = string(encoded)
						}
					}
					if existing.Arguments == "" {
						existing.Arguments = args
					} else if args != "" && existing.Arguments != args {
						existing.Arguments = mergeStreamedArguments(existing.Arguments, args)
					}
					delta.ToolCalls = append(delta.ToolCalls, StreamedToolCall{
						Index:            index,
						ID:               existing.ID,
						Name:             existing.Name,
						Arguments:        existing.Arguments,
						ThoughtSignature: existing.ThoughtSignature,
					})
				case part.FunctionResponse != nil:
					// A function response in a model turn is an echo of history,
					// never new output, so it is not surfaced.
				case part.Text != "":
					if part.Thought {
						delta.Reasoning += part.Text
					} else {
						delta.Content += part.Text
					}
				}
			}
			if reason := geminiFinishReason(candidate.FinishReason); reason != "" {
				finishReason = reason
				delta.FinishReason = reason
			}
		}
		if event.UsageMetadata != nil {
			usage := &UsageInfo{
				InputTokens:       event.UsageMetadata.PromptTokenCount,
				OutputTokens:      event.UsageMetadata.CandidatesTokenCount,
				CachedInputTokens: event.UsageMetadata.CachedContentTokenCount,
				ReasoningTokens:   event.UsageMetadata.ThoughtsTokenCount,
				TotalTokens:       event.UsageMetadata.TotalTokenCount,
			}
			if usage.TotalTokens == 0 {
				usage.TotalTokens = usage.InputTokens + usage.OutputTokens + usage.ReasoningTokens
			}
			delta.Usage = usage
		}
		if delta.Content != "" || delta.Reasoning != "" || len(delta.ToolCalls) > 0 || delta.Usage != nil || delta.FinishReason != "" {
			emitted = true
		}
		onDelta(delta)
	}
	if err := scanner.Err(); err != nil {
		return &StreamError{Err: fmt.Errorf("read gemini stream: %w", err), Retryable: true, Partial: emitted}
	}
	if finishReason == "" {
		return &StreamError{Err: fmt.Errorf("gemini stream ended without finish_reason"), Retryable: true, Partial: emitted}
	}
	onDelta(ProviderResponse{Done: true, FinishReason: finishReason, ResponseID: responseID, ResponseModel: responseModel})
	return nil
}

// streamGemini performs one native Gemini request, including retries. It
// mirrors the chat-completions path so both transports surface the same errors.
func (c *StreamingClient) streamGemini(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	body := buildGeminiRequest(req, c.cfg.Model)
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode gemini request: %w", err)
	}
	url := geminiStreamURL(c.cfg.BaseURL, c.cfg.Model)

	stats := geminiSignatureStats(req.Messages)
	note := fmt.Sprintf("gemini-native model=%s host=%s contents=%d signed_calls=%d unsigned_calls=%d turn=%s\n",
		c.cfg.Model, requestHost(c.cfg.BaseURL), len(body.Contents), stats.signed, stats.unsigned, describeGeminiTurn(body.Contents))
	diagNote(note)
	writeDebugNote("gemini.log", note)

	// A missing thought display has two very different causes: we never asked
	// for thoughts, or we asked and the model returned none. Recording the
	// request's thinking configuration alongside what actually came back tells
	// the two apart, instead of leaving it to guesswork.
	thinking := body.GenerationConfig.thinkingSummary()
	reasoningChars := 0
	thoughtTokens := 0
	tracked := func(delta ProviderResponse) {
		reasoningChars += utf8.RuneCountInString(delta.Reasoning)
		if delta.Usage != nil && delta.Usage.ReasoningTokens > 0 {
			thoughtTokens = delta.Usage.ReasoningTokens
		}
		onDelta(delta)
	}
	finish := func() {
		writeDebugNote("gemini.log", fmt.Sprintf(
			"gemini-thoughts model=%s thinking=%s include_thoughts=%t reasoning_chars=%d thought_tokens=%d\n",
			c.cfg.Model, thinking, includesThoughts(body), reasoningChars, thoughtTokens))
	}

	var lastErr error
	// attempts tracks provider round trips, including the one in flight when the
	// loop exits, so the reported count is the number actually made.
	attempts := 0
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		attempts = attempt + 1
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		if c.cfg.APIKey != "" {
			hreq.Header.Set("x-goog-api-key", c.cfg.APIKey)
		}
		for key, value := range c.cfg.ExtraHeaders {
			hreq.Header.Set(key, value)
		}
		resp, err := c.doRequest(hreq)
		if err != nil {
			lastErr = fmt.Errorf("request: %w", err)
			if attempt < c.cfg.MaxRetries {
				if !sleepContext(ctx, backoffScaled(attempt, c.cfg.RetryBackoff)) {
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
			if providerErr.Retryable && attempt < c.cfg.MaxRetries {
				delay := providerErr.RetryAfter
				if delay <= 0 {
					delay = backoffScaled(attempt, c.cfg.RetryBackoff)
				}
				if !sleepContext(ctx, delay) {
					break
				}
				continue
			}
			break
		}
		err = c.readGeminiStream(resp.Body, tracked)
		_ = resp.Body.Close()
		if err == nil || !isStreamRetryable(err) {
			finish()
		}
		if streamErr, ok := err.(*StreamError); ok && streamErr.Retryable && !streamErr.Partial && attempt < c.cfg.MaxRetries {
			if !sleepContext(ctx, backoffScaled(attempt, c.cfg.RetryBackoff)) {
				break
			}
			continue
		}
		return err
	}
	// Report the attempt count, as the chat-completions path does. See the note in
	// client.go: a terminal ProviderError returned bare gave no way to tell a
	// first-attempt failure from one that had been retried and still failed.
	if providerErr, ok := lastErr.(*ProviderError); ok {
		return fmt.Errorf("after %d attempt(s): %w", attempts, providerErr)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("gemini request failed")
	}
	return fmt.Errorf("after %d attempt(s): %w", attempts, lastErr)
}

// describeGeminiTurn renders a native request for the diagnostic log, naming
// each function call and the signature it carries. Content is never included.
func describeGeminiTurn(contents []geminiContent) string {
	descriptions := make([]string, 0, len(contents))
	for _, content := range contents {
		role := content.Role
		if role == "" {
			role = "system"
		}
		var calls []string
		responses := 0
		for _, part := range content.Parts {
			if part.FunctionResponse != nil {
				responses++
				continue
			}
			if part.FunctionCall == nil {
				continue
			}
			signature := "none"
			switch {
			case part.ThoughtSignature == "":
			case part.ThoughtSignature == skipThoughtSignatureValidator:
				signature = "sentinel"
			default:
				signature = fmt.Sprintf("sig%d", len(part.ThoughtSignature))
			}
			calls = append(calls, part.FunctionCall.Name+":"+signature)
		}
		switch {
		case len(calls) > 0:
			descriptions = append(descriptions, role+"/functionCall("+strings.Join(calls, ",")+")")
		case responses > 0:
			descriptions = append(descriptions, fmt.Sprintf("%s/functionResponse*%d", role, responses))
		default:
			descriptions = append(descriptions, role)
		}
	}
	return strings.Join(descriptions, "|")
}

// thinkingSummary names the thinking configuration a request carried, so a log
// line states the level actually sent rather than the one that was intended.
func (c *geminiGenerationConfig) thinkingSummary() string {
	if c == nil || c.ThinkingConfig == nil {
		return "unset"
	}
	switch {
	case c.ThinkingConfig.ThinkingLevel != "":
		return c.ThinkingConfig.ThinkingLevel
	case c.ThinkingConfig.ThinkingBudget != nil:
		return fmt.Sprintf("budget=%d", *c.ThinkingConfig.ThinkingBudget)
	}
	return "default"
}

func includesThoughts(body geminiRequest) bool {
	return body.GenerationConfig != nil &&
		body.GenerationConfig.ThinkingConfig != nil &&
		body.GenerationConfig.ThinkingConfig.IncludeThoughts
}

// isStreamRetryable reports whether a stream failed in a way a retry may fix.
func isStreamRetryable(err error) bool {
	streamErr, ok := err.(*StreamError)
	return ok && streamErr.Retryable
}

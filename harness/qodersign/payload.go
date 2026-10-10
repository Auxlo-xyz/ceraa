package qodersign

// Ask describes one inference request in caller terms. BuildPayload renders it
// into the plaintext body the signer encrypts, mirroring the vendor client's
// own construction field for field.
type Ask struct {
	// ModelKey is the directory key (qfmodel), not a display name: the
	// endpoint routes on it.
	ModelKey string
	// UserText is the latest user message; it fills chat_context.text.
	UserText   string
	SystemText string
	// IsReasoning writes model_config.is_reasoning.
	IsReasoning bool
	// History is the conversation so far in order. Only role, content and the
	// applicable tool keys travel; caller-side fields must not leak upstream.
	History []Message
	// MaxTokens caps output; zero omits it.
	MaxTokens int
	// ReasoningEffort sets reasoning_effort/enable_thinking; "" omits both.
	// "none" disables thinking.
	ReasoningEffort string
	// Source defaults to "system".
	Source string
	// IsVl writes model_config.is_vl; nil defaults true like the reference.
	IsVl *bool
	// ContextWindow writes parameters.context_length; zero omits it.
	ContextWindow int
	DisplayName   string
	// Format defaults to "openai".
	Format string
	// MaxInputTokens falls back to ContextWindow, then 200000.
	MaxInputTokens int
	// SessionType defaults to "qodercli" (the intl CLI identity).
	SessionType string
	// Business is mandatory: without it the server routes to a dead node.
	// Typically {"type": "agent"}.
	Business map[string]any
	// Tools travel as top-level OpenAI-style definitions; nil sends [].
	Tools []Tool
}

// Message is one transcript entry. Content is a string, or a multimodal array
// for image-carrying messages; tool_calls appears on assistant messages only,
// tool_call_id on role tool only.
type Message struct {
	Role       string
	Content    any
	ToolCalls  []ToolCall `json:",omitempty"`
	ToolCallID string     `json:",omitempty"`
}

// ToolCall is one OpenAI-style call on an assistant message.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is one top-level function definition.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// BuildPayload renders ask into the plaintext request body. requestID and
// sessionID must be fresh UUIDs per call: the server treats a repeated
// request_id as a duplicate and answers 409.
func BuildPayload(ask Ask, requestID, sessionID string) map[string]any {
	source := ask.Source
	if source == "" {
		source = "system"
	}
	format := ask.Format
	if format == "" {
		format = "openai"
	}
	sessionType := ask.SessionType
	if sessionType == "" {
		sessionType = "qodercli"
	}
	maxInput := ask.MaxInputTokens
	if maxInput == 0 {
		maxInput = ask.ContextWindow
	}
	if maxInput == 0 {
		maxInput = 200000
	}

	parameters := map[string]any{}
	if ask.MaxTokens != 0 {
		parameters["max_tokens"] = ask.MaxTokens
	}
	if ask.ReasoningEffort != "" {
		parameters["reasoning_effort"] = ask.ReasoningEffort
		parameters["enable_thinking"] = ask.ReasoningEffort != "none"
	}
	if ask.ContextWindow != 0 {
		parameters["context_length"] = ask.ContextWindow
	}

	messages := make([]any, 0, len(ask.History)+1)
	for _, m := range ask.History {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCalls != nil {
			entry["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		messages = append(messages, entry)
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": ask.UserText})
	}

	tools := make([]any, 0, len(ask.Tools))
	for _, t := range ask.Tools {
		tools = append(tools, t)
	}

	system := []any{}
	if ask.SystemText != "" {
		system = append(system, map[string]any{"type": "text", "text": ask.SystemText})
	}

	body := map[string]any{
		"request_id":     requestID,
		"request_set_id": requestID,
		"chat_record_id": requestID,
		"session_id":     sessionID,
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"text":     ask.UserText,
			"features": []any{},
			"extra": map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"key":          ask.ModelKey,
					"is_reasoning": ask.IsReasoning,
				},
				"originalContent": ask.UserText,
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"session_type":     sessionType,
		"aliyun_user_type": "",
		"model_config": map[string]any{
			"key":              ask.ModelKey,
			"display_name":     ask.DisplayName,
			"model":            "",
			"format":           format,
			"is_vl":            boolOr(ask.IsVl, true),
			"is_reasoning":     ask.IsReasoning,
			"api_key":          "",
			"url":              "",
			"source":           source,
			"max_input_tokens": maxInput,
		},
		"custom_model": nil,
		"system":       system,
		"messages":     messages,
		"tools":        tools,
		"parameters":   parameters,
	}
	if ask.Business != nil {
		body["business"] = ask.Business
	}
	return body
}

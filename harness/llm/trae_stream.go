package llm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// streamTrae is the Trae international native transport: the raw-chat SSE
// endpoint behind a PAT-exchanged ide token.
//
// The wire shape is the Trae CLI's own: config_name + model_name, a stable
// conversation/session id per Ceraa session, text messages, and OpenAI-shaped
// tool declarations. Stream events are `output` (response text deltas plus
// native tool_calls), `token_usage`, and `error`. Anything else is ignored:
// metadata/timing frames carry no content.
//
// v1 limits: message content is text-only (images are dropped, never
// half-sent), and tool results travel as user text in the plugin's shape. If
// the endpoint ever rejects declared tools, the fallback is to omit them and
// run chat-only; that call is made at verification time, not here.

func (c *StreamingClient) streamTrae(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	token := strings.TrimSpace(c.cfg.APIKey)
	if token == "" {
		return fmt.Errorf("trae: no credential configured; sign in from the provider menu first")
	}
	base := strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/")
	if base == "" {
		base = TraeBaseURL
	}
	configName, modelName := TraeWireModel(c.cfg.Model)
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = traeUUID()
	}
	body := map[string]any{
		"config_name":     configName,
		"model_name":      modelName,
		"conversation_id": sessionID,
		"session_id":      sessionID,
		"messages":        traeMessages(req.Messages),
		"stream":          true,
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  tool.Parameters,
				},
			})
		}
		body["tools"] = tools
		toolChoice := strings.TrimSpace(req.ToolChoice)
		if toolChoice == "" {
			toolChoice = "auto"
		}
		body["tool_choice"] = toolChoice
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	extra, _ := json.Marshal(map[string]any{
		"agent_loop_id":         sessionID,
		"api_host":              base,
		"api_key":               token,
		"base_url":              base + "/trae-cli/api/v1/llm/proxy",
		"config_name":           configName,
		"config_source":         1,
		"display_name":          configName,
		"model_name":            modelName,
		"real_api_key":          "",
		"real_base_url":         "",
		"session_id":            sessionID,
		"user_prompt_submit_id": sessionID,
	})
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode trae request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, "POST", base+"/api/ide/v2/llm_raw_chat", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("Authorization", "Cloud-IDE-JWT "+token)
	hreq.Header.Set("X-App-Id", TraeAppID)
	hreq.Header.Set("X-Ide-Version-Code", TraeIDEVersionCode)
	hreq.Header.Set("X-Ide-Function", "chat")
	hreq.Header.Set("Extra", string(extra))
	for key, value := range c.cfg.ExtraHeaders {
		hreq.Header.Set(key, value)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return fmt.Errorf("trae request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return &ProviderError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw)), Retryable: shouldRetryStatus(resp.StatusCode)}
	}
	return traeConsumeStream(resp.Body, onDelta)
}

// traeMessages renders items into raw-chat text messages. Tool traffic is
// text: an assistant tool call becomes an assistant line naming the call, and
// a tool result becomes a user message carrying the output. Reasoning items
// are dropped — the endpoint keeps no thought-signature contract, so replaying
// thoughts would only spend context.
func traeMessages(items []Item) []map[string]any {
	msgs := make([]map[string]any, 0, len(items))
	textOf := func(s string) []map[string]any {
		return []map[string]any{{"type": "text", "text": s}}
	}
	for _, item := range items {
		switch item.Kind {
		case "text":
			role := string(item.Role)
			text := item.Text
			if item.Role == RoleTool {
				role = "user"
				name := strings.TrimSpace(item.Name)
				if name == "" {
					name = strings.TrimSpace(item.CallID)
				}
				output := item.Output
				if strings.TrimSpace(output) == "" {
					output = "No output was produced."
				}
				text = "Tool `" + name + "` returned:\n" + output
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			if role != "system" && role != "user" && role != "assistant" {
				role = "user"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": textOf(text)})
		case "tool_call":
			line := "[called " + item.Name + "(" + item.Args + ")]"
			if strings.TrimSpace(item.Text) != "" {
				line = item.Text + "\n" + line
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": textOf(line)})
		case "tool_calls":
			var parts []string
			if strings.TrimSpace(item.Text) != "" {
				parts = append(parts, item.Text)
			}
			for _, call := range item.ToolCalls {
				parts = append(parts, "[called "+call.Name+"("+call.Args+")]")
			}
			if len(parts) == 0 {
				continue
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": textOf(strings.Join(parts, "\n"))})
		}
	}
	return msgs
}

type traeToolAccum struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

// traeConsumeStream reads raw-chat SSE and emits provider deltas. Text deltas
// stream as they arrive; tool calls accumulate by id and emit whole, because
// the endpoint sends each call's arguments in one frame rather than spliced
// across frames.
func traeConsumeStream(body io.Reader, onDelta func(ProviderResponse)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var event string
	var data strings.Builder
	accum := map[string]*traeToolAccum{}
	var order []string
	usage := &UsageInfo{}
	usageRaw := []byte{}
	flush := func() error {
		if event == "" && data.Len() == 0 {
			return nil
		}
		defer func() {
			event = ""
			data.Reset()
		}()
		raw := strings.TrimSpace(data.String())
		if raw == "" || raw == "[DONE]" {
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return nil
		}
		switch event {
		case "error":
			code, _ := payload["code"]
			message, _ := payload["message"].(string)
			if message == "" {
				message, _ = payload["error"].(string)
			}
			if strings.TrimSpace(message) == "" {
				message = "unknown error"
			}
			return fmt.Errorf("trae error %v: %s", code, message)
		case "token_usage":
			if v, ok := numberField(payload, "prompt_tokens"); ok {
				usage.InputTokens = v
			}
			if v, ok := numberField(payload, "completion_tokens"); ok {
				usage.OutputTokens = v
			}
			if rawBytes, err := json.Marshal(payload); err == nil {
				usageRaw = rawBytes
			}
			onDelta(ProviderResponse{Usage: usage, UsageRaw: usageRaw})
		case "output":
			if text, ok := payload["response"].(string); ok && text != "" {
				onDelta(ProviderResponse{Content: text})
			}
			if reason, ok := payload["reasoning_content"].(string); ok && reason != "" {
				onDelta(ProviderResponse{Reasoning: reason})
			}
			calls, _ := payload["tool_calls"].([]any)
			for i, call := range calls {
				entry, _ := call.(map[string]any)
				if entry == nil {
					continue
				}
				id := stringField(entry, "id", "tool_call_id")
				if id == "" {
					id = fmt.Sprintf("trae-call-%d", len(order))
				}
				name, args := traeCallNameArgs(entry)
				acc, ok := accum[id]
				if !ok {
					acc = &traeToolAccum{index: len(order), id: id}
					accum[id] = acc
					order = append(order, id)
				}
				if name != "" {
					acc.name = name
				}
				if args != "" {
					acc.args.WriteString(args)
				}
				_ = i
			}
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[len("event:"):])
		} else if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	calls := make([]StreamedToolCall, 0, len(order))
	for _, id := range order {
		acc := accum[id]
		calls = append(calls, StreamedToolCall{Index: acc.index, ID: acc.id, Name: acc.name, Arguments: acc.args.String()})
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	onDelta(ProviderResponse{ToolCalls: calls, Done: true, FinishReason: finish, Usage: usage, UsageRaw: usageRaw})
	return nil
}

func traeCallNameArgs(entry map[string]any) (string, string) {
	fn, _ := entry["function"].(map[string]any)
	name := stringField(fn, "name")
	if name == "" {
		name = stringField(entry, "name")
	}
	args := stringField(fn, "arguments")
	if args == "" {
		args = stringField(entry, "arguments", "input")
	}
	return name, args
}

func stringField(entry map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := entry[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func numberField(entry map[string]any, key string) (int, bool) {
	switch v := entry[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func traeUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

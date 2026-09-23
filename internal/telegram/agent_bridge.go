package telegram

import (
	"encoding/json"
	"strings"

	"soraa-agent/internal/llm"
	"soraa-agent/internal/prompt"
	"soraa-agent/internal/tools"
)

func runAgent(user string, onReasoning func(string), onAnswer func(string)) error {
	api, key, model := llm.Config()
	msgs := []llm.Message{{Role: "system", Content: prompt.System}, {Role: "user", Content: user}}
	tl := tools.All()
	tdefs := make([]map[string]any, len(tl))
	m := map[string]tools.Tool{}
	for i, t := range tl {
		tdefs[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}}
		m[t.Name] = t
	}
	for i := 0; i < 10; i++ {
		var content, reasoning string
		var toolCalls []llm.ToolCall
		err := llm.ChatStream(api, key, model, msgs, tdefs, func(d llm.StreamDelta) {
			content += d.Content
			reasoning += d.Reasoning
			if len(d.ToolCalls) > 0 {
				toolCalls = d.ToolCalls
			}
			if d.Reasoning != "" {
				onReasoning(reasoning)
			}
		})
		if err != nil {
			return err
		}
		if len(toolCalls) == 0 {
			onAnswer(content)
			return nil
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: content, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			out, err := m[tc.Function.Name].Run(args)
			if err != nil {
				out = "error: " + err.Error()
			}
			out = strings.TrimSpace(out)
			if len(out) > 4000 {
				out = out[:4000]
			}
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
	return nil
}

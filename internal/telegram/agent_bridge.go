package telegram

import (
	"encoding/json"
	"fmt"
	"strings"

	"soraa-agent/internal/llm"
	"soraa-agent/internal/ops"
	"soraa-agent/internal/prompt"
	"soraa-agent/internal/store"
	"soraa-agent/internal/tools"
)

func runAgent(sessionID string, user string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	sess, err := store.Load(sessionID)
	if err != nil {
		return err
	}

	if len(sess.Messages) == 0 {
		sess.Messages = append(sess.Messages, llm.Message{Role: "system", Content: prompt.System})
	}
	sess.Messages = append(sess.Messages, llm.Message{Role: "user", Content: user})

	api, key, model := llm.Config()
	tl := tools.All()
	tdefs := make([]map[string]any, len(tl))
	for i, t := range tl {
		tdefs[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}}
	}

	for i := 0; i < 10; i++ {
		var content, reasoning string
		var toolCalls []llm.ToolCall
		err := llm.ChatStream(api, key, model, sess.Messages, tdefs, func(d llm.StreamDelta) {
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
			sess.Messages = append(sess.Messages, llm.Message{Role: "assistant", Content: content})
			sess.Save()
			onAnswer(content)
			return nil
		}

		sess.Messages = append(sess.Messages, llm.Message{Role: "assistant", Content: content, ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			if !json.Valid([]byte(tc.Function.Arguments)) {
				continue
			}

			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)

			// Create Operation
			op := store.Operation{
				ID:     tc.ID,
				Tool:   tc.Function.Name,
				Args:   args,
				Status: "pending",
			}
			sess.Operations = append(sess.Operations, op)
			sess.Save()

			if onTool != nil {
				argPreview := tc.Function.Arguments
				if len(argPreview) > 200 {
					argPreview = argPreview[:200] + "..."
				}
				onTool("🔧 " + tc.Function.Name + " " + argPreview)
			}

			// Run Operation via Runner
			out, err := ops.Run(sess, op.ID)
			if err != nil {
				out = "error: " + err.Error()
			}
			out = strings.TrimSpace(out)

			if onTool != nil {
				preview := out
				if len(preview) > 300 {
					preview = preview[:300] + "..."
				}
				onTool("✓ " + tc.Function.Name + " → " + preview)
			}
			if len(out) > 4000 {
				out = out[:4000]
			}
			sess.Messages = append(sess.Messages, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
		// Save progress after each tool execution block
		sess.Save()
	}
	return nil
}

func clearSession(sessionID string) error {
	sess, err := store.Load(sessionID)
	if err != nil {
		return err
	}
	return sess.Clear()
}

package agent

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

func Run(user string) error {
	sessionID := "cli"
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
		resp, err := llm.Chat(api, key, model, sess.Messages, tdefs)
		if err != nil {
			return err
		}
		if len(resp.ToolCalls) == 0 {
			fmt.Println(resp.Content)
			sess.Messages = append(sess.Messages, llm.Message{Role: "assistant", Content: resp.Content})
			sess.Save()
			return nil
		}
		sess.Messages = append(sess.Messages, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
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

			fmt.Printf("» %s (pending)\n", tc.Function.Name)

			// Run Operation via Runner
			out, err := ops.Run(sess, op.ID)
			if err != nil {
				out = "error: " + err.Error()
			}
			out = strings.TrimSpace(out)

			if len(out) > 4000 {
				out = out[:4000]
			}
			sess.Messages = append(sess.Messages, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
			fmt.Printf("✓ %s → %d chars\n", tc.Function.Name, len(out))
		}
		sess.Save()
	}
	return nil
}

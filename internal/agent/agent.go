package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"soraa-agent/internal/llm"
	"soraa-agent/internal/prompt"
	"soraa-agent/internal/tools"
)

func Run(user string) error {
	api, key, model := llm.Config()
	msgs := []llm.Message{
		{Role: "system", Content: prompt.System},
		{Role: "user", Content: user},
	}
	tl := tools.All()
	tdefs := make([]map[string]any, len(tl))
	m := map[string]tools.Tool{}
	for i, t := range tl {
		tdefs[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}}
		m[t.Name] = t
	}
	for i := 0; i < 10; i++ {
		resp, err := llm.Chat(api, key, model, msgs, tdefs)
		if err != nil {
			return err
		}
		if len(resp.ToolCalls) == 0 {
			fmt.Println(resp.Content)
			return nil
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
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
			fmt.Printf("» %s → %d chars\n", tc.Function.Name, len(out))
		}
	}
	return nil
}

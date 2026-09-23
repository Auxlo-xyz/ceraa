package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"soraa-agent/harness/llm"
	"soraa-agent/harness/operation"
	"soraa-agent/harness/sessionstore"
	"soraa-agent/harness/tool"
	"soraa-agent/internal/prompt"
)

// runAgent runs the agent loop within a Telegram session.
// onReasoning streams thinking blocks, onTool reports tool execution progress,
// and onAnswer sends the final response.
func runAgent(sessionID string, user string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := sessionstore.New("sessions")
	if err != nil {
		return err
	}

	sess, err := store.Load(sessionID)
	if err != nil {
		return err
	}

	if len(sess.Messages) == 0 {
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:    llm.RoleSystem,
			Content: prompt.System,
		})
	}
	sess.Messages = append(sess.Messages, sessionstore.Message{
		Role:    llm.RoleUser,
		Content: user,
	})

	reg := tool.DefaultRegistry()
	opMgr := operation.New()

	// Build initial messages
	msgs := []llm.Item{}
	for _, m := range sess.Messages {
		msgs = append(msgs, toLLMItem(m))
	}

	tdefs := []llm.ToolDef{}
	for _, t := range reg.All() {
		tdefs = append(tdefs, llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}

	var lastContent string
	var accumulatedReasoning string

	for i := 0; i < 10; i++ {
		client := llm.New(llm.DefaultConfig())
		streamReq := llm.StreamRequest{
			Messages: msgs,
			Tools:    tdefs,
		}

		var contentBuilder strings.Builder
		toolCalls := []llm.StreamedToolCall{}

		err := client.Stream(ctx, streamReq, func(delta llm.ProviderResponse) {
			if delta.Content != "" {
				contentBuilder.WriteString(delta.Content)
			}
			if delta.Reasoning != "" {
				accumulatedReasoning += delta.Reasoning
				if onReasoning != nil {
					onReasoning(accumulatedReasoning)
				}
			}
			if len(delta.ToolCalls) > 0 {
				toolCalls = delta.ToolCalls
			}
			if delta.Done {
				// pass - results collected below
			}
		})
		if err != nil {
			return err
		}

		content := contentBuilder.String()
		lastContent = content

		if len(toolCalls) == 0 {
			// Final answer
			sess.Messages = append(sess.Messages, sessionstore.Message{
				Role:    llm.RoleAssistant,
				Content: content,
			})
			_ = store.Save(sess)
			if onAnswer != nil && content != "" {
				onAnswer(content)
			}
			return nil
		}

		// Append assistant message
		tc := toolCalls[0]
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:    llm.RoleAssistant,
			Content: content,
			ToolCall: &sessionstore.ToolCallRef{
				ID:   tc.ID,
				Name: tc.Name,
				Args: tc.Arguments,
			},
		})
		_ = store.Save(sess)

		if onTool != nil {
			argPreview := tc.Arguments
			if len(argPreview) > 200 {
				argPreview = argPreview[:200] + "..."
			}
			onTool("🔧 " + tc.Name + " " + argPreview)
		}

		// Execute all tool calls concurrently
		for _, tc := range toolCalls {
			var args map[string]any
			if !json.Valid([]byte(tc.Arguments)) {
				continue
			}
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				continue
			}
			t, ok := reg.Get(tc.Name)
			if !ok {
				continue
			}
			spec, err := t.Translate(args)
			if err != nil {
				continue
			}
			op := operation.Operation{
				ID:     tc.ID,
				Spec:   spec,
				Status: operation.StatusRunning,
			}
			opMgr.Add(op)
		}

		// Collect results
		timeout := time.After(60 * time.Second)
		collected := 0
		resultMap := make(map[string]string)

		for collected < len(toolCalls) {
			select {
			case r := <-opMgr.Results():
				resultMap[r.OpID] = r.Output
				collected++
				if onTool != nil {
					preview := r.Output
					if len(preview) > 300 {
						preview = preview[:300] + "..."
					}
					status := "✓"
					if r.Status == operation.StatusFailed {
						status = "✗"
					}
					onTool(fmt.Sprintf("%s %s → %s", status, r.OpID, preview))
				}
			case <-timeout:
				return fmt.Errorf("tool execution timeout")
			}
		}

		// Append tool results
		for _, tc := range toolCalls {
			out := resultMap[tc.ID]
			if len(out) > 4000 {
				out = out[:4000] + "...[truncated]"
			}
			sess.Messages = append(sess.Messages, sessionstore.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				ToolResult: out,
			})
			msgs = append(msgs, toLLMItem(sessionstore.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				ToolResult: out,
			}))
		}
		_ = store.Save(sess)
		_ = lastContent
	}
	return nil
}

func toLLMItem(m sessionstore.Message) llm.Item {
	if m.ToolCall != nil {
		return llm.Item{
			Kind:   "tool_call",
			CallID: m.ToolCall.ID,
			Name:   m.ToolCall.Name,
			Args:   m.ToolCall.Args,
		}
	}
	return llm.Item{
		Kind:  "text",
		Role:  m.Role,
		Text:  m.Content,
		CallID: m.ToolCallID,
		Output: m.ToolResult,
	}
}

func clearSession(sessionID string) error {
	store, err := sessionstore.New("sessions")
	if err != nil {
		return err
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		return err
	}
	return sess.Clear()
}

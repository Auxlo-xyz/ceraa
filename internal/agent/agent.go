package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"soraa-agent/harness/contextbuilder"
	"soraa-agent/harness/inbox"
	"soraa-agent/harness/llm"
	"soraa-agent/harness/operation"
	"soraa-agent/harness/sessionstore"
	"soraa-agent/harness/tool"
	"soraa-agent/internal/prompt"
)

// Run executes the agent for a single user message via the harness.
func Run(user string) error {
	ctx := context.Background()

	store, err := sessionstore.New("sessions")
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}

	sess, err := store.Load("cli")
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if len(sess.Messages) == 0 {
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:    llm.RoleSystem,
			Content: prompt.System,
		})
		_ = store.Save(sess)
	}
	sess.Messages = append(sess.Messages, sessionstore.Message{
		Role:    llm.RoleUser,
		Content: user,
	})

	reg := tool.DefaultRegistry()
	opMgr := operation.New()

	// Build initial request
	builder := contextbuilder.New(sess, prompt.System)
	req := builder.Build()
	msgs := req.Messages

	// Tool definitions
	tdefs := []llm.ToolDef{}
	for _, t := range reg.All() {
		tdefs = append(tdefs, llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}

	// Create inbox (single message, no prior IDs)
	ib, err := inbox.New(ctx, nil, 1000)
	if err != nil {
		return err
	}
	defer ib.Stop()

	for i := 0; i < 10; i++ {
		client := llm.New(llm.DefaultConfig())
		streamReq := llm.StreamRequest{
			Messages: msgs,
			Tools:    tdefs,
		}

		var contentBuilder strings.Builder
		var toolCalls []llm.StreamedToolCall

		err := client.Stream(ctx, streamReq, func(delta llm.ProviderResponse) {
			if delta.Content != "" {
				contentBuilder.WriteString(delta.Content)
				fmt.Print(delta.Content)
			}
			if len(delta.ToolCalls) > 0 {
				toolCalls = delta.ToolCalls
			}
		})
		if err != nil {
			return fmt.Errorf("LLM: %w", err)
		}

		content := contentBuilder.String()

		if len(toolCalls) == 0 {
			fmt.Println()
			sess.Messages = append(sess.Messages, sessionstore.Message{
				Role:    llm.RoleAssistant,
				Content: content,
			})
			_ = store.Save(sess)
			return nil
		}

		// Append assistant message with tool calls
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

		// Display start
		fmt.Printf("\n» %s\n", tc.Name)

		// Execute all tool calls concurrently
		for _, tc := range toolCalls {
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				fmt.Printf("✗ invalid args: %s\n", err)
				continue
			}
			t, ok := reg.Get(tc.Name)
			if !ok {
				fmt.Printf("✗ tool %s not found\n", tc.Name)
				continue
			}
			spec, err := t.Translate(args)
			if err != nil {
				fmt.Printf("✗ translate error: %s\n", err)
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
				status := "✓"
				if r.Status == operation.StatusFailed {
					status = "✗"
				}
				fmt.Printf("%s %s → %d chars\n", status, r.OpID, len(r.Output))
			case <-timeout:
				return fmt.Errorf("tool execution timeout after %d calls", collected)
			}
		}

		// Append tool results to messages
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
			msgs = append(msgs, llm.Item{
				Kind:   "text",
				Role:   llm.RoleTool,
				CallID: tc.ID,
				Output: out,
			})
		}
		_ = store.Save(sess)
	}
	return nil
}

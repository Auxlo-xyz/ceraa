package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sooraa/harness/contextbuilder"
	"sooraa/harness/inbox"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/tool"
	"sooraa/internal/config"
	"sooraa/internal/prompt"
)

// toolRunner bridges tool.Tool Run methods to operation.ToolRunner.
type toolRunner struct {
	reg *tool.Registry
}

func (r *toolRunner) RunTool(ctx context.Context, spec operation.Spec) (string, error) {
	t, ok := r.reg.Get(spec.Type)
	if !ok {
		return "", fmt.Errorf("unknown tool type: %s", spec.Type)
	}
	runner, ok := t.(tool.Runner)
	if !ok {
		return "", fmt.Errorf("tool %s does not support Run", spec.Type)
	}
	return runner.Run(spec)
}

func toLLMItem(m sessionstore.Message) llm.Item {
	if m.Compaction != nil {
		return llm.Item{Kind: "text", Role: llm.RoleUser, Text: "<conversation-checkpoint>\n<summary>" + m.Compaction.Summary + "</summary>\n<recent-context>" + m.Compaction.Recent + "</recent-context>\n</conversation-checkpoint>"}
	}
	if calls := m.AllToolCalls(); len(calls) > 0 {
		if len(calls) == 1 {
			return llm.Item{Kind: "tool_call", CallID: calls[0].ID, Name: calls[0].Name, Args: calls[0].Args, Text: m.Content}
		}
		items := make([]llm.ToolCall, 0, len(calls))
		for _, call := range calls {
			items = append(items, llm.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args})
		}
		return llm.Item{Kind: "tool_calls", ToolCalls: items, Text: m.Content}
	}
	return llm.Item{Kind: "text", Role: m.Role, Text: m.Content, CallID: m.ToolCallID, Output: m.ToolResult}
}

// Run executes the agent for a single user message via the harness.
func Run(user string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return RunWithConfig(user, cfg)
}

// RunWithConfig executes the agent with the provided config.
func RunWithConfig(user string, cfg *config.Config) error {
	ctx := context.Background()

	store, err := sessionstore.New(cfg.SessionFile)
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
	opMgr := operation.New(&toolRunner{reg: reg})

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

	for i := 0; i < cfg.MaxSteps; i++ {
		client := llm.New(llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model))
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
				toolCalls = llm.MergeStreamedToolCalls(toolCalls, delta.ToolCalls)
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

		assistantMessage := sessionstore.Message{
			Role:      llm.RoleAssistant,
			Content:   content,
			ToolCalls: make([]sessionstore.ToolCallRef, 0, len(toolCalls)),
		}
		for _, tc := range toolCalls {
			assistantMessage.ToolCalls = append(assistantMessage.ToolCalls, sessionstore.ToolCallRef{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
		}
		sess.Messages = append(sess.Messages, assistantMessage)
		msgs = append(msgs, toLLMItem(assistantMessage))

		for _, tc := range toolCalls {
			fmt.Printf("\n» %s\n", tc.Name)
		}

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

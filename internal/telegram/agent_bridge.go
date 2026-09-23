package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sooraa/harness/compaction"
	"sooraa/harness/event"
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

// runAgent runs the agent loop within a Telegram session.
// onReasoning streams thinking blocks, onTool reports tool execution progress,
// and onAnswer sends the final response.
func runAgent(sessionID string, user string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runAgentWithConfig(sessionID, user, cfg, onReasoning, onTool, onAnswer)
}

func runAgentWithConfig(sessionID string, user string, cfg *config.Config, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := sessionstore.New(cfg.SessionFile)
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
	opMgr := operation.New(&toolRunner{reg: reg})

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

	// Compaction setup -> maps to soraacode agent/core/src/session/compaction.ts compactIfNeeded + overflow.ts
	compactor := newCompactor(cfg)

	for i := 0; i < cfg.MaxSteps; i++ {
		// Pre-flight compaction check like soraacode runner/llm.ts compactIfNeeded
		if compactor.NeedsCompaction(sess) {
			if onTool != nil {
				onTool("⚡ Compacting context...")
			}
			if ok, err := compactor.Compact(ctx, sess); err == nil && ok {
				_ = store.Save(sess)
				// Rebuild msgs from compacted session
				msgs = []llm.Item{}
				for _, m := range sess.Messages {
					msgs = append(msgs, toLLMItem(m))
				}
				if onTool != nil {
					onTool("✓ Compaction done, recent preserved")
				}
			}
		}
		// Tool output pruning like soraacode compaction.prune (40k protected, 20k minimum)
		if pruned := pruneIfNeeded(sess); pruned > 0 {
			_ = store.Save(sess)
		}
		client := llm.New(llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model))
		streamReq := llm.StreamRequest{
			Messages: msgs,
			Tools:    tdefs,
		}

		var contentBuilder strings.Builder
		toolCalls := []llm.StreamedToolCall{}

		err := client.Stream(ctx, streamReq, func(delta llm.ProviderResponse) {
			if delta.Content != "" {
				contentBuilder.WriteString(delta.Content)
				// Publish content event -> maps to soraacode ResponseStreamer
				event.Default.Publish(event.Event{Type: event.TypeComplete, SessionID: sessionID, Data: map[string]any{"content": delta.Content}})
			}
			if delta.Reasoning != "" {
				accumulatedReasoning += delta.Reasoning
				if onReasoning != nil {
					onReasoning(accumulatedReasoning)
				}
				// Separate thinking event -> soraacode thinking-rendering.js
				event.Default.Publish(event.Event{Type: event.TypeThinking, SessionID: sessionID, Data: map[string]any{"reasoning": delta.Reasoning, "full": accumulatedReasoning}})
			}
			if len(delta.ToolCalls) > 0 {
				toolCalls = delta.ToolCalls
				for _, tc := range delta.ToolCalls {
					event.Default.Publish(event.Event{Type: event.TypeToolStart, SessionID: sessionID, Data: map[string]any{"tool": tc.Name, "args": tc.Arguments}})
				}
			}
			if delta.Done {
				// pass - results collected below
			}
		})
		if err != nil {
			// Overflow recovery like soraacode processor halt -> needsCompaction -> prompt loop prune+retry
			if compactor.IsOverflow(sess) || strings.Contains(strings.ToLower(err.Error()), "context") || strings.Contains(err.Error(), "token") {
				if onTool != nil {
					onTool("⚡ Context overflow, compacting...")
				}
				if ok, cerr := compactor.Compact(ctx, sess); cerr == nil && ok {
					_ = store.Save(sess)
					msgs = []llm.Item{}
					for _, m := range sess.Messages {
						msgs = append(msgs, toLLMItem(m))
					}
					continue // retry with compacted context
				}
			}
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
			event.Default.Publish(event.Event{Type: event.TypeComplete, SessionID: sessionID, Data: map[string]any{"content": content}})
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
			// Permission check -> maps to soraacode permission-manager generation
			if needsPermission(tc.Name, tc.Arguments) {
				event.Default.Publish(event.Event{Type: event.TypePermission, SessionID: sessionID, Data: map[string]any{"tool": tc.Name, "args": tc.Arguments, "request_id": tc.ID}})
				if onTool != nil {
					onTool("🔒 Permission required: " + tc.Name + " " + tc.Arguments[:min(100, len(tc.Arguments))])
				}
				// For now auto-allow after publish, real flow would wait for callback allowOnce/Always
			}
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
			// Pass session context for question tool -> needed for question manager
			if spec.Args == nil {
				spec.Args = make(map[string]any)
			}
			spec.Args["session_id"] = sessionID
			spec.Args["request_id"] = tc.ID
			op := operation.Operation{
				ID:     tc.ID,
				Spec:   spec,
				Status: operation.StatusRunning,
			}
			opMgr.Add(op)
		}

		// Collect results - timeout 3m to allow question tool 2m wait -> maps to soraacode running-tool-tracker 2h
		timeout := time.After(180 * time.Second)
		collected := 0
		resultMap := make(map[string]string)

		for collected < len(toolCalls) {
			select {
			case r := <-opMgr.Results():
				resultMap[r.OpID] = r.Output
				collected++
				// Publish tool end event -> maps to soraacode ToolMessageBatcher + RunningToolTracker
				event.Default.Publish(event.Event{Type: event.TypeToolEnd, SessionID: sessionID, Data: map[string]any{"op_id": r.OpID, "output": r.Output, "status": string(r.Status)}})
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

		// Publish tokens/cost -> maps to soraacode pinned-message-manager + tokens/cost events
		tokensUsed := 0
		for _, m := range msgs {
			tokensUsed += len(m.Text)/4 + len(m.Args)/4 + len(m.Output)/4 + 10
		}
		event.Default.Publish(event.Event{Type: event.TypeTokens, SessionID: sessionID, Data: map[string]any{"tokens": tokensUsed}})
		event.Default.Publish(event.Event{Type: event.TypeCost, SessionID: sessionID, Data: map[string]any{"cost": fmt.Sprintf("$%.4f", float64(tokensUsed)*0.00001)}})

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
	if m.Compaction != nil {
		// Maps to soraacode agent/core/src/session/runner/to-llm-message.ts compaction case
		return llm.Item{
			Kind: "text",
			Role: llm.RoleUser,
			Text: fmt.Sprintf("<conversation-checkpoint>\n<summary>%s</summary>\n<recent-context>%s</recent-context>\n</conversation-checkpoint>", m.Compaction.Summary, m.Compaction.Recent),
		}
	}
	if m.ToolCall != nil {
		return llm.Item{
			Kind:   "tool_call",
			CallID: m.ToolCall.ID,
			Name:   m.ToolCall.Name,
			Args:   m.ToolCall.Args,
		}
	}
	return llm.Item{
		Kind:   "text",
		Role:   m.Role,
		Text:   m.Content,
		CallID: m.ToolCallID,
		Output: m.ToolResult,
	}
}

func newCompactor(cfg *config.Config) *compaction.Compactor {
	pcfg := llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model)
	return compaction.New(pcfg)
}

func pruneIfNeeded(sess *sessionstore.Session) int {
	return compaction.Prune(sess)
}

func needsPermission(tool, args string) bool {
	if tool != "bash" {
		return false
	}
	lower := strings.ToLower(args)
	risky := []string{"rm -rf", "sudo ", "chmod 777", "mkfs", "dd if=", ":(){:|:&};:"}
	for _, r := range risky {
		if strings.Contains(lower, r) {
			return true
		}
	}
	return false
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

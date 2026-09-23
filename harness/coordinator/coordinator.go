package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sooraa/harness/inbox"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/tool"
)

const (
	ToolHeartbeatInterval = 10 * time.Minute
	toolCallGracePeriod   = 1 * time.Second
)

type Config struct {
	SessionID    string
	SystemPrompt string
	MaxSteps     int
	Reasoning    llm.ReasonLevel
	StopWhenIdle bool
}

// Callbacks let the caller observe async events.
type Callbacks struct {
	OnContent   func(string)                    // streamed text chunk
	OnReasoning func(string)                    // accumulated reasoning
	OnToolStart func(tool.Tool, map[string]any) // tool about to run
	OnToolDone  func(operation.ResultEvent)     // tool completed
	OnDone      func(string)                    // final answer
	OnError     func(error)
}

// RunAsync starts an async agent session. The inbox drives external inputs
// (user messages, stop commands). Operations are dispatched concurrently.
func RunAsync(ctx context.Context, cfg Config, store *sessionstore.Store, reg *tool.Registry, opMgr *operation.LocalManager, ib *inbox.Inbox, cb Callbacks) error {
	sess, err := store.Load(cfg.SessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if sessionstore.Recover(sess) {
		if err := store.Save(sess); err != nil {
			return fmt.Errorf("recover session: %w", err)
		}
	}
	if len(sess.Messages) == 0 {
		sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleSystem, Content: cfg.SystemPrompt})
		if err := store.Save(sess); err != nil {
			return err
		}
	}

	client := llm.New(llm.DefaultConfig())
	tdefs := make([]llm.ToolDef, 0, len(reg.All()))
	for _, t := range reg.All() {
		tdefs = append(tdefs, llm.ToolDef{Name: t.Name(), Description: t.Description(), Parameters: t.Schema()})
	}

	for step := 0; cfg.MaxSteps == 0 || step < cfg.MaxSteps; step++ {
		select {
		case sub := <-ib.Output():
			if err := handleInboxEvent(sub, nil, nil, &cfg, cb); err != nil {
				return err
			}
			if cfg.StopWhenIdle {
				return nil
			}
			continue
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		var contentBuilder strings.Builder
		var toolCalls []llm.StreamedToolCall
		err := client.Stream(ctx, llm.StreamRequest{Messages: buildLLMMessages(sess.Messages), Tools: tdefs, Reason: cfg.Reasoning}, func(delta llm.ProviderResponse) {
			if delta.Content != "" {
				contentBuilder.WriteString(delta.Content)
				if cb.OnContent != nil {
					cb.OnContent(delta.Content)
				}
			}
			if delta.Reasoning != "" && cb.OnReasoning != nil {
				cb.OnReasoning(delta.Reasoning)
			}
			if len(delta.ToolCalls) > 0 {
				toolCalls = llm.MergeStreamedToolCalls(toolCalls, delta.ToolCalls)
			}
		})
		if err != nil {
			if cb.OnError != nil {
				cb.OnError(err)
			}
			return err
		}

		content := contentBuilder.String()
		toolCalls = llm.EnsureToolCallIDs(toolCalls, fmt.Sprintf("call_%d", step))
		if len(toolCalls) == 0 {
			sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleAssistant, Content: content})
			if err := store.Save(sess); err != nil {
				return err
			}
			if cb.OnDone != nil {
				cb.OnDone(content)
			}
			return nil
		}

		assistant := sessionstore.Message{Role: llm.RoleAssistant, Content: content, ToolCalls: make([]sessionstore.ToolCallRef, 0, len(toolCalls))}
		for _, call := range toolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, sessionstore.ToolCallRef{ID: call.ID, Name: call.Name, Args: call.Arguments})
		}
		sess.Messages = append(sess.Messages, assistant)
		if err := store.Save(sess); err != nil {
			return err
		}

		results := make(map[string]string)
		pending := make(map[string]struct{})
		for _, call := range toolCalls {
			var args map[string]any
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				results[call.ID] = "Error: invalid arguments: " + err.Error()
				continue
			}
			t, ok := reg.Get(call.Name)
			if !ok {
				results[call.ID] = "Error: unknown tool " + call.Name
				continue
			}
			spec, err := t.Translate(args)
			if err != nil {
				results[call.ID] = "Error: " + err.Error()
				continue
			}
			if cb.OnToolStart != nil {
				cb.OnToolStart(t, args)
			}
			opMgr.Add(operation.Operation{ID: call.ID, Spec: spec, Status: operation.StatusRunning})
			pending[call.ID] = struct{}{}
		}
		if err := store.Save(sess); err != nil {
			return err
		}
		timeout := time.NewTimer(60 * time.Second)
		for len(pending) > 0 {
			select {
			case result := <-opMgr.Results():
				delete(pending, result.OpID)
				if result.Error != "" {
					results[result.OpID] = "Error: " + result.Error
				} else {
					results[result.OpID] = result.Output
				}
				if cb.OnToolDone != nil {
					cb.OnToolDone(result)
				}
			case <-ctx.Done():
				opMgr.CancelAll()
				if !timeout.Stop() {
					<-timeout.C
				}
				return ctx.Err()
			case <-timeout.C:
				opMgr.CancelAll()
				return fmt.Errorf("tool execution timeout")
			}
		}
		if !timeout.Stop() {
			<-timeout.C
		}
		for _, call := range toolCalls {
			sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleTool, ToolCallID: call.ID, ToolResult: results[call.ID]})
		}
		if err := store.Save(sess); err != nil {
			return err
		}
	}
	return nil
}

func buildLLMMessages(msgs []sessionstore.Message) []llm.Item {
	items := make([]llm.Item, 0, len(msgs))
	for _, m := range msgs {
		if calls := m.AllToolCalls(); len(calls) > 0 {
			if len(calls) == 1 {
				items = append(items, llm.Item{Kind: "tool_call", CallID: calls[0].ID, Name: calls[0].Name, Args: calls[0].Args})
			} else {
				toolCalls := make([]llm.ToolCall, 0, len(calls))
				for _, call := range calls {
					toolCalls = append(toolCalls, llm.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args})
				}
				items = append(items, llm.Item{Kind: "tool_calls", ToolCalls: toolCalls})
			}
			continue
		}
		item := llm.Item{
			Kind: "text",
			Role: m.Role,
			Text: m.Content,
		}
		if m.Role == llm.RoleTool {
			item.CallID = m.ToolCallID
			item.Output = m.ToolResult
		}
		items = append(items, item)
	}
	return items
}

func handleInboxEvent(sub inbox.Submission, pendingToolIDs *map[string]bool, modelCancel context.CancelCauseFunc, cfg *Config, cb Callbacks) error {
	switch sub.Kind {
	case inbox.KindControl:
		var env struct {
			Mode       string          `json:"mode"`
			Reason     string          `json:"reason"`
			Parameters json.RawMessage `json:"parameters"`
		}
		if sub.Kind == inbox.KindControl {
			_ = json.Unmarshal(sub.Payload, &env)
		}
		switch inbox.ControlMode(env.Mode) {
		case inbox.ModeStopHard:
			if modelCancel != nil {
				modelCancel(fmt.Errorf("hard stop: %s", env.Reason))
			}
			return fmt.Errorf("hard stop requested: %s", env.Reason)
		case inbox.ModeStopWhenIdle:
			cfg.StopWhenIdle = true
		case inbox.ModeUpdateSettings:
			var s struct {
				ReasoningEffort string `json:"reasoning,omitempty"`
			}
			_ = json.Unmarshal(env.Parameters, &s)
			if s.ReasoningEffort != "" {
				cfg.Reasoning = llm.ReasonLevel(s.ReasoningEffort)
			}
		}
	case inbox.KindExternal:
		// new user message - would create a new turn
	case inbox.KindCrash:
		return fmt.Errorf("crash event from inbox")
	}
	return nil
}

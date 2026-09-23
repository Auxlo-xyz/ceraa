package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"soraa-agent/harness/inbox"
	"soraa-agent/harness/llm"
	"soraa-agent/harness/operation"
	"soraa-agent/harness/sessionstore"
	"soraa-agent/harness/tool"
)

const (
	ToolHeartbeatInterval = 10 * time.Minute
	toolCallGracePeriod   = 1 * time.Second
)

type Config struct {
	SessionID      string
	SystemPrompt   string
	MaxSteps       int
	Reasoning      llm.ReasonLevel
}

// Callbacks let the caller observe async events.
type Callbacks struct {
	OnContent   func(string)   // streamed text chunk
	OnReasoning func(string)   // accumulated reasoning
	OnToolStart func(tool.Tool, map[string]any) // tool about to run
	OnToolDone  func(operation.ResultEvent)    // tool completed
	OnDone      func(string)   // final answer
	OnError     func(error)
}

// RunAsync starts an async agent session. The inbox drives external inputs
// (user messages, stop commands). Operations are dispatched concurrently.
func RunAsync(ctx context.Context, cfg Config, store *sessionstore.Store, reg *tool.Registry, opMgr *operation.LocalManager, ib *inbox.Inbox, cb Callbacks) error {
	sess, err := store.Load(cfg.SessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	// init system message
	if len(sess.Messages) == 0 {
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:    llm.RoleSystem,
			Content: cfg.SystemPrompt,
		})
		_ = store.Save(sess)
	}

	var (
		mu             sync.Mutex
		content        strings.Builder
		toolCalls      []llm.StreamedToolCall
		pendingToolIDs = make(map[string]bool)
		modelCtx       context.Context
		modelCancel    context.CancelCauseFunc
	)

	client := llm.New(llm.DefaultConfig())

	// Build tool definitions
	tdefs := make([]llm.ToolDef, 0, len(reg.All()))
	for _, t := range reg.All() {
		tdefs = append(tdefs, llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}

	// Main event loop: select over inbox, operation results, and model stream
	for step := 0; step < cfg.MaxSteps || cfg.MaxSteps == 0; step++ {
		// No pending work -> request model response
		if len(pendingToolIDs) == 0 && len(toolCalls) == 0 {
			// Build message list from session
			msgs := buildLLMMessages(sess.Messages)

			// Start model request in a goroutine
			modelCtx, modelCancel = context.WithCancelCause(ctx)
			toolCalls = nil
			content.Reset()

			streamReq := llm.StreamRequest{
				Messages: msgs,
				Tools:    tdefs,
				Reason:   cfg.Reasoning,
			}

			go func() {
				err := client.Stream(modelCtx, streamReq, func(delta llm.ProviderResponse) {
					if delta.Content != "" {
						content.WriteString(delta.Content)
						if cb.OnContent != nil {
							cb.OnContent(delta.Content)
						}
					}
					if delta.Reasoning != "" && cb.OnReasoning != nil {
						cb.OnReasoning(delta.Reasoning)
					}
					if len(delta.ToolCalls) > 0 {
						mu.Lock()
						toolCalls = delta.ToolCalls
						for _, tc := range toolCalls {
							pendingToolIDs[tc.ID] = true
						}
						mu.Unlock()
					}
					if delta.Done {
						// Check if we got tool calls or final content
						mu.Lock()
						hadTools := len(toolCalls) > 0
						txt := content.String()
						mu.Unlock()

						if !hadTools {
							// Final answer
							if cb.OnDone != nil {
								cb.OnDone(txt)
							}
							// Save and finish
							sess.Messages = append(sess.Messages, sessionstore.Message{
								Role:    llm.RoleAssistant,
								Content: txt,
							})
							_ = store.Save(sess)
						}
					}
				})
				if err != nil && modelCancel != nil {
					modelCancel(err)
				}
			}()
		}

		// Select on: inbox events, operation results, heartbeat, context done
		select {
		case <-ctx.Done():
			if modelCancel != nil {
				modelCancel(ctx.Err())
			}
			return ctx.Err()

		case sub := <-ib.Output():
			if err := handleInboxEvent(sub, &pendingToolIDs, modelCancel, &cfg, cb); err != nil {
				return err
			}

		case opResult := <-opMgr.Results():
			// Tool completed - add result to messages
			mu.Lock()
			delete(pendingToolIDs, opResult.OpID)
			remaining := len(pendingToolIDs)
			mu.Unlock()

			if cb.OnToolDone != nil {
				cb.OnToolDone(opResult)
			}

			// Append tool result to session
			sess.Messages = append(sess.Messages, sessionstore.Message{
				Role:       llm.RoleTool,
				ToolCallID: opResult.OpID,
				ToolResult: opResult.Output,
			})
			_ = store.Save(sess)

			// If all tool calls done, request next model turn
			if remaining == 0 && len(toolCalls) > 0 {
				// Reset for next turn
				mu.Lock()
				toolCalls = nil
				mu.Unlock()
				// Loop will auto-request model since pendingToolIDs is now 0
			}

		case <-time.After(ToolHeartbeatInterval):
			if len(pendingToolIDs) > 0 && cb.OnError != nil {
				cb.OnError(fmt.Errorf("heartbeat: %d tools still running", len(pendingToolIDs)))
			}
		}
	}

	return nil
}

func buildLLMMessages(msgs []sessionstore.Message) []llm.Item {
	items := make([]llm.Item, 0, len(msgs))
	for _, m := range msgs {
		if m.ToolCall != nil {
			items = append(items, llm.Item{
				Kind:   "tool_call",
				CallID: m.ToolCall.ID,
				Name:   m.ToolCall.Name,
				Args:   m.ToolCall.Args,
			})
		} else {
			item := llm.Item{
				Kind:  "text",
				Role:  m.Role,
				Text:  m.Content,
			}
			if m.Role == llm.RoleTool {
				item.CallID = m.ToolCallID
				item.Output = m.ToolResult
			}
			items = append(items, item)
		}
	}
	return items
}

func handleInboxEvent(sub inbox.Submission, pendingToolIDs *map[string]bool, modelCancel context.CancelCauseFunc, cfg *Config, cb Callbacks) error {
	switch sub.Kind {
	case inbox.KindControl:
		var env struct {
			Mode       string          `json:"mode"`
			Reason     string          `json:"reason"`
			Parameters json.RawMessage  `json:"parameters"`
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
			cfg.MaxSteps = 0 // signal to stop after current turn
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

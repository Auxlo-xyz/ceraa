package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"sooraa/harness/inbox"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/tool"
)

const ToolHeartbeatInterval = 10 * time.Minute

func RunAsync(ctx context.Context, cfg Config, store *sessionstore.Store, reg *tool.Registry, opMgr *operation.LocalManager, ib *inbox.Inbox, cb Callbacks) error {
	_, err := New(RunnerConfig{
		SessionID:          cfg.SessionID,
		SystemPrompt:       cfg.SystemPrompt,
		MaxSteps:           cfg.MaxSteps,
		Reasoning:          cfg.Reasoning,
		StopWhenIdle:       cfg.StopWhenIdle,
		Model:              cfg.Model,
		ContextLimit:       cfg.ContextLimit,
		MaxOutputTokens:    cfg.MaxOutputTokens,
		WorkDir:            cfg.WorkDir,
		ToolOutputDir:      cfg.ToolOutputDir,
		MaxToolResultChars: cfg.MaxToolResultChars,
		Inbox:              ib,
		OperationPool:      opMgr,
		Compactor:          cfg.Compactor,
		ForcedCompaction:   cfg.ForcedCompaction,
		AuthorizeTool:      cfg.AuthorizeTool,
		NeedsAuthorization: cfg.NeedsAuthorization,
		AdjustSpec:         cfg.AdjustSpec,
		Callbacks:          cb,
	}, store, reg).Run(ctx, "")
	if err != nil {
		if cb.OnError != nil {
			cb.OnError(err)
		}
		return err
	}
	return nil
}

func buildLLMMessages(msgs []sessionstore.Message) []llm.Item {
	items := make([]llm.Item, 0, len(msgs))
	for _, m := range msgs {
		if calls := m.AllToolCalls(); len(calls) > 0 {
			if len(calls) == 1 {
				items = append(items, llm.Item{Kind: "tool_call", CallID: calls[0].ID, Name: calls[0].Name, Args: calls[0].Args, Text: m.Content})
			} else {
				toolCalls := make([]llm.ToolCall, 0, len(calls))
				for _, call := range calls {
					toolCalls = append(toolCalls, llm.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args})
				}
				items = append(items, llm.Item{Kind: "tool_calls", ToolCalls: toolCalls, Text: m.Content})
			}
			continue
		}
		items = append(items, llm.Item{Kind: "text", Role: m.Role, Text: m.Content, CallID: m.ToolCallID, Output: m.ToolResult})
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
		_ = json.Unmarshal(sub.Payload, &env)
		switch inbox.ControlMode(env.Mode) {
		case inbox.ModeStopHard:
			if modelCancel != nil {
				modelCancel(fmt.Errorf("hard stop: %s", env.Reason))
			}
			return fmt.Errorf("hard stop requested: %s", env.Reason)
		case inbox.ModeStopWhenIdle:
			cfg.StopWhenIdle = true
		case inbox.ModeUpdateSettings:
			var settings struct {
				ReasoningEffort string `json:"reasoning,omitempty"`
			}
			_ = json.Unmarshal(env.Parameters, &settings)
			if settings.ReasoningEffort != "" {
				cfg.Reasoning = llm.ReasonLevel(settings.ReasoningEffort)
			}
		}
	case inbox.KindCrash:
		return fmt.Errorf("crash event from inbox")
	}
	return nil
}

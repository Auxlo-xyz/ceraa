package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"ceraa/harness/inbox"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

const ToolHeartbeatInterval = 10 * time.Minute

func RunAsync(ctx context.Context, cfg Config, store *sessionstore.Store, reg *tool.Registry, opMgr *operation.LocalManager, ib *inbox.Inbox, cb Callbacks) error {
	_, err := New(RunnerConfig{
		SessionID:           cfg.SessionID,
		SystemPrompt:        cfg.SystemPrompt,
		Reasoning:           cfg.Reasoning,
		StopWhenIdle:        cfg.StopWhenIdle,
		Model:               cfg.Model,
		ContextLimit:        cfg.ContextLimit,
		MaxOutputTokens:     cfg.MaxOutputTokens,
		WorkDir:             cfg.WorkDir,
		ToolOutputDir:       cfg.ToolOutputDir,
		MaxToolResultChars:  cfg.MaxToolResultChars,
		ModelCatalogURL:     cfg.ModelCatalogURL,
		ModelCatalogCache:   cfg.ModelCatalogCache,
		ModelCatalogRefresh: cfg.ModelCatalogRefresh,
		Inbox:               ib,
		OperationPool:       opMgr,
		Compactor:           cfg.Compactor,
		ForcedCompaction:    cfg.ForcedCompaction,
		AuthorizeTool:       cfg.AuthorizeTool,
		NeedsAuthorization:  cfg.NeedsAuthorization,
		AdjustSpec:          cfg.AdjustSpec,
		Callbacks:           cb,
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
	toolNames := make(map[string]string)
	for _, m := range msgs {
		for _, call := range m.AllToolCalls() {
			if call.Name != "" {
				toolNames[call.ID] = call.Name
			}
		}
	}
	for _, m := range msgs {
		if calls := m.AllToolCalls(); len(calls) > 0 {
			if len(calls) == 1 {
				args := calls[0].Args
				if repaired, changed := llm.RepairToolArgumentsJSON(args); changed {
					args = repaired
				}
				items = append(items, llm.Item{Kind: "tool_call", CallID: calls[0].ID, Name: calls[0].Name, Args: args, ThoughtSignature: calls[0].ThoughtSignature, Text: m.Content})
			} else {
				toolCalls := make([]llm.ToolCall, 0, len(calls))
				for _, call := range calls {
					args := call.Args
					if repaired, changed := llm.RepairToolArgumentsJSON(args); changed {
						args = repaired
					}
					toolCalls = append(toolCalls, llm.ToolCall{ID: call.ID, Name: call.Name, Args: args, ThoughtSignature: call.ThoughtSignature})
				}
				items = append(items, llm.Item{Kind: "tool_calls", ToolCalls: toolCalls, Text: m.Content})
			}
			continue
		}
		name := m.ToolName
		if name == "" {
			name = toolNames[m.ToolCallID]
		}
		items = append(items, llm.Item{Kind: "text", Role: m.Role, Text: m.Content, CallID: m.ToolCallID, Name: name, Output: m.ToolResult})
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

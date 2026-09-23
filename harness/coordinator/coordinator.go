// Package coordinator orchestrates the async agent loop.
// It reads events from an Inbox, runs LLM turns, dispatches tool calls
// concurrently, and wakes for results. Heartbeat and grace periods
// ensure liveness without blocking.
package coordinator

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
)

var (
	ToolHeartbeatInterval = 10 * time.Minute
	toolCallGracePeriod   = 1 * time.Second
)

type Dependencies struct {
	Inbox          *inbox.Inbox
	Sessions       *sessionstore.Store
	Builder        *contextbuilder.Builder
	LLM            *llm.StreamingClient
	ToolRegistry   *tool.Registry
	Operations     *operation.LocalManager
	StopWhenIdle   bool
	HeartbeatFunc  func(reason string) // called on heartbeat
}

type turnState struct {
	currentToolCalls map[string]bool // by call ID
	pendingOps       map[string]struct{}
}

// Run owns the main event loop for one session until stop or context cancel.
func Run(ctx context.Context, deps Dependencies) error {
	turnID := fmt.Sprintf("turn-%d", time.Now().UnixNano())
	ts := &turnState{
		currentToolCalls: make(map[string]bool),
		pendingOps:       make(map[string]struct{}),
	}

	// Main loop: select over inbox, operation results, heartbeats, and model timeouts.
	for {
		// Request a model response if we have no pending tool results
		if len(ts.pendingOps) == 0 && len(ts.currentToolCalls) == 0 {
			if deps.StopWhenIdle {
				// no tool calls or pending ops, session complete
				return nil
			}
			if err := requestModel(ctx, deps, turnID, ts); err != nil {
				return fmt.Errorf("model response: %w", err)
			}
			continue
		}

		// Wait for events from inbox or operation results
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sub := <-deps.Inbox.Output():
			if err := handleInput(ctx, deps, sub, turnID, ts); err != nil {
				return err
			}
		case opResult := <-deps.Operations.Results():
			// process completed operation
			_ = opResult
			// mark op as done, feed result to builder for next turn
			ts.pendingOps = make(map[string]struct{})
		}
	}
}

func requestModel(ctx context.Context, deps Dependencies, turnID string, ts *turnState) error {
	// Build request from context builder
	req := deps.Builder.Build()

	// Create model request with tool definitions
	tdefs := []llm.ToolDef{}
	for _, t := range deps.ToolRegistry.All() {
		schema := t.Schema()
		props, _ := schema["properties"].(map[string]any)
		desc, _ := props[t.Name()].(string)
		tdefs = append(tdefs, llm.ToolDef{
			Name:        t.Name(),
			Description: desc,
			Parameters:  schema,
		})
	}

	streamReq := llm.StreamRequest{
		Messages: req.Messages,
		Tools:    tdefs,
	}

	var content strings.Builder
	_ = content

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	return deps.LLM.Stream(ctx, streamReq, func(delta llm.ProviderResponse) {
		if delta.Content != "" {
			content.WriteString(delta.Content)
		}
		if delta.Reasoning != "" {
			if deps.HeartbeatFunc != nil {
				deps.HeartbeatFunc(delta.Reasoning)
			}
		}
		if delta.Done {
			// all done
		}
	})
}

func handleInput(ctx context.Context, deps Dependencies, sub inbox.Submission, turnID string, ts *turnState) error {
	switch sub.Kind {
	case inbox.KindControl:
		var ctrl inbox.Control
		if err := json.Unmarshal(sub.Payload, &ctrl); err != nil {
			return fmt.Errorf("decode control: %w", err)
		}
		switch ctrl.Mode {
		case inbox.ModeStopHard:
			return fmt.Errorf("hard stop requested: %s", ctrl.Reason)
		case inbox.ModeStopWhenIdle:
			deps.StopWhenIdle = true
		case inbox.ModeUpdateSettings:
			// apply reasoning effort changes
			if ctrl.Parameters.ReasoningEffort != "" {
				// rebuild context builder with new reasoning level
			}
		}
	case inbox.KindExternal:
		// user sent a new message - this would create a new turn
		_ = ctx
	case inbox.KindCrash:
		return fmt.Errorf("crash event received: %s", string(sub.Payload))
	}
	return nil
}

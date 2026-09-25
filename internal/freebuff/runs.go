package freebuff

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The Freebuff hosted completions route resolves codebuff_metadata.run_id
// against a run registered in their own database, so a client cannot invent
// one. Inventing it fails closed with 400 "runId Not Found".
//
// The reference registers a run with POST /api/v1/agent-runs before the first
// completion and keeps using that id for the run. The untracked-<uuid> prefix
// the reference mints locally is only for its internal context-pruner agent,
// which is deliberately not tracked at all, so it is not a general escape
// hatch for a client.

// agentIDByModel maps a hosted model to the Freebuff root agent the CLI runs for
// it. The CLI ships with CLI_HARNESS base3, so these are the base3 roots.
var agentIDByModel = map[string]string{
	"deepseek/deepseek-v4-pro":   "base3-free-deepseek",
	"deepseek/deepseek-v4-flash": "base3-free-deepseek-flash",
	"mimo/mimo-v2.5":             "base3-free-mimo",
	"mimo/mimo-v2.6-pro":         "base3-free-mimo-2-6-pro",
	"minimax/minimax-m3":         "base3-free-minimax-m3",
	"openai/gpt-5.6-luna":        "base3-free-luna",
	"openai/gpt-5.6-luna-es":     "base3-free-luna-es",
	"openai/gpt-6-luna":          "base3-free-luna-6",
	"z-ai/glm-5.2":               "base3-free-glm",
	"z-ai/glm-5.3-flash":         "base3-free-glm-5-3-flash",
	"crof/kimi-k3-eco":           "base3-free-kimi-k3-eco",
	"upstage/solar-pro4":         "base3-free-solar-pro4",
	"upstage/solar-mini4":        "base3-free-solar-mini4",
	"stealth/ox-alpha":           "base3-free-ox-alpha",
	"stealth/space-bunny-alpha":  "base3-free-space-bunny-alpha",
	"google/gemini-3.8-flash":    "base3-free-gemini-3-8-flash",
	"meta/muse-spark-1.2-contributor": "base3-free-muse-spark",
	"meta/muse-spark-1.3-contributor": "base3-free-muse-spark-1-3",
}

// defaultAgentID is the reference's fallback for a model with no mapped root.
// It deliberately runs the model's own base2 harness rather than another
// model's root, because the wrong model is a session_model_mismatch 403.
const defaultAgentID = "base2-free"

// AgentIDForModel returns the Freebuff root agent id for a hosted model.
func AgentIDForModel(model string) string {
	if agentID, ok := agentIDByModel[model]; ok {
		return agentID
	}
	return defaultAgentID
}

// runStartPath is the run-registration endpoint.
const runStartPath = "/api/v1/agent-runs"

// StartRun registers a run and returns the id the completions route expects.
func (c *Client) StartRun(ctx context.Context, token, agentID string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	body := map[string]any{
		"action":         "START",
		"agentId":        agentID,
		"ancestorRunIds": []string{},
	}
	result, err := c.do(reqCtx, requestOptions{
		method:  "POST",
		path:    runStartPath,
		token:   token,
		body:    body,
		baseURL: c.apiBase(),
	})
	if err != nil {
		return "", fmt.Errorf("register freebuff run: %w", err)
	}
	var payload struct {
		RunID string `json:"runId"`
		RunId string `json:"run_id"`
	}
	if json.Unmarshal(result.body, &payload) != nil {
		return "", fmt.Errorf("register freebuff run: unreadable response")
	}
	runID := strings.TrimSpace(payload.RunID)
	if runID == "" {
		runID = strings.TrimSpace(payload.RunId)
	}
	if runID == "" {
		return "", fmt.Errorf("register freebuff run: response carried no run id")
	}
	return runID, nil
}

package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// liveCodexTokens reads the real credential out of the encrypted store so the
// transport can be exercised exactly as Ceraa runs it. It never logs the token.
//
// It deliberately does not implement CodexRefresher: a live test cannot
// manufacture an unauthorised response, and a stub refresh that returns nothing
// would only pretend to cover the 401 recovery path. That path is covered
// against a fake endpoint in codex_test.go.
type liveCodexTokens struct{}

func (liveCodexTokens) AccessToken(context.Context) (string, error) {
	return secret.Get("codex_access_token")
}

// TestLiveCodexAgentTurn drives a full agent turn through the real transport
// against the real endpoint: a user request, a tool call, a tool result, and a
// final answer. Skipped unless CERAA_LIVE_CODEX=1.
func TestLiveCodexAgentTurn(t *testing.T) {
	if os.Getenv("CERAA_LIVE_CODEX") != "1" {
		t.Skip("set CERAA_LIVE_CODEX=1 to run a live Codex agent turn")
	}
	dir := os.Getenv("CERAA_HOME")
	if dir == "" {
		t.Fatal("CERAA_HOME must be set; refusing to run")
	}
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}

	client := New(ProviderConfig{
		BaseURL:     CodexBaseURL,
		Model:       DefaultCodexModel,
		CodexNative: true,
		CodexTokens: liveCodexTokens{},
		MaxRetries:  1,
		Timeout:     120 * time.Second,
	})
	tools := []ToolDef{{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string", "description": "City name"},
			},
			"required":             []any{"city"},
			"additionalProperties": false,
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Turn 1: the model should call the tool.
	var reply string
	var call StreamedToolCall
	var done bool
	err := client.Stream(ctx, StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "You are a weather assistant. Always use the tool for weather questions."},
			{Kind: "text", Role: RoleUser, Text: "What is the weather in Lagos? Use the tool."},
		},
		Tools:  tools,
		Reason: ReasonLow,
	}, func(r ProviderResponse) {
		reply += r.Content
		if r.Done {
			done = true
			if len(r.ToolCalls) > 0 {
				call = r.ToolCalls[0]
			}
		}
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if !done {
		t.Fatal("turn 1 produced no terminal event")
	}
	if call.Name != "get_weather" {
		t.Fatalf("turn 1: tool call = %+v, want get_weather", call)
	}
	t.Logf("turn 1: called %s(%s)", call.Name, call.Arguments)
	if !strings.Contains(call.Arguments, "Lagos") {
		t.Errorf("turn 1: arguments %q do not name the city", call.Arguments)
	}

	// Turn 2: feed the result back and require a prose answer.
	answer := ""
	err = client.Stream(ctx, StreamRequest{
		Messages: []Item{
			{Kind: "text", Role: RoleSystem, Text: "You are a weather assistant. Always use the tool for weather questions."},
			{Kind: "text", Role: RoleUser, Text: "What is the weather in Lagos? Use the tool."},
			{Kind: "tool_call", CallID: call.ID, Name: call.Name, Args: call.Arguments},
			{Kind: "text", Role: RoleTool, CallID: call.ID, Name: call.Name, Output: `{"temp_c":29,"sky":"rain","city":"Lagos"}`},
		},
		Tools:  tools,
		Reason: ReasonLow,
	}, func(r ProviderResponse) {
		answer += r.Content
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(strings.ToLower(answer), "29") {
		t.Errorf("turn 2: answer %q does not use the tool result", answer)
	}
	t.Logf("turn 2: answered %d chars, mentions 29: %v", len(answer), strings.Contains(answer, "29"))
	t.Log("LIVE CODEX AGENT TURN OK")
}

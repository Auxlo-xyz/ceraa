package coordinator

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// The identity block used to be recomputed on every step, and its error fallback
// silently rendered a different host. A transient resolution failure would then
// rewrite the first token of the system prompt mid-run and invalidate the
// provider's cached prefix from byte zero.
func TestModelIdentityIsPinnedForTheRun(t *testing.T) {
	var calls int
	runner := &Runner{cfg: RunnerConfig{
		Model: llm.FromProvider("https://chatgpt.com/backend-api/codex", "", "gpt-5.6-luna"),
		ResolveModel: func(context.Context) (llm.ProviderConfig, error) {
			calls++
			return llm.ProviderConfig{
				Model:       "gpt-5.6-luna",
				BaseURL:     "https://chatgpt.com/backend-api/codex",
				CodexNative: true,
			}, nil
		},
	}}

	first := runner.modelIdentity()
	for i := 0; i < 20; i++ {
		if got := runner.modelIdentity(); got != first {
			t.Fatalf("identity changed mid-run:\n%q\n%q", first, got)
		}
	}
	if calls != 1 {
		t.Errorf("resolver called %d times, want 1 for the whole run", calls)
	}
	if !strings.Contains(first, "gpt-5.6-luna") {
		t.Errorf("identity does not name the model: %q", first)
	}
}

// A resolver that starts failing must not silently change what the model is told
// it is, mid-run.
func TestModelIdentitySurvivesResolverFlapping(t *testing.T) {
	var fail bool
	runner := &Runner{cfg: RunnerConfig{
		Model: llm.FromProvider("https://hermes.test/v1", "", "configured-model"),
		ResolveModel: func(context.Context) (llm.ProviderConfig, error) {
			if fail {
				return llm.ProviderConfig{}, context.DeadlineExceeded
			}
			return llm.ProviderConfig{Model: "resolved-model", BaseURL: "https://resolved.test/v1"}, nil
		},
	}}
	before := runner.modelIdentity()
	if !strings.Contains(before, "resolved-model") {
		t.Fatalf("identity did not use the resolved model: %q", before)
	}
	fail = true
	if after := runner.modelIdentity(); after != before {
		t.Errorf("identity changed when the resolver started failing:\nbefore %q\nafter  %q", before, after)
	}
}

// The store must never be rewritten, and the request must carry the stored
// result verbatim. Capping happens once in formatOutput when the result is
// captured; anything that shortens it afterwards invalidates the cached prefix
// permanently, because the shortened bytes are what later requests re-send.
func TestTruncateToolOutputLeavesTheStoreIntact(t *testing.T) {
	long := strings.Repeat("x", 5000)
	sess := &sessionstore.Session{Messages: []sessionstore.Message{{
		Role: llm.RoleTool, ToolResult: long, ToolResultPresent: true, ToolCallID: "c1",
	}}}

	// Whatever the request path does, the stored message must be untouched.
	_ = renderForTest(sess)

	if sess.Messages[0].ToolResult != long {
		t.Errorf("stored tool result was mutated: %d bytes now, was %d",
			len(sess.Messages[0].ToolResult), len(long))
	}
}

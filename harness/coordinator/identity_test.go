package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/llm"
)

func TestModelIdentityNamesTheModel(t *testing.T) {
	runner := &Runner{cfg: RunnerConfig{
		Model: llm.ProviderConfig{Model: "gpt-5.6-luna", BaseURL: "https://chatgpt.com/backend-api/codex", CodexNative: true},
	}}
	identity := runner.modelIdentity()
	if !strings.Contains(identity, "gpt-5.6-luna") {
		t.Errorf("identity does not name the model: %q", identity)
	}
	if !strings.Contains(identity, "Codex") {
		t.Errorf("identity does not identify the subscription: %q", identity)
	}
}

// The provider is resolved per run, so a session that switches provider
// mid-flight must be told the new one, not the one it started on.
func TestModelIdentityFollowsTheResolvedProvider(t *testing.T) {
	runner := &Runner{cfg: RunnerConfig{
		Model: llm.ProviderConfig{Model: "stale-model"},
		ResolveModel: func(context.Context) (llm.ProviderConfig, error) {
			return llm.ProviderConfig{Model: "gpt-5.6-luna", BaseURL: "https://chatgpt.com/backend-api/codex", CodexNative: true}, nil
		},
	}}
	identity := runner.modelIdentity()
	if strings.Contains(identity, "stale-model") {
		t.Errorf("identity reports the stale configured model: %q", identity)
	}
	if !strings.Contains(identity, "gpt-5.6-luna") {
		t.Errorf("identity does not follow the resolver: %q", identity)
	}
}

// A failing resolver must not blank the identity or crash the prompt build.
func TestModelIdentitySurvivesResolverError(t *testing.T) {
	runner := &Runner{cfg: RunnerConfig{
		Model: llm.ProviderConfig{Model: "gemini-3.5-flash-lite", BaseURL: "https://example.test"},
		ResolveModel: func(context.Context) (llm.ProviderConfig, error) {
			return llm.ProviderConfig{}, context.DeadlineExceeded
		},
	}}
	identity := runner.modelIdentity()
	if !strings.Contains(identity, "gemini-3.5-flash-lite") {
		t.Errorf("resolver error discarded the configured model: %q", identity)
	}
}

func TestModelIdentityEmptyWithoutModel(t *testing.T) {
	runner := &Runner{cfg: RunnerConfig{Model: llm.ProviderConfig{}}}
	if identity := runner.modelIdentity(); identity != "" {
		t.Errorf("identity = %q, want empty when no model is configured", identity)
	}
}

// The identity must be present in the assembled system prompt, and before the
// vendored prompt text, or it reads as an afterthought.
func TestSystemPromptIncludesIdentityFirst(t *testing.T) {
	runner := &Runner{cfg: RunnerConfig{
		SystemPrompt: "You are Ceraa.",
		Model:        llm.ProviderConfig{Model: "gpt-5.6-luna", BaseURL: "https://example.test"},
	}}
	prompt := runner.systemPrompt()
	identityAt := strings.Index(prompt, "gpt-5.6-luna")
	vendoredAt := strings.Index(prompt, "You are Ceraa.")
	if identityAt < 0 || vendoredAt < 0 {
		t.Fatalf("prompt missing a part: %q", prompt)
	}
	if identityAt > vendoredAt {
		t.Errorf("identity appears after the vendored prompt:\n%s", prompt)
	}
}

func TestProviderHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://chatgpt.com/backend-api/codex", "chatgpt.com"},
		{"https://generativelanguage.googleapis.com/v1beta", "generativelanguage.googleapis.com"},
		{"http://127.0.0.1:8099/v1", "127.0.0.1:8099"},
		{"", ""},
		{"   ", ""},
	} {
		if got := providerHost(tc.in); got != tc.want {
			t.Errorf("providerHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The prompt used to carry a literal "This is year 2026", which is correct
// exactly until 1 January and silently wrong after that.
func TestCurrentDateIsInjectedNotHardcoded(t *testing.T) {
	// Read the file by path, not through prompt.System: that is resolved
	// relative to the working directory, so from this package it silently falls
	// back and the assertion would pass against text the model never sees.
	raw, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	if strings.Contains(string(raw), "This is year") {
		t.Error("the prompt file still hardcodes a year; it will be wrong after 31 December")
	}
	context := todayContext()
	if !strings.Contains(context, time.Now().Format("2006-01-02")) {
		t.Errorf("todayContext() = %q, want today's date", context)
	}
	if !strings.HasPrefix(context, "<current-date>") || !strings.HasSuffix(strings.TrimSpace(context), "</current-date>") {
		t.Errorf("todayContext() is not wrapped in a tag: %q", context)
	}
}

// The date must sit after the static prompt. Prefix caching is positional, so a
// fact at the front would invalidate the whole prefix every time the day rolls.
func TestCurrentDateSitsAfterTheStaticPrompt(t *testing.T) {
	runner := &Runner{cfg: RunnerConfig{SystemPrompt: "STATIC"}}
	prompt := runner.systemPrompt()
	staticAt := strings.Index(prompt, "STATIC")
	dateAt := strings.Index(prompt, "<current-date>")
	if staticAt < 0 || dateAt < 0 {
		t.Fatalf("prompt is missing a part: %q", prompt)
	}
	if dateAt < staticAt {
		t.Errorf("the date precedes the static prompt, so a daily rollover would invalidate everything:\n%s", prompt)
	}
}

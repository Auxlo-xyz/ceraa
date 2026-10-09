package coordinator

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/compaction"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

func liveRequestTestRunner(t *testing.T) (*Runner, *sessionstore.Session) {
	t.Helper()
	cfg := RunnerConfig{
		SessionID:    "live",
		SystemPrompt: "you are a test agent",
		Model:        llm.FromProvider("http://127.0.0.1:1", "k", "m"),
		ContextLimit: 32000,
		WorkDir:      t.TempDir(),
	}
	reg := tool.DefaultRegistry()
	runner := New(cfg, nil, reg)
	sess := &sessionstore.Session{
		ID: "live",
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "hello"},
		},
	}
	return runner, sess
}

// The live request is the turn's own bytes: the system prompt first, the
// history after it, the whole tool catalogue beside them.
func TestLiveRequestCarriesTheTurnRequest(t *testing.T) {
	runner, sess := liveRequestTestRunner(t)
	got := runner.LiveRequest(sess)
	if len(got.Messages) < 2 {
		t.Fatalf("got %d messages, want at least system and user", len(got.Messages))
	}
	if first := got.Messages[0]; first.Role != llm.RoleSystem || !strings.Contains(first.Text, "you are a test agent") {
		t.Errorf("first item is not the system prompt: %+v", first)
	}
	if last := got.Messages[len(got.Messages)-1]; last.Role != llm.RoleUser || last.Text != "hello" {
		t.Errorf("last item is not the live turn: %+v", last)
	}
	if len(got.Tools) == 0 {
		t.Error("the request carries no tools, so the catalogue is cold")
	}
	if got.SessionID != "live" {
		t.Errorf("session = %q, want live", got.SessionID)
	}
}

// LiveRequestFor exists so a caller with no runner produces the same request.
// This pins the promise: same config, same registry, same bytes.
func TestLiveRequestForMatchesTheRunner(t *testing.T) {
	runner, sess := liveRequestTestRunner(t)
	fromRunner := runner.LiveRequest(sess)
	fromHelper := LiveRequestFor(runner.cfg, runner.reg, nil)(context.Background(), sess)
	if len(fromHelper.Messages) != len(fromRunner.Messages) {
		t.Fatalf("helper built %d messages, the runner %d", len(fromHelper.Messages), len(fromRunner.Messages))
	}
	for i := range fromRunner.Messages {
		a, b := fromRunner.Messages[i], fromHelper.Messages[i]
		if a.Kind != b.Kind || a.Role != b.Role || a.Text != b.Text {
			t.Errorf("message %d differs: %+v vs %+v", i, a, b)
		}
	}
	if len(fromHelper.Tools) != len(fromRunner.Tools) {
		t.Fatalf("helper lists %d tools, the runner %d", len(fromHelper.Tools), len(fromRunner.Tools))
	}
	for i := range fromRunner.Tools {
		if fromRunner.Tools[i].Name != fromHelper.Tools[i].Name {
			t.Errorf("tool %d differs: %q vs %q", i, fromRunner.Tools[i].Name, fromHelper.Tools[i].Name)
		}
	}
}

// attachLiveRequest installs the runner's own assembly on the compactor it was
// given, and never overwrites a builder the caller installed deliberately.
func TestAttachLiveRequestInstallsOnce(t *testing.T) {
	runner, sess := liveRequestTestRunner(t)
	if runner.cfg.Compactor != nil {
		t.Fatal("test fixture leaked a compactor into the runner config")
	}
	called := false
	custom := func(context.Context, *sessionstore.Session) llm.StreamRequest {
		called = true
		return llm.StreamRequest{}
	}
	runner.cfg.Compactor = &compaction.Compactor{Settings: compaction.DefaultSettings()}
	runner.attachLiveRequest()
	if runner.cfg.Compactor.LiveRequest == nil {
		t.Fatal("no builder was installed")
	}
	request := runner.cfg.Compactor.LiveRequest(context.Background(), sess)
	if len(request.Messages) == 0 {
		t.Fatal("the installed builder returns nothing")
	}
	runner.cfg.Compactor.LiveRequest = custom
	runner.attachLiveRequest()
	runner.cfg.Compactor.LiveRequest(context.Background(), sess)
	if !called {
		t.Fatal("the second install overwrote a caller-installed builder")
	}
}

// The spool defaults beside the sessions, not in the OS temp dir: a pointer
// to a spool file is a promise it survives a reboot.
func TestSpoolDefaultsBesideTheSessions(t *testing.T) {
	if got := defaultToolOutputDir("/data/sessions"); got != "/data/sessions/tool-output" {
		t.Errorf("spool dir = %q, want it beside the sessions", got)
	}
	dir := t.TempDir()
	r := New(RunnerConfig{GoalDir: dir}, nil, tool.NewRegistry())
	if want := dir + "/tool-output"; r.cfg.ToolOutputDir != want {
		t.Errorf("spool dir = %q, want %q", r.cfg.ToolOutputDir, want)
	}
}

// The configured level is clamped against the vocabulary the catalog resolved,
// once, so every request the runner makes carries a sendable level. A model
// whose vocabulary is max/high/low must never be sent medium.
func TestRunnerClampsTheReasoningLevel(t *testing.T) {
	reg := tool.NewRegistry()
	runner := New(RunnerConfig{
		SessionID:        "clamp",
		SystemPrompt:     "system",
		Model:            llm.FromProvider("https://x.invalid/v1", "k", "m"),
		Reasoning:        llm.ReasonMedium,
		ReasoningEfforts: []string{"max", "high", "low"},
		ReasoningDefault: "high",
	}, nil, reg)
	if runner.cfg.Reasoning != llm.ReasonHigh {
		t.Fatalf("reasoning = %q, want high", runner.cfg.Reasoning)
	}
}

// With no resolved vocabulary the conservative mapping stands, which is the
// behaviour the transport had before the catalog was parsed.
func TestRunnerKeepsTheTopTierWhenUnresolved(t *testing.T) {
	reg := tool.NewRegistry()
	runner := New(RunnerConfig{
		SessionID:    "clamp",
		SystemPrompt: "system",
		Model:        llm.FromProvider("https://x.invalid/v1", "k", "m"),
		Reasoning:    llm.ReasonMax,
	}, nil, reg)
	if runner.cfg.Reasoning != llm.ReasonHigh {
		t.Fatalf("reasoning = %q, want high", runner.cfg.Reasoning)
	}
}

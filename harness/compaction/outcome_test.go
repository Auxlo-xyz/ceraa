package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// The compactor used to log nothing at all, and four of its exits returned the
// same (false, nil), so a fold that spent tens of thousands of input tokens and
// left no summary was indistinguishable from no fold. That is how a 28-second
// compaction at 15:32 went unexamined. These cover the two instruments that
// replace that silence: a named outcome, and one log line per fold.

// captureFoldLog records what the compactor would have sent to the log.
func captureFoldLog(c *Compactor) *[]string {
	lines := []string{}
	c.Logf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return &lines
}

func TestAFoldThatChangesNothingIsNamedAndLogged(t *testing.T) {
	c := New(llm.ProviderConfig{})
	sess := &sessionstore.Session{
		ID:       "outcome-nothing-to-fold",
		Messages: []sessionstore.Message{{Role: llm.RoleUser, Content: "one short line"}},
	}
	lines := captureFoldLog(c)

	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if ok {
		t.Error("a session with nothing to fold reported a fold")
	}
	if c.LastOutcome != FoldNothingToFold {
		t.Errorf("outcome = %q, want %q", c.LastOutcome, FoldNothingToFold)
	}
	if len(*lines) != 1 {
		t.Fatalf("logged %d lines, want exactly one: %v", len(*lines), *lines)
	}
	if !strings.Contains((*lines)[0], string(FoldNothingToFold)) {
		t.Errorf("the log line does not name the outcome: %q", (*lines)[0])
	}
}

func TestTheCooldownIsNamedRatherThanSilent(t *testing.T) {
	c := New(llm.ProviderConfig{})
	// A previous summary failed, so this attempt is refused before any work.
	c.noteSummaryFailure()
	lines := captureFoldLog(c)

	ok, err := c.Compact(context.Background(), compactionTestSession())
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if ok {
		t.Error("a refused attempt reported a fold")
	}
	if c.LastOutcome != FoldCooldown {
		t.Errorf("outcome = %q, want %q", c.LastOutcome, FoldCooldown)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], string(FoldCooldown)) {
		t.Errorf("the cooldown was not reported on the log bus: %v", *lines)
	}
}

func TestAnEmptySummaryIsReportedAsAFailureWithItsReason(t *testing.T) {
	srv := newSummaryServer(t, "")
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hi"}})
	lines := captureFoldLog(c)

	ok, err := c.Compact(context.Background(), compactionTestSession())
	if err == nil {
		t.Fatal("an empty summary was reported as success")
	}
	if ok {
		t.Error("an empty summary reported a fold")
	}
	// summarise validates its own output, so empty text arrives as an error
	// rather than an empty string. That is the useful shape: the reason reaches
	// the log bus instead of the fold vanishing, which is what happened to the
	// compaction at 15:32.
	if c.LastOutcome != FoldFailed {
		t.Errorf("outcome = %q, want %q", c.LastOutcome, FoldFailed)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], string(FoldFailed)) {
		t.Fatalf("the failure was not reported on the log bus: %v", *lines)
	}
	if !strings.Contains((*lines)[0], err.Error()) {
		t.Errorf("the log line does not carry the reason %q: %q", err.Error(), (*lines)[0])
	}
}

func TestASuccessfulFoldIsNamedAndItsCostReported(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	c := inBandCompactor(srv.server, []llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "fix the index"},
		{Kind: "text", Role: llm.RoleAssistant, Text: "on it"},
	})
	lines := captureFoldLog(c)

	ok, err := c.Compact(context.Background(), compactionTestSession())
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	if c.LastOutcome != FoldDone {
		t.Errorf("outcome = %q, want %q", c.LastOutcome, FoldDone)
	}
	if len(*lines) != 1 {
		t.Fatalf("logged %d lines, want exactly one: %v", len(*lines), *lines)
	}
	line := (*lines)[0]
	for _, want := range []string{"outcome=folded", "summary_chars=", "prompt_tokens=", "messages="} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line is missing %q: %q", want, line)
		}
	}
	// A fold that ran has spent tokens, so the line must show a summary was
	// actually kept rather than reporting a fold that kept nothing.
	if strings.Contains(line, "summary_chars=0 ") {
		t.Errorf("a completed fold reported a zero-length summary: %q", line)
	}
}

func TestAFoldThatLogsIsTheOnlyOneThatSpeaks(t *testing.T) {
	// A compactor nobody wired a logger into must stay silent rather than panic,
	// because the hook is optional and most callers have nowhere to log.
	c := New(llm.ProviderConfig{})
	if c.Logf != nil {
		t.Fatal("a fresh compactor came with a logger set")
	}
	if _, err := c.Compact(context.Background(), compactionTestSession()); err != nil {
		t.Fatalf("compact with no logger: %v", err)
	}
}

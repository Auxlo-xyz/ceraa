package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// runLoop drives a loop over scripted input and returns what it printed.
//
// The session is not used, because the slash commands must work before a run
// happens and a command must never depend on a provider being configured.
func runLoop(t *testing.T, in string) string {
	t.Helper()
	var out bytes.Buffer
	// The loop writes its own output; the commands that need a session are the only
	// ones that touch it, and those are covered separately.
	l := &Loop{
		render:  NewRenderer(&out, Plain()),
		in:      strings.NewReader(in),
		out:     &out,
		pal:     Plain(),
		version: "test",
	}
	// nil session: only the commands that do not need a provider may run.
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop returned %v", err)
	}
	return out.String()
}

func TestLoopPrintsBanner(t *testing.T) {
	got := runLoop(t, "")
	if !strings.Contains(got, "Ceraa") {
		t.Errorf("no banner in output:\n%s", got)
	}
}

func TestLoopHelpListsCommands(t *testing.T) {
	got := runLoop(t, "/help\n")
	for _, want := range []string{"/help", "/status", "/sessions", "/new", "/model", "/exit"} {
		if !strings.Contains(got, want) {
			t.Errorf("help is missing %q:\n%s", want, got)
		}
	}
}

func TestLoopExitEnds(t *testing.T) {
	got := runLoop(t, "/exit\n")
	if !strings.Contains(got, "bye") {
		t.Errorf("no farewell on exit:\n%s", got)
	}
}

func TestLoopQuitEnds(t *testing.T) {
	got := runLoop(t, "/quit\n")
	if !strings.Contains(got, "bye") {
		t.Errorf("/quit did not end the loop:\n%s", got)
	}
}

func TestLoopUnknownSlashIsNotSentToTheModel(t *testing.T) {
	// Spending a real inference call on a mistyped command is the bug the
	// near-miss guard in the root command already exists to prevent.
	got := runLoop(t, "/nope\n")
	if !strings.Contains(got, "unknown command") {
		t.Errorf("an unknown command was not reported:\n%s", got)
	}
	if !strings.Contains(got, "/help") {
		t.Errorf("the report should point at /help:\n%s", got)
	}
}

func TestLoopIgnoresBlankLines(t *testing.T) {
	got := runLoop(t, "\n\n\n/help\n")
	if strings.Contains(got, "unknown command") {
		t.Errorf("a blank line was treated as a command:\n%s", got)
	}
}

func TestNilSessionAnswersEmptyRatherThanPanicking(t *testing.T) {
	// The loop prints the session id in its prompt and its exit line, so a nil
	// session has to be readable or a command can take the process down.
	var s *Session
	if got := s.SessionID(); got != "" {
		t.Errorf("SessionID on nil = %q", got)
	}
	if got := s.WorkDir(); got != "" {
		t.Errorf("WorkDir on nil = %q", got)
	}
	if s.Store() != nil {
		t.Error("Store on nil returned a store")
	}
	if p, m := s.Model(); p != "" || m != "" {
		t.Errorf("Model on nil = %q,%q", p, m)
	}
}

func TestPadDoesNotTruncate(t *testing.T) {
	if got := pad("ab", 5); got != "ab   " {
		t.Errorf("pad = %q, want padding to 5", got)
	}
	if got := pad("abcdef", 3); got != "abcdef" {
		t.Errorf("pad truncated a long value: %q", got)
	}
}

func TestIsCanceledRecognisesCancellation(t *testing.T) {
	if !isCanceled(context.Canceled) {
		t.Error("context.Canceled not recognised")
	}
	if isCanceled(nil) {
		t.Error("nil treated as cancellation")
	}
	if isCanceled(context.DeadlineExceeded) {
		t.Error("a deadline was treated as a cancellation")
	}
}

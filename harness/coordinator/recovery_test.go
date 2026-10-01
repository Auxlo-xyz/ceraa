package coordinator

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// An assistant message that declares a tool call with no matching tool result is
// a request providers reject. So the entry is not optional: without it one lost
// turn makes every request after it fail too.
func TestAppendResultsAnswersEveryDeclaredCall(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{
			{ID: "a", Name: "bash"}, {ID: "b", Name: "read"},
		}},
	}}
	calls := []llm.StreamedToolCall{
		{ID: "a", Name: "bash"},
		{ID: "b", Name: "read"},
	}
	// Only one of the two was reported.
	(&Runner{}).appendResults(sess, calls, map[string]string{"a": "done"})

	tool := 0
	for _, m := range sess.Messages {
		if m.Role == llm.RoleTool {
			tool++
		}
	}
	if tool != 2 {
		t.Fatalf("got %d tool results for 2 declared calls: %+v", tool, sess.Messages)
	}
	seen := map[string]bool{}
	for _, m := range sess.Messages {
		if m.Role == llm.RoleTool {
			if seen[m.ToolCallID] {
				t.Errorf("call %s answered twice", m.ToolCallID)
			}
			seen[m.ToolCallID] = true
		}
	}
	for _, id := range []string{"a", "b"} {
		if !seen[id] {
			t.Errorf("call %s is unanswered", id)
		}
	}
}

// Repeating a command whose outcome is unknown is how a file gets written twice.
// The text has to tell the model that, and the two cases are not the same.
func TestRecoveryTextDistinguishesStartedFromNotStarted(t *testing.T) {
	notStarted := recoveryResult("bash", false)
	if !strings.Contains(notStarted, "before it was started") {
		t.Errorf("a call that never started is not marked as such: %q", notStarted)
	}

	readOnly := recoveryResult("read", true)
	if !strings.Contains(readOnly, "only reads") {
		t.Errorf("a read-only tool is not told retrying is safe: %q", readOnly)
	}

	mutating := recoveryResult("bash", true)
	if !strings.Contains(mutating, "unknown") {
		t.Errorf("an interrupted call does not say the outcome is unknown: %q", mutating)
	}
	if !strings.Contains(mutating, "check the affected state") {
		t.Errorf("a mutating tool is not told to check before retrying: %q", mutating)
	}
	if strings.Contains(mutating, "safe") {
		t.Errorf("a mutating tool was told a retry is safe: %q", mutating)
	}
}

// A call with no operation record cannot have taken effect, which is the safe
// direction to be wrong in.
func TestCallStartedIsFalseWithoutARecord(t *testing.T) {
	sess := &sessionstore.Session{}
	if callStarted(sess, "nope") {
		t.Error("a call with no operation record was reported as started")
	}
	sess.Operations = []sessionstore.Operation{
		{ID: "pending", Status: sessionstore.StatusPending},
		{ID: "running", Status: sessionstore.StatusRunning},
		{ID: "done", Status: sessionstore.StatusCompleted},
	}
	for id, want := range map[string]bool{"pending": false, "running": true, "done": true} {
		if got := callStarted(sess, id); got != want {
			t.Errorf("callStarted(%s) = %v, want %v", id, got, want)
		}
	}
}

// Already-answered calls must not get a second entry. A duplicate for one call
// id is what the original skip was avoiding.
func TestRecoverUnansweredSkipsCallsThatAlreadyHaveAResult(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{{ID: "a", Name: "bash"}}},
		{Role: llm.RoleTool, ToolCallID: "a", ToolResult: "done", ToolResultPresent: true},
	}}
	got := recoveryMessages(sess, []llm.StreamedToolCall{{ID: "a", Name: "bash"}})
	if len(got) != 0 {
		t.Errorf("an answered call got a recovery entry: %+v", got)
	}
}

func TestRecoverUnansweredCoversOnlyTheGaps(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleAssistant, ToolCalls: []sessionstore.ToolCallRef{
			{ID: "a", Name: "bash"}, {ID: "b", Name: "read"},
		}},
		{Role: llm.RoleTool, ToolCallID: "a", ToolResult: "done", ToolResultPresent: true},
	}}
	got := recoveryMessages(sess, []llm.StreamedToolCall{
		{ID: "a", Name: "bash"}, {ID: "b", Name: "read"},
	})
	if len(got) != 1 || got[0].ToolCallID != "b" {
		t.Fatalf("got %d recovery entries, want only b: %+v", len(got), got)
	}
	if !got[0].ToolResultPresent {
		t.Error("the recovery entry is not marked present, so it would be dropped as empty")
	}
	if got[0].ResultStatus != sessionstore.StatusUnknown {
		t.Errorf("status is %q, want unknown", got[0].ResultStatus)
	}
}

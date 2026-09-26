package coordinator

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// A stored tool result is trimmed when a request is rendered. The provider
// reuses a cached prefix only if the bytes it receives are identical, so the
// trimming has to be a pure function of the stored text: the same session
// rendered twice must produce byte-identical request messages.
//
// The old behaviour was not. It rewrote the stored result in place whenever a
// running tool-output budget was exceeded, so the same message rendered
// differently at different points in one session, and the change was persisted.
func TestRenderIsByteIdenticalAcrossRepeatedCalls(t *testing.T) {
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleUser, Content: "run the tests"},
		{Role: llm.RoleTool, ToolResult: strings.Repeat("tool output ", 900),
			ToolResultPresent: true, ToolCallID: "c1", ToolName: "bash"},
		{Role: llm.RoleAssistant, Content: "done"},
	}}

	first := renderForTest(sess)
	for i := 0; i < 50; i++ {
		again := renderForTest(sess)
		if len(first) != len(again) {
			t.Fatalf("render %d produced %d messages, first produced %d", i, len(again), len(first))
		}
		for j := range first {
			if first[j].Text != again[j].Text || first[j].Output != again[j].Output {
				t.Fatalf("render %d differs at message %d:\n%q / %q\n%q / %q",
					i, j, first[j].Text, first[j].Output, again[j].Text, again[j].Output)
			}
		}
	}
}

// The trimmed output must be the same on every render, which is what makes the
// prefix reusable. A cumulative budget would trim a given message only after
// enough other output accumulated, so the sent bytes would change mid-session.
func TestLongToolOutputIsTrimmedIdenticallyEveryTime(t *testing.T) {
	long := strings.Repeat("z", 9000)
	sess := &sessionstore.Session{Messages: []sessionstore.Message{
		{Role: llm.RoleTool, ToolResult: long, ToolResultPresent: true, ToolCallID: "c1"},
	}}
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		rendered := renderForTest(sess)
		seen[rendered[0].Output] = true
	}
	if len(seen) != 1 {
		t.Errorf("the same stored result rendered %d different ways, so the prefix would change mid-session", len(seen))
	}
	// And the store is still whole afterwards.
	if sess.Messages[0].ToolResult != long {
		t.Error("the stored result was modified by rendering")
	}
}

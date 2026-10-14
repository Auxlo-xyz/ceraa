package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// validTestSummary carries every required heading. Built from the template's
// own list rather than restated, so it tracks new sections on its own.
func validTestSummary() string {
	var b strings.Builder
	for _, heading := range requiredSummaryHeadings() {
		fmt.Fprintf(&b, "## %s\nA body line for %s.\n", heading, heading)
	}
	return b.String()
}

func TestTemplateRequiresTheWorkStateSections(t *testing.T) {
	required := requiredSummaryHeadings()
	want := []string{"Objective", "Progress", "In-flight & Blocked", "Errors & Fixes", "User's consistent remarks", "Relevant Files", "Decisions", "Next Steps", "Handoff", "Design Lessons Learnt"}
	if len(required) != len(want) {
		t.Fatalf("template has %d headings, want %d: %v", len(required), len(want), required)
	}
	for i, heading := range want {
		if required[i] != heading {
			t.Errorf("heading %d = %q, want %q", i, required[i], heading)
		}
	}
}

func TestValidateSummaryAcceptsACompleteSummary(t *testing.T) {
	if err := validateSummary(validTestSummary()); err != nil {
		t.Errorf("a complete summary was rejected: %v", err)
	}
}

func TestValidateSummaryRejectsABrokenOne(t *testing.T) {
	headings := requiredSummaryHeadings()
	for _, tc := range []struct {
		name string
		make func() string
	}{
		{"missing section", func() string {
			var b strings.Builder
			for _, h := range headings[1:] {
				fmt.Fprintf(&b, "## %s\nBody.\n", h)
			}
			return b.String()
		}},
		{"out of order", func() string {
			var b strings.Builder
			for i := len(headings) - 1; i >= 0; i-- {
				fmt.Fprintf(&b, "## %s\nBody.\n", headings[i])
			}
			return b.String()
		}},
		{"empty section", func() string {
			var b strings.Builder
			for i, h := range headings {
				fmt.Fprintf(&b, "## %s\n", h)
				if i != 2 {
					b.WriteString("Body.\n")
				}
			}
			return b.String()
		}},
		{"one-line stub", func() string { return "Looks good, moving on." }},
		{"nothing", func() string { return "   " }},
	} {
		if err := validateSummary(tc.make()); err == nil {
			t.Errorf("%s: a broken summary passed validation", tc.name)
		}
	}
}

// summaryServer serves SSE content replies and records every request body. A
// reply starting with "TOOL:" is sent as a tool call instead of text.
type summaryServer struct {
	t       *testing.T
	replies []string
	bodies  []map[string]any
	server  *httptest.Server
}

func newSummaryServer(t *testing.T, replies ...string) *summaryServer {
	s := &summaryServer{t: t, replies: append([]string{}, replies...)}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		s.bodies = append(s.bodies, decoded)
		reply := ""
		if len(s.replies) > 0 {
			reply, s.replies = s.replies[0], s.replies[1:]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasPrefix(reply, "TOOL:") {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"%s\",\"arguments\":\"{}\"}}]}}]}\n\n",
				reply[len("TOOL:"):])
		} else {
			encoded, _ := json.Marshal(reply)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", encoded)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	return s
}

func (s *summaryServer) close() { s.server.Close() }

func (s *summaryServer) lastBody(t *testing.T) map[string]any {
	t.Helper()
	if len(s.bodies) == 0 {
		t.Fatal("no request reached the server")
	}
	return s.bodies[len(s.bodies)-1]
}

func inBandCompactor(server *httptest.Server, live []llm.Item) *Compactor {
	c := &Compactor{
		LLM:          llm.ProviderConfig{BaseURL: server.URL, APIKey: "k", Model: "m"},
		Settings:     DefaultSettings(),
		ContextLimit: 32000,
	}
	c.Settings.Keep.Tokens = 100
	c.LiveRequest = func(context.Context, *sessionstore.Session) llm.StreamRequest {
		return llm.StreamRequest{
			Messages: live,
			Tools:    []llm.ToolDef{{Name: "read", Description: "read a file", Parameters: map[string]any{"type": "object"}}},
		}
	}
	return c
}

func compactionTestSession() *sessionstore.Session {
	return &sessionstore.Session{
		ID: "tg-test",
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "fix the index"},
			{Role: llm.RoleAssistant, Content: "on it", ToolCalls: []sessionstore.ToolCallRef{{ID: "c1", Name: "read", Args: `{"path":"/repo/types.ts"}`}}},
			{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "read", ToolResult: strings.Repeat("a matching line\n", 60)},
			{Role: llm.RoleUser, Content: "and now write the tests"},
			{Role: llm.RoleAssistant, Content: "tests written, moving on"},
		},
	}
}

func TestInBandCompactionSummarisesOverTheLiveRequest(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	sess := compactionTestSession()
	live := []llm.Item{
		{Kind: "text", Role: llm.RoleUser, Text: "fix the index"},
		{Kind: "text", Role: llm.RoleAssistant, Text: "on it"},
	}
	c := inBandCompactor(srv.server, live)
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	body := srv.lastBody(t)
	if _, hasTools := body["tools"]; !hasTools {
		t.Error("the summary request carries no tools, so its cache prefix is cold")
	}
	if body["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want none", body["tool_choice"])
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("the request carries no messages")
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	content, _ := last["content"].(string)
	for _, section := range []string{"## In-flight & Blocked", "## Errors & Fixes", "## Handoff"} {
		if !strings.Contains(content, section) {
			t.Errorf("the appended instruction does not carry %q: %.120q", section, content)
		}
	}
	found := false
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			found = true
			if err := validateSummary(message.Compaction.Summary); err != nil {
				t.Errorf("the stored checkpoint fails validation: %v", err)
			}
		}
	}
	if !found {
		t.Error("no checkpoint was written to the session")
	}
	for _, message := range sess.Messages {
		if strings.Contains(message.Content, "write it as yourself, to yourself") {
			t.Error("the instruction leaked into the session transcript")
		}
	}
}

func TestInvalidSummariesAreRefusedWithNothingWritten(t *testing.T) {
	srv := newSummaryServer(t, "Looks good, moving on.", "Still not a summary.")
	defer srv.close()
	sess := compactionTestSession()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}})
	if _, err := c.Compact(context.Background(), sess); err == nil {
		t.Fatal("two invalid summaries were accepted as a checkpoint")
	}
	if len(srv.bodies) != summaryAttempts {
		t.Fatalf("want exactly %d attempts, got %d requests", summaryAttempts, len(srv.bodies))
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			t.Fatal("a failing summary was still written as the checkpoint")
		}
	}
}

func TestInBandCompactionRetriesAToolCallAndKeepsGoing(t *testing.T) {
	srv := newSummaryServer(t, "TOOL:read", validTestSummary())
	defer srv.close()
	sess := compactionTestSession()
	c := inBandCompactor(srv.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}})
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	if len(srv.bodies) != 2 {
		t.Fatalf("want a failed attempt and a retry, got %d requests", len(srv.bodies))
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil && message.Compaction.Summary != validTestSummary() {
			t.Errorf("the checkpoint is not the retried summary: %.80q", message.Compaction.Summary)
		}
	}
}

func TestInBandCompactionIsRefusedWhenTheContextCannotCarryIt(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	// A history the live request already fills: the instruction and the
	// summary's own output budget no longer fit beside it. There is no smaller
	// request left to try, so the compaction fails honestly instead of faking a
	// checkpoint from a conversation that cannot be sent. This is also the
	// overflow trigger's shape.
	sess := compactionTestSession()
	sess.Messages[2].ToolResult = strings.Repeat("a matching line with a long tail to pad tokens\n", 400)
	live := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}}
	c := inBandCompactor(srv.server, live)
	c.ContextLimit = 7000
	if _, err := c.Compact(context.Background(), sess); err == nil {
		t.Fatal("an impossible compaction succeeded")
	}
	if len(srv.bodies) != 0 {
		t.Fatalf("a request went out that could not fit the window: %d requests", len(srv.bodies))
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			t.Fatal("a checkpoint was written without a summary behind it")
		}
	}
}

func TestCompactionWithoutALiveRequestIsAnError(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	sess := compactionTestSession()
	c := &Compactor{
		LLM:          llm.ProviderConfig{BaseURL: srv.server.URL, APIKey: "k", Model: "m"},
		Settings:     DefaultSettings(),
		ContextLimit: 32000,
	}
	c.Settings.Keep.Tokens = 100
	if _, err := c.Compact(context.Background(), sess); err == nil {
		t.Fatal("a compaction with no request to summarise over succeeded")
	}
	if len(srv.bodies) != 0 {
		t.Fatal("a request went out with no live request behind it")
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			t.Fatal("a checkpoint was written without a summary behind it")
		}
	}
}

// A giant turn near the tail used to collapse the verbatim tail to the small
// turns after it: the budget blew on the giant unit and the tear fell between
// whole turns. The rule is now structural, the last two exchanges, so the
// outcome is the same here but deliberate: the two orphans genuinely are the
// entire open past, and the giant turn is resolved work the summary carries.
func TestRecentKeepsTheLastTwoExchangesDeliberately(t *testing.T) {
	messages := []sessionstore.Message{
		{Role: llm.RoleUser, Content: "first task"},
		{Role: llm.RoleAssistant, Content: "done", ToolCalls: []sessionstore.ToolCallRef{{ID: "c0", Name: "read", Args: `{}`}}},
		{Role: llm.RoleTool, ToolCallID: "c0", ToolName: "read", ToolResult: "ok"},
		{Role: llm.RoleUser, Content: "the giant turn " + strings.Repeat("x", 40000)},
		{Role: llm.RoleAssistant, Content: "handled", ToolCalls: []sessionstore.ToolCallRef{{ID: "c1", Name: "bash", Args: `{}`}}},
		{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "bash", ToolResult: "done"},
		{Role: llm.RoleUser, Content: "Continue"},
		{Role: llm.RoleUser, Content: "Gg"},
	}
	head, recent := selectMessageUnits(messages, 8000, true)
	if len(recent) != 2 || recent[0].Content != "Continue" || recent[1].Content != "Gg" {
		t.Fatalf("recent is not the last two exchanges: %+v", recent)
	}
	if len(head) != 6 {
		t.Fatalf("head holds %d messages, want the 6 before the orphans", len(head))
	}
}

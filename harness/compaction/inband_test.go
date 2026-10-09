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
	want := []string{"Objective", "Progress", "In-flight & Blocked", "Errors & Fixes", "Relevant Files", "Decisions", "Next Steps", "Design Lessons Learnt"}
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
	if !strings.Contains(content, "## In-flight & Blocked") || !strings.Contains(content, "## Errors & Fixes") {
		t.Errorf("the appended instruction does not carry the new sections: %.120q", content)
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
		if strings.Contains(message.Content, "Produce a compaction of this conversation") {
			t.Error("the instruction leaked into the session transcript")
		}
	}
}

func TestInBandCompactionFallsBackToColdOnAnInvalidSummary(t *testing.T) {
	// The first reply answers a request carrying tools, which is the in-band
	// one, with a stub. The second carries none, which is the cold fallback,
	// and gets the real summary.
	s := &summaryServer{t: t}
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
		reply := validTestSummary()
		if _, hasTools := decoded["tools"]; hasTools {
			reply = "Looks good, moving on."
		}
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(reply)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", encoded)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer s.close()
	sess := compactionTestSession()
	c := inBandCompactor(s.server, []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}})
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	if len(s.bodies) < 3 {
		t.Fatalf("want at least two in-band attempts plus the cold fallback, got %d requests", len(s.bodies))
	}
	last := s.bodies[len(s.bodies)-1]
	if _, hasTools := last["tools"]; hasTools {
		t.Error("the succeeding request carries tools: the cold fallback never ran")
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil && message.Compaction.Summary != validTestSummary() {
			t.Errorf("the checkpoint is not the cold summary: %.80q", message.Compaction.Summary)
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

func TestInBandCompactionIsSkippedWhenTheContextCannotCarryIt(t *testing.T) {
	srv := newSummaryServer(t, validTestSummary())
	defer srv.close()
	// A history the live request already fills: the instruction and the
	// summary's own output budget no longer fit beside it, so the in-band
	// attempt is declined and the cold one - smaller, because it truncates -
	// serves it instead. This is also the overflow trigger's shape.
	sess := compactionTestSession()
	sess.Messages[2].ToolResult = strings.Repeat("a matching line with a long tail to pad tokens\n", 400)
	live := []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "fix the index"}}
	c := inBandCompactor(srv.server, live)
	c.ContextLimit = 7000
	ok, err := c.Compact(context.Background(), sess)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !ok {
		t.Fatal("nothing was compacted")
	}
	for _, body := range srv.bodies {
		if _, hasTools := body["tools"]; hasTools {
			t.Fatal("an in-band request went out that could not fit the window")
		}
	}
}

func TestColdCompactionAlsoValidates(t *testing.T) {
	srv := newSummaryServer(t, "Looks good, moving on.")
	defer srv.close()
	sess := compactionTestSession()
	c := &Compactor{
		LLM:          llm.ProviderConfig{BaseURL: srv.server.URL, APIKey: "k", Model: "m"},
		Settings:     DefaultSettings(),
		ContextLimit: 32000,
	}
	c.Settings.Keep.Tokens = 100
	if _, err := c.Compact(context.Background(), sess); err == nil {
		t.Fatal("a stub was accepted as a checkpoint")
	}
	for _, message := range sess.Messages {
		if message.Compaction != nil {
			t.Fatal("a failing summary was still written as the checkpoint")
		}
	}
}

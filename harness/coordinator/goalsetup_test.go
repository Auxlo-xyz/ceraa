package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// goalSetupSeamRun drives a whole run against a fake provider that issues the
// given calls in order and then answers, and returns the tool results the model
// actually read plus the persisted session.
//
// It follows goalCloseSeamRun: the reminder is one more term in the sum at
// runner.go, so the assertion has to be about the bytes on the wire and the
// message the session keeps, not about the function that rendered it.
func goalSetupSeamRun(t *testing.T, sessionID string, calls []scriptedCall) ([]string, *sessionstore.Session) {
	t.Helper()
	work := t.TempDir()

	var (
		mu         sync.Mutex
		toolResult []string
		requests   atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, m := range body.Messages {
			if m.Role == "tool" {
				toolResult = append(toolResult, m.Content)
			}
		}
		mu.Unlock()

		var payload map[string]any
		i := int(requests.Add(1)) - 1
		if i < len(calls) {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"id":       fmt.Sprintf("call%d", i+1),
					"function": map[string]any{"name": calls[i].name, "arguments": calls[i].args},
				}},
			}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := RunnerConfig{
		SessionID:       sessionID,
		SystemPrompt:    "STATIC",
		Model:           llm.FromProvider(server.URL, "test", "test"),
		ModelCatalogURL: "http://127.0.0.1:1",
		WorkDir:         work,
		GoalDir:         t.TempDir(),
		// A goal created by one of these calls would be continued by the runner,
		// and a continued run never reaches the final answer these cases are
		// about. Goal continuation is covered by its own tests.
		StopWhenIdle: true,
		MaxRequests:  len(calls) + 2,
	}
	runner := New(config, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "do the task"); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), toolResult...), sess
}

// bashCalls is n identical trivial shell calls, spelled once so a case reads as
// its tool-call count rather than as a wall of JSON.
func bashCalls(n int) []scriptedCall {
	out := make([]scriptedCall, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, scriptedCall{name: "bash", args: `{"command":"true"}`})
	}
	return out
}

func toolMessageByCall(sess *sessionstore.Session, id string) *sessionstore.Message {
	for i := range sess.Messages {
		if sess.Messages[i].Role == llm.RoleTool && sess.Messages[i].ToolCallID == id {
			return &sess.Messages[i]
		}
	}
	return nil
}

// The seam: the sixth completed call is the one that carries the reminder, so
// the model reads it in the turn the work is still in hand. Five calls is not
// yet evidence of a multi-step task, and the seventh must not repeat the ask.
func TestTheSixthToolCallAsksForAGoal(t *testing.T) {
	results, sess := goalSetupSeamRun(t, "goal-setup", bashCalls(goalSetupAfter))

	carrying := 0
	for _, text := range results {
		if !strings.Contains(text, "<goal_reminder>") {
			continue
		}
		carrying++
		for _, want := range []string{"create_goal", "update_goal", "todos", "survives this run"} {
			if !strings.Contains(text, want) {
				t.Errorf("the reminder is missing %q: %q", want, text)
			}
		}
		// The tool's own answer has to survive: a reminder that replaces the
		// result hides what the command actually did.
		if !strings.Contains(text, "Full output") && strings.TrimSpace(text) == "" {
			t.Errorf("the reminder replaced the tool result: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no tool result the model reads carries the reminder: %#v", results)
	}

	sixth := toolMessageByCall(sess, fmt.Sprintf("call%d", goalSetupAfter))
	if sixth == nil {
		t.Fatalf("the sixth call left no tool message")
	}
	if !strings.Contains(sixth.ToolResult, "<goal_reminder>") {
		t.Errorf("the sixth call's result does not carry the reminder: %q", sixth.ToolResult)
	}
	for i := 1; i < goalSetupAfter; i++ {
		message := toolMessageByCall(sess, fmt.Sprintf("call%d", i))
		if message == nil {
			t.Fatalf("call %d left no tool message", i)
		}
		if strings.Contains(message.ToolResult, "<goal_reminder>") {
			t.Errorf("call %d fired the reminder before the line: %q", i, message.ToolResult)
		}
	}
}

func TestTheGoalReminderWaitsForTheSixthCall(t *testing.T) {
	_, sess := goalSetupSeamRun(t, "goal-setup-early", bashCalls(goalSetupAfter-1))
	for _, message := range sess.Messages {
		if message.Role == llm.RoleTool && strings.Contains(message.ToolResult, "<goal_reminder>") {
			t.Errorf("the reminder fired at %d calls: %q", goalSetupAfter-1, message.ToolResult)
		}
	}
}

// A goal that already exists must not be overwritten by a reminder written for
// a task that has none. Active is the common case in a continuation, and paused
// or blocked carry state the model wrote.
func TestTheGoalReminderStaysQuietWhenAGoalAlreadyExists(t *testing.T) {
	cases := []struct {
		name  string
		calls []scriptedCall
	}{
		{
			name:  "active goal",
			calls: append([]scriptedCall{createGoalCall}, bashCalls(goalSetupAfter-1)...),
		},
		{
			name:  "goal created as the sixth call",
			calls: append(bashCalls(goalSetupAfter-1), createGoalCall),
		},
		{
			name:  "goal closed as the sixth call",
			calls: append(append([]scriptedCall{createGoalCall}, bashCalls(goalSetupAfter-2)...), closeGoalCall),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sess := goalSetupSeamRun(t, "goal-setup-quiet", tc.calls)
			for _, message := range sess.Messages {
				if message.Role == llm.RoleTool && strings.Contains(message.ToolResult, "<goal_reminder>") {
					t.Errorf("the reminder fired with an existing goal: %q", message.ToolResult)
				}
			}
		})
	}
}

// The other half of the seam, same rule as goalCloseNote: the operation record
// keeps what the tool returned, so a later reader is not told which harness
// reminders happened to fire at the time.
func TestTheGoalReminderNeverReachesTheOperationRecord(t *testing.T) {
	_, sess := goalSetupSeamRun(t, "goal-setup-split", bashCalls(goalSetupAfter))

	reminders := 0
	for _, message := range sess.Messages {
		if message.Role == llm.RoleTool && strings.Contains(message.ToolResult, "<goal_reminder>") {
			reminders++
		}
	}
	if reminders != 1 {
		t.Fatalf("want exactly one reminder on the model path, got %d", reminders)
	}
	for _, op := range sess.Operations {
		if strings.Contains(op.Output, "<goal_reminder>") {
			t.Errorf("the reminder leaked into the stored operation record: %q", op.Output)
		}
	}
}

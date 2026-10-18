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

// scriptedCall is one tool call the fake provider will emit, one per request.
type scriptedCall struct {
	name string
	args string
}

// goalCloseSeamRun drives a whole run against a fake provider that issues the
// given calls in order and then answers, and returns the tool results the model
// actually read plus the persisted session.
//
// It follows instructionSeamRun deliberately: every claim below is about the
// bytes on the wire and about the session file, because goalCloseNote is one more
// term in the same sum at runner.go:1077, and that is the seam that can lose it.
func goalCloseSeamRun(t *testing.T, sessionID string, calls []scriptedCall) ([]string, *sessionstore.Session) {
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
		// GoalDir defaults to the relative "sessions", which resolves against
		// the package directory: two cases would then share one goal file and
		// the second would read a goal the first one wrote. A temp dir per run
		// is what makes each case start with no goal at all.
		GoalDir: t.TempDir(),
		// An active goal is continued by the runner, so a scripted run that
		// leaves one active never reaches its final "done" and is abandoned
		// after the budget instead. These runs are about one tool result, not
		// about continuation.
		StopWhenIdle: true,
		MaxRequests:  6,
	}
	runner := New(config, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "close the goal"); err != nil {
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

// createCall and the close call, spelled once so every case below reads as its
// intent rather than as a JSON blob.
var (
	createGoalCall = scriptedCall{name: "create_goal", args: `{"objective":"map the goal system"}`}
	closeGoalCall  = scriptedCall{name: "update_goal", args: `{"status":"completed","reason":"mapped it"}`}
	planOnlyCall   = scriptedCall{name: "update_goal", args: `{"todos":[{"content":"step one","status":"completed","priority":"high"}]}`}
	pauseGoalCall  = scriptedCall{name: "update_goal", args: `{"status":"paused","reason":"waiting on the owner"}`}
)

// The seam: closing a goal has to reach the model with the reminder attached to
// the very result that closed it, so it is read while the findings are still in
// context and before the answer that ends the run.
func TestClosingAGoalAsCompletedNudgesTheModelToRecordFindings(t *testing.T) {
	results, _ := goalCloseSeamRun(t, "goal-close", []scriptedCall{createGoalCall, closeGoalCall})

	carrying := 0
	for _, text := range results {
		if !strings.Contains(text, "<goal_complete>") {
			continue
		}
		carrying++
		for _, want := range []string{"Goal closed as completed", "create a new .md", "append to an existing one", "concise"} {
			if !strings.Contains(text, want) {
				t.Errorf("the nudge is missing %q: %q", want, text)
			}
		}
		// The tool's own answer has to survive: a reminder that replaces the
		// result hides the goal record the model just wrote.
		if !strings.Contains(text, `"status":"completed"`) {
			t.Errorf("the nudge replaced the tool result rather than accompanying it: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no tool result the model reads carries the nudge: %#v", results)
	}
}

// The other half of the same seam, and the reason the nudge lives in the runner
// rather than in the tool: the session record keeps what the tool returned, so a
// later reader is not told which reminders happened to fire at the time.
func TestTheGoalCloseNudgeNeverReachesTheSessionRecord(t *testing.T) {
	results, sess := goalCloseSeamRun(t, "goal-close-split", []scriptedCall{createGoalCall, closeGoalCall})

	recorded, bare := 0, 0
	for _, op := range sess.Operations {
		if op.Output == "" {
			continue
		}
		recorded++
		if strings.Contains(op.Output, "<goal_complete>") {
			t.Errorf("the nudge leaked into the stored operation record: %q", op.Output)
		}
		if op.SpecType == "update_goal" && strings.Contains(op.Output, `"status":"completed"`) {
			bare++
		}
	}
	if recorded == 0 {
		t.Fatal("no operation was recorded, so the split was not exercised")
	}
	if bare == 0 {
		t.Fatal("the closing operation kept no bare goal record, so the split proved nothing")
	}
	if len(results) == 0 {
		t.Fatal("no tool result reached the model, so the test proved nothing")
	}
}

// Only a completed close fires. Each of these is a call that must stay silent:
// creating a goal is not closing one, a plan-only update carries no status, a
// pause is not a completion, and a close that failed closed nothing.
func TestTheGoalCloseNudgeFiresOnNoOtherCall(t *testing.T) {
	cases := []struct {
		name  string
		calls []scriptedCall
	}{
		{"create only", []scriptedCall{createGoalCall}},
		{"plan-only update", []scriptedCall{createGoalCall, planOnlyCall}},
		{"paused instead of completed", []scriptedCall{createGoalCall, pauseGoalCall}},
		// No create, so runGoal answers "no persistent goal exists": the spec
		// says completed but the operation failed, and a failed close is not a
		// close.
		{"failed close", []scriptedCall{closeGoalCall}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, _ := goalCloseSeamRun(t, "goal-close-silent", tc.calls)
			if len(results) == 0 {
				t.Fatal("no tool result reached the model, so the silence proved nothing")
			}
			for _, text := range results {
				if strings.Contains(text, "<goal_complete>") {
					t.Errorf("the nudge fired on %q: %q", tc.name, text)
				}
			}
		})
	}
}

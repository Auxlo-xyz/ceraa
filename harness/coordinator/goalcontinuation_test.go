package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ceraa/harness/goal"
	"ceraa/harness/llm"
	"ceraa/harness/question"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// runGoalContinuation drives one whole run against a fake provider that answers
// every turn with a final answer and no tool calls, so the only thing that can
// end the run early is the goal continuation loop itself. The goal is seeded
// before the run; the returned goal is what was loaded back afterwards.
func runGoalContinuation(t *testing.T, sessionID string, seed goal.Goal, cfg func(*RunnerConfig)) (goal.Goal, *sessionstore.Session, Result) {
	t.Helper()
	goalDir := t.TempDir()
	gs, err := goal.New(goalDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed.CreatedAt, seed.UpdatedAt = now, now
	// The runner reads the owner's one goal, so the seed has to land on the
	// global key whatever the session id is; per-session records are dead keys
	// since the goal went global.
	if _, err := gs.Save(goal.GlobalKey, seed); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}},
			// Reported so the token budget can be exercised with real spend: an
			// unreported run leaves runUsage at zero and the fence never trips.
			"usage": map[string]any{"prompt_tokens": 60, "completion_tokens": 40, "total_tokens": 100},
		})
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
		SystemPrompt:    "test",
		Model:           llm.FromProvider(server.URL, "test", "test"),
		ModelCatalogURL: "http://127.0.0.1:1",
		WorkDir:         t.TempDir(),
		GoalDir:         goalDir,
	}
	if cfg != nil {
		cfg(&config)
	}
	result, err := New(config, store, tool.DefaultRegistry()).Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	loaded, exists, err := gs.Load(goal.GlobalKey)
	if err != nil || !exists {
		t.Fatalf("goal reload: exists=%v err=%v", exists, err)
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, sess, result
}

func hasContinuationMessage(sess *sessionstore.Session) bool {
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "Continue working autonomously") {
			return true
		}
	}
	return false
}

// The token budget used to be written on every continuation and read by nobody,
// so the only ceiling on the loop was its continuation count. It is a fence now:
// a goal that has spent its budget is paused rather than continued, and the
// pause must be visible to the owner rather than only in the goal file.
func TestATokenBudgetPausesAnActiveGoalInsteadOfContinuing(t *testing.T) {
	g, sess, result := runGoalContinuation(t, "budget", goal.Goal{
		Objective:   "ship it",
		Status:      goal.StatusActive,
		TokenBudget: 5,
		TokensUsed:  5,
	}, nil)

	if g.Status != goal.StatusPaused {
		t.Fatalf("goal status = %q, want paused once the budget is spent", g.Status)
	}
	if !strings.Contains(g.Reason, "budget") {
		t.Errorf("goal reason = %q, want it to name the budget", g.Reason)
	}
	if !strings.Contains(result.Final, "paused at token budget") {
		t.Errorf("final answer does not carry the pause notice: %q", result.Final)
	}
	if hasContinuationMessage(sess) {
		t.Error("the loop continued a goal it had just paused")
	}
}

// A plan run touches no files by definition, so a continuation in plan mode can
// only produce more plan text: the one shape guaranteed to spend turns without
// converging.
func TestPlanModeDoesNotContinueAnActiveGoal(t *testing.T) {
	g, sess, result := runGoalContinuation(t, "plan", goal.Goal{
		Objective: "plan the migration",
		Status:    goal.StatusActive,
	}, func(c *RunnerConfig) { c.PlanMode = true })

	if g.Status != goal.StatusActive {
		t.Errorf("goal status = %q, want active: a refusal is not a state change", g.Status)
	}
	if g.Iterations != 0 {
		t.Errorf("goal iterations = %d, want 0: plan mode must not continue", g.Iterations)
	}
	if hasContinuationMessage(sess) {
		t.Error("plan mode continued an active goal")
	}
	if strings.Contains(result.Final, "paused at token budget") {
		t.Errorf("a plan-mode refusal paused the goal: %q", result.Final)
	}
}

// An open question is the run waiting on the human. Continuing would answer it
// with the model's own guess, which is the fabrication the question exists to
// prevent.
func TestAPendingQuestionDoesNotContinueAnActiveGoal(t *testing.T) {
	const sessionID = "question"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	go func() {
		_, _ = question.Default.CreateWithContext(ctx, sessionID, "q-1", "which approach?", nil, time.Hour, func() {
			close(ready)
		})
	}()
	<-ready
	if len(question.Default.Pending(sessionID)) == 0 {
		t.Fatal("fixture did not register a pending question")
	}

	g, sess, _ := runGoalContinuation(t, sessionID, goal.Goal{
		Objective: "answer the question",
		Status:    goal.StatusActive,
	}, nil)

	if g.Status != goal.StatusActive {
		t.Errorf("goal status = %q, want active: a refusal is not a state change", g.Status)
	}
	if g.Iterations != 0 {
		t.Errorf("goal iterations = %d, want 0: a pending question must not continue", g.Iterations)
	}
	if hasContinuationMessage(sess) {
		t.Error("the loop continued while a question was still pending")
	}
}

// The positive case, so a guard that accidentally disables the loop outright
// cannot pass: an active goal with no blocker still continues. The budget is set
// to 1.5 requests' worth of reported spend, so the loop must continue once and
// then be caught by the fence - which is what keeps this bounded rather than
// running to the continuation cap.
func TestAnActiveGoalWithNoBlockerStillContinues(t *testing.T) {
	g, sess, _ := runGoalContinuation(t, "continue", goal.Goal{
		Objective:   "keep going",
		Status:      goal.StatusActive,
		TokenBudget: 150,
	}, nil)

	if g.Iterations < 1 {
		t.Fatalf("goal iterations = %d, want the loop to have continued at least once", g.Iterations)
	}
	if !hasContinuationMessage(sess) {
		t.Error("no continuation was delivered to the session")
	}
	if g.Status != goal.StatusPaused {
		t.Errorf("goal status = %q, want paused: the budget fence should have stopped the loop", g.Status)
	}
}

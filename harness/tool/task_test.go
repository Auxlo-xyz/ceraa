package tool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/operation"
)

// stubSubagent reports whatever the test tells it to.
type stubSubagent struct {
	mu       sync.Mutex
	requests int
	final    string
	err      error
	block    chan struct{}
}

func (s *stubSubagent) Run(ctx context.Context, brief string, budget RequestBudget) (SubagentResult, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return SubagentResult{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return SubagentResult{}, s.err
	}
	return SubagentResult{Final: s.final, Requests: s.requests, Yielded: s.requests >= budget.YieldAt}, nil
}

// stubFactory hands out stubSubagents and records the ids it issues.
type stubFactory struct {
	mu       sync.Mutex
	issued   int
	byID     map[string]*stubSubagent
	next     *stubSubagent
	failNext error
}

func newStubFactory() *stubFactory {
	return &stubFactory{byID: map[string]*stubSubagent{}}
}

func (f *stubFactory) SubagentFor(id string) (Subagent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	child, ok := f.byID[id]
	if !ok {
		return nil, errors.New("no such task")
	}
	return child, nil
}

func (f *stubFactory) NewSubagent(kind string) (Subagent, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return nil, "", err
	}
	f.issued++
	id := "task-" + string(rune('a'+f.issued-1))
	child := f.next
	if child == nil {
		child = &stubSubagent{final: "report from " + id}
	}
	f.byID[id] = child
	return child, id, nil
}

func taskTool(factory SubagentFactory) *TaskTool {
	return &TaskTool{Factory: factory, MaxConcurrent: 5, MaxDepth: 1, WallClock: 30 * time.Second}
}

func spec(t *testing.T, tool *TaskTool, args map[string]any) map[string]any {
	t.Helper()
	s, err := tool.Translate(args)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return s.Args
}

func TestTaskRunContextCancelsChild(t *testing.T) {
	factory := newStubFactory()
	release := make(chan struct{})
	factory.next = &stubSubagent{block: release}
	tool := taskTool(factory)
	sp := spec(t, tool, map[string]any{"subagent_type": "general", "description": "cancel child", "prompt": "wait"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.RunContext(ctx, operationSpec(sp))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunContext error = %v, want context.Canceled", err)
	}
}
func TestTaskDelegatesAndReturnsAnEnvelopeCarryingTheID(t *testing.T) {
	factory := newStubFactory()
	factory.next = &stubSubagent{final: "the provider exposes xai under provider:xai", requests: 3}
	tool := taskTool(factory)

	sp := spec(t, tool, map[string]any{
		"subagent_type": "explore", "description": "find the xai routes", "prompt": "look in callback.go",
	})
	out, err := tool.run(context.Background(), operationSpec(sp))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The id is the only way to resume or send more context, so it must be in the
	// envelope rather than buried in prose.
	if !strings.Contains(out, `id="task-a"`) {
		t.Errorf("the envelope does not carry the task id, so the child cannot be resumed:\n%s", out)
	}
	if !strings.Contains(out, "the provider exposes xai") {
		t.Errorf("the child's report is missing from the envelope:\n%s", out)
	}
	if !strings.Contains(out, `state="completed"`) {
		t.Errorf("a finished child should say so:\n%s", out)
	}
}

// A child stopped at its budget must not read as a finished one. Presenting a
// partial report as complete is how a model proceeds on a wrong belief.
func TestTaskEnvelopeMarksAStoppedChild(t *testing.T) {
	factory := newStubFactory()
	factory.next = &stubSubagent{final: "partial findings", requests: 30}
	tool := taskTool(factory)
	sp := spec(t, tool, map[string]any{"subagent_type": "general", "description": "long job", "prompt": "go"})
	out, err := tool.run(context.Background(), operationSpec(sp))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `state="stopped"`) {
		t.Errorf("a budget-stopped child reported as completed:\n%s", out)
	}
	if !strings.Contains(out, "partial") {
		t.Errorf("the partial report should still be returned:\n%s", out)
	}
}

func TestTaskRefusesToExceedFiveConcurrentChildren(t *testing.T) {
	factory := newStubFactory()
	tool := taskTool(factory)

	// Fill the ceiling from one goroutine, so the test cannot race itself. A
	// concurrent version of this passed or failed depending on scheduling, which
	// is the worst property a concurrency guard can have.
	release := make(chan struct{})
	factory.next = &stubSubagent{block: release}

	if got := tool.MaxConcurrent; got != 5 {
		t.Fatalf("ceiling is %d, want 5", got)
	}

	// Occupy every slot through the tool itself, each on its own goroutine, and
	// wait for each to confirm it is holding one.
	holding := make(chan struct{}, 5)
	factory.next = &stubSubagent{block: release}
	for i := 0; i < 5; i++ {
		go func() {
			sp, err := tool.Translate(map[string]any{"subagent_type": "general", "description": "d", "prompt": "p"})
			if err != nil {
				return
			}
			holding <- struct{}{}
			_, _ = tool.run(context.Background(), sp)
		}()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-holding:
		case <-time.After(5 * time.Second):
			t.Fatal("a child never took its slot")
		}
	}
	// Wait for the slots to be visible before asserting on the ceiling.
	deadline := time.Now().Add(3 * time.Second)
	for tool.LiveCount() < 5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if live := tool.LiveCount(); live != 5 {
		t.Fatalf("%d slots held, want 5", live)
	}

	// The sixth is refused, not queued. A queued child is invisible spend: it
	// keeps running after the parent has been told something else.
	sp, err := tool.Translate(map[string]any{"subagent_type": "general", "description": "d", "prompt": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.run(context.Background(), sp); err == nil {
		t.Error("the sixth child was accepted")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("the refusal does not mention the limit: %v", err)
	}

	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for tool.LiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := tool.LiveCount(); n != 0 {
		t.Errorf("%d slots still held after every child returned", n)
	}
}

// The slot must be released when the factory fails, or one bad call leaks a slot
// and the ceiling ratchets down for the rest of the session.
func TestTaskReleasesItsSlotWhenTheChildCannotStart(t *testing.T) {
	factory := newStubFactory()
	factory.failNext = errors.New("no model available")
	tool := taskTool(factory)
	sp := spec(t, tool, map[string]any{"subagent_type": "general", "description": "d", "prompt": "p"})
	if _, err := tool.run(context.Background(), operationSpec(sp)); err == nil {
		t.Fatal("expected the failed start to be reported")
	}
	if tool.LiveCount() != 0 {
		t.Errorf("%d slots leaked after a failed start", tool.LiveCount())
	}
}

func TestTaskResumeReusesTheSameChild(t *testing.T) {
	factory := newStubFactory()
	original := &stubSubagent{final: "first pass", requests: 2}
	factory.next = original
	tool := taskTool(factory)

	sp, err := tool.Translate(map[string]any{"subagent_type": "general", "description": "d", "prompt": "start"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := tool.run(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "first pass") {
		t.Fatalf("the first run did not return the child's answer:\n%s", first)
	}

	// The child keeps its own state, so a resumed call comes back from the same
	// object. Replacing the factory's template must not change that: the template
	// is only consulted when a child is created.
	factory.next = &stubSubagent{final: "a different child entirely", requests: 2}
	original.final = "second pass, with what the first pass learned"

	sp, err = tool.Translate(map[string]any{
		"subagent_type": "general", "description": "d", "prompt": "more", "task_id": "task-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.run(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if factory.issued != 1 {
		t.Errorf("%d children were created, want 1; a resume must reuse the child", factory.issued)
	}
	if !strings.Contains(out, "second pass") {
		t.Errorf("the resumed child did not answer:\n%s", out)
	}
	if strings.Contains(out, "a different child entirely") {
		t.Errorf("a second child answered the resume:\n%s", out)
	}
}

func TestTaskRefusesAnUnknownResumeID(t *testing.T) {
	tool := taskTool(newStubFactory())
	sp := spec(t, tool, map[string]any{"subagent_type": "general", "description": "d", "prompt": "p", "task_id": "nope"})
	if _, err := tool.run(context.Background(), operationSpec(sp)); err == nil {
		t.Error("an unknown task_id was accepted; the parent would think it resumed something")
	}
}

// A child must not be able to spawn a grandchild. This is the guard against
// exponential fan-out, and it belongs in Translate so a crafted call cannot skip it.
func TestTaskRefusesToNest(t *testing.T) {
	tool := taskTool(newStubFactory())
	_, err := tool.Translate(map[string]any{
		"subagent_type": "general", "description": "d", "prompt": "p", "_depth": 1,
	})
	if err == nil {
		t.Fatal("a nested delegation was accepted")
	}
	if !strings.Contains(err.Error(), "cannot delegate") {
		t.Errorf("the refusal does not explain the rule: %v", err)
	}
}

func TestTaskRejectsUnknownSubagentType(t *testing.T) {
	tool := taskTool(newStubFactory())
	_, err := tool.Translate(map[string]any{"subagent_type": "root", "description": "d", "prompt": "p"})
	if err == nil {
		t.Fatal("an unknown subagent_type was accepted")
	}
	if !strings.Contains(err.Error(), "explore") {
		t.Errorf("the refusal should name the valid types: %v", err)
	}
}

// A child starts blind. An empty brief is a guaranteed wasted run, so it is
// refused at translation rather than after a model request has been paid for.
func TestTaskRequiresABrief(t *testing.T) {
	tool := taskTool(newStubFactory())
	_, err := tool.Translate(map[string]any{"subagent_type": "explore", "description": "d", "prompt": "  "})
	if err == nil {
		t.Fatal("an empty brief was accepted")
	}
	if !strings.Contains(err.Error(), "no memory") {
		t.Errorf("the refusal should say why: %v", err)
	}
}

func TestTaskWithoutAFactoryFailsClearly(t *testing.T) {
	tool := &TaskTool{}
	sp, err := tool.Translate(map[string]any{"subagent_type": "explore", "description": "d", "prompt": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(sp); err == nil || !strings.Contains(err.Error(), "no factory") {
		t.Errorf("a missing factory produced %v, want a clear refusal", err)
	}
}

// The budget is graduated, and the graduation has to be an ordering: warn, then
// force a yield, then give up. If the yield point were below the warning the
// child would be stopped before it was ever told to wrap up.
func TestRequestBudgetIsGraduated(t *testing.T) {
	b := DefaultRequestBudget()
	if b.Soft <= 0 {
		t.Fatalf("soft budget is %d", b.Soft)
	}
	if b.YieldAt <= b.Soft {
		t.Errorf("yield at %d is not after the soft budget %d", b.YieldAt, b.Soft)
	}
	if b.HardAfter <= 0 {
		t.Errorf("no grace after the yield, so a child told to wrap up is cut off mid-report: %d", b.HardAfter)
	}
}

// operationSpec is a small helper so the tests read as calls rather than as
// struct literals at every site.
func operationSpec(args map[string]any) operation.Spec {
	return operation.Spec{Type: "task", Args: args}
}

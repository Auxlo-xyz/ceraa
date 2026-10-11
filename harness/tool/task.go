package tool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ceraa/harness/operation"
)

// The task tool delegates work to a child agent.
//
// Three decisions here are cost decisions, and each was made against a
// measurement rather than a preference.
//
// A child gets a **brief, never a fork** of the parent's transcript. Ceraa's
// largest live context was 118,599 tokens, of which 91.9% was tool output.
// Inheriting that costs full price on the child's first turn, at the *cold* cache
// rate, for a subtask that usually needs three files. A written brief of two
// thousand tokens costs two thousand. This is the single biggest lever available
// and it is free.
//
// Fan-out is **bounded**. opencode caps nesting depth but has no concurrency cap
// at all, so a model that decides to launch tasks in a loop is unbounded spend.
// There is a hard ceiling on live children here.
//
// The budget is **graduated, not a hard cap** (omp's design). Crossing it warns
// the child to wrap up. At 1.5x the free-running turn is stopped and the child is
// driven to one final answer, so partial findings come back as a real report
// rather than being discarded. A hard cap throws work away; a forced yield
// returns it.

// SubagentTypes are the child roles that can be delegated to.
var SubagentTypes = []string{"explore", "general", "review"}

// Subagent is a child agent, supplied by whoever knows how to build a runner.
//
// This indirection is not decoration: the tool layer cannot import the
// coordinator, because the coordinator imports the tool layer. The seam is the
// same one view_image uses for its vision client.
type Subagent interface {
	// Run drives the child to a final answer and returns it.
	Run(ctx context.Context, brief string, budget RequestBudget) (SubagentResult, error)
}

// SubagentResult is what a child produced.
type SubagentResult struct {
	Final string
	// Requests is how many model requests the child actually made, which is what
	// the budget is enforced against.
	Requests int
	// Yielded reports that the child was stopped and asked for its findings, so
	// the result is a real report rather than a truncated transcript.
	Yielded bool
}

// SubagentFactory builds a child, or resumes one by id.
//
// A non-empty resumeID must return the same child that id names, so the child's
// cache stays warm. A new child gets a new id back, which the caller records.
type SubagentFactory interface {
	SubagentFor(resumeID string) (Subagent, error)
	// NewSubagent creates a child and returns it with the id to resume it by.
	NewSubagent(kind string) (Subagent, string, error)
}

// RequestBudget is a soft ceiling on model requests for one child.
type RequestBudget struct {
	Soft      int
	YieldAt   int
	HardAfter int
}

// DefaultRequestBudget is the graduated budget: warn at Soft, force a yield at
// YieldAt, and give up HardAfter requests after that.
//
// The grace exists because a child told to wrap up needs a turn or two to do it.
// Cutting it off at the yield point would discard the findings it was about to
// report, which is the outcome the graduation is meant to avoid.
func DefaultRequestBudget() RequestBudget {
	const soft = 20
	return RequestBudget{Soft: soft, YieldAt: int(float64(soft) * 1.5), HardAfter: 5}
}

// TaskTool delegates work to a child agent.
type TaskTool struct {
	Factory SubagentFactory
	// MaxConcurrent is the hard ceiling on live children.
	MaxConcurrent int
	// MaxDepth is how deep delegation may nest. Only 0 (no children) and 1 (one
	// level) are meaningful here; the child has no task tool at all, so depth
	// cannot exceed 1 in practice and this is a second line of defence.
	MaxDepth int
	// WallClock bounds one child regardless of what it is doing. Backgrounding
	// makes cost worse rather than better, because the child keeps spending after
	// the person stops watching.
	WallClock time.Duration

	mu       sync.Mutex
	live     map[string]struct{}
	records  map[string]taskRecord
	nextSlot int
}

type taskRecord struct {
	kind   string
	brief  string
	budget int
}

// ensure lazily creates the maps under the tool's lock: run is entered from one
// goroutine per child, and two of them initialising the same map at once is a
// race the concurrency test caught.
func (t *TaskTool) ensure() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live == nil {
		t.live = map[string]struct{}{}
	}
	if t.records == nil {
		t.records = map[string]taskRecord{}
	}
}

// NewTaskTool returns a task tool with no factory attached, so the registry can
// hold it while the application decides whether subagents are available.
func NewTaskTool() *TaskTool { return &TaskTool{MaxConcurrent: 5, MaxDepth: 1} }

func (t *TaskTool) Name() string { return "task" }

// Description states what the tool is. When to use it, how to brief a child, and
// what to do with the result all live in the prompt's toolrules section, per
// TestDescriptionsCarryNoUsageGuidance.
func (t *TaskTool) Description() string {
	return "Delegate work to a child agent and get its report back. The child is a " +
		"separate agent with its own context: it receives only the brief in prompt, " +
		"never this conversation's history. It cannot delegate further. Returns the " +
		"child's final report wrapped in a task envelope carrying the id needed to " +
		"resume it."
}

func (t *TaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subagent_type": map[string]any{
				"type":        "string",
				"enum":        SubagentTypes,
				"description": "Which child role to run. explore reads and searches, general takes a self-contained job, review critiques something already written.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A short label for the task, three to five words.",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "The brief for the child: the goal, the files or paths involved, and exactly what to report back. The child has no memory of this conversation, so be complete.",
			},
			"task_id": map[string]any{
				"type":        "string",
				"description": "Resume a previous child instead of starting a new one, by the task_id a previous result returned.",
			},
		},
		"required": []string{"subagent_type", "description", "prompt"},
	}
}

func (t *TaskTool) Translate(args map[string]any) (operation.Spec, error) {
	kind, _ := args["subagent_type"].(string)
	description, _ := args["description"].(string)
	prompt, _ := args["prompt"].(string)
	resume, _ := args["task_id"].(string)

	kind = strings.TrimSpace(kind)
	if kind == "" {
		return operation.Spec{}, fmt.Errorf("subagent_type is required")
	}
	if !validSubagentType(kind) {
		return operation.Spec{}, fmt.Errorf("unknown subagent_type %q; use one of %s", kind, strings.Join(SubagentTypes, ", "))
	}
	if strings.TrimSpace(description) == "" {
		return operation.Spec{}, fmt.Errorf("description is required")
	}
	if strings.TrimSpace(prompt) == "" {
		return operation.Spec{}, fmt.Errorf("prompt is required; the child has no memory of this conversation")
	}
	if depth, _ := args["_depth"].(int); depth > 0 {
		return operation.Spec{}, fmt.Errorf("a child cannot delegate further; do this work in the parent")
	}
	// Names and text only. The brief goes into SpecArgs, which is persisted, so
	// this must never carry anything secret.
	return operation.Spec{Type: "task", Args: map[string]any{
		"subagent_type": kind,
		"description":   strings.TrimSpace(description),
		"prompt":        strings.TrimSpace(prompt),
		"task_id":       strings.TrimSpace(resume),
	}}, nil
}

func (t *TaskTool) TranslateResult(op operation.Operation) string { return op.Output }

func validSubagentType(kind string) bool {
	for _, known := range SubagentTypes {
		if known == kind {
			return true
		}
	}
	return false
}

// Run drives a child to a report.
//
// It is a tool.Runner rather than an operation, like get_goal and list_secrets: a
// subagent is a nested agent loop, not a queued unit of work, and putting it on
// the operation pool would occupy a concurrency slot for the whole of its run.
func (t *TaskTool) Run(spec operation.Spec) (string, error) {
	return t.run(context.Background(), spec)
}

func (t *TaskTool) run(ctx context.Context, spec operation.Spec) (string, error) {
	if t.Factory == nil {
		return "", fmt.Errorf("subagents are not available: no factory is attached")
	}
	t.ensure()
	kind, _ := spec.Args["subagent_type"].(string)
	brief, _ := spec.Args["prompt"].(string)
	resume, _ := spec.Args["task_id"].(string)
	budget := DefaultRequestBudget()

	var child Subagent
	var id string
	var slot string
	var err error
	if resume != "" {
		child, err = t.Factory.SubagentFor(resume)
		if err != nil {
			return "", fmt.Errorf("cannot resume %q: %w", resume, err)
		}
		id = resume
	} else {
		// Assigned, not declared: a := here would shadow the outer slot and the
		// release at the end of this function would then be handed an empty string,
		// leaking every slot the first time a child finished.
		var slotErr error
		slot, slotErr = t.acquire()
		if slotErr != nil {
			return "", slotErr
		}
		child, id, err = t.Factory.NewSubagent(kind)
		if err != nil {
			t.release(slot)
			return "", fmt.Errorf("cannot start a %s subagent: %w", kind, err)
		}
		t.mu.Lock()
		t.records[id] = taskRecord{kind: kind, brief: brief, budget: budget.Soft}
		t.mu.Unlock()
	}

	if t.WallClock > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.WallClock)
		defer cancel()
	}
	result, runErr := child.Run(ctx, brief, budget)
	t.release(slot)
	if runErr != nil {
		return "", fmt.Errorf("subagent %s failed: %w", id, runErr)
	}
	return renderTaskResult(id, kind, result, budget), nil
}

// renderTaskResult wraps the child's report for the parent.
//
// The envelope carries the id so the parent can resume or send more context, and
// the state so a stopped child is not read as a finished one.
func renderTaskResult(id, kind string, result SubagentResult, budget RequestBudget) string {
	state := "completed"
	note := ""
	if result.Yielded {
		state = "stopped"
		note = fmt.Sprintf("\nThis child was stopped at its request budget and asked to report early, "+
			"so treat this as a partial result. It made %d of %d requests before being stopped.",
			result.Requests, budget.Soft)
	}
	return fmt.Sprintf("<task id=\"%s\" type=\"%s\" state=\"%s\" requests=\"%d\">\n<task_result>%s\n</task_result>\n</task>%s",
		id, kind, state, result.Requests, strings.TrimSpace(result.Final), note)
}

// acquire takes a concurrency slot, or refuses.
//
// A refusal has to be an error the model can act on rather than a silent queue,
// because a queued child is invisible spend: it is still running after the
// parent has been told something else.
func (t *TaskTool) acquire() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	limit := t.MaxConcurrent
	if limit <= 0 {
		limit = 5
	}
	if len(t.live) >= limit {
		return "", fmt.Errorf("%d subagents are already running, which is the limit; "+
			"wait for one to finish, or continue the work yourself", limit)
	}
	// A unique key per slot. A constant key would make every acquire overwrite the
	// previous one, so len(live) stayed at 1 and the ceiling was never enforced --
	// which is exactly the bug the concurrency test is here to catch.
	t.nextSlot++
	slot := fmt.Sprintf("slot-%d", t.nextSlot)
	t.live[slot] = struct{}{}
	return slot, nil
}

func (t *TaskTool) release(slot string) {
	if slot == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.live, slot)
}

// LiveCount reports how many children are running, for tests and for the Hub.
func (t *TaskTool) LiveCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.live)
}

// Sequential reports the tool as ordered. Two children touching the same files
// concurrently is the failure this whole feature invites.
func (t *TaskTool) Sequential() bool { return false }

// MutationKey puts children of one kind in their own lane, so the operation
// scheduler can interleave different kinds while serialising nothing.
func (t *TaskTool) MutationKey(args map[string]any) string {
	kind, _ := args["subagent_type"].(string)
	return "subagent:" + kind
}

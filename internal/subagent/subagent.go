// Package subagent builds child runs for the task tool.
//
// It lives outside both front-ends because neither may own it: the CLI must not
// import Telegram, and a child's registry must be built the way its own
// front-end builds one (NewRegistry), or a child would get a different tool set
// from its parent.
package subagent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"

	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal/config"
	"ceraa/internal/logger"
)

// Factory builds child runs for the task tool.
//
// The tool layer cannot import the coordinator, which imports the tool layer, so
// the seam is a factory supplied from here: the one place that knows how a run is
// assembled, which provider is resolved, and what stores exist.
//
// A child is an ordinary run with three differences. All three are cost or safety
// decisions rather than features.
//
// It gets **no history**. The parent's transcript is never copied. Ceraa's largest
// live context was 118,599 tokens, 91.9% of it tool output, and a child that
// inherited that would pay for all of it on its first turn at the cold cache rate,
// for a subtask that usually needs three files. The brief is the whole input.
//
// It gets **no task tool**. Nesting is refused in the tool's Translate and the
// tool is also removed from the child's registry, so a crafted call cannot reach
// it either. Two independent guards, because unbounded fan-out is the failure
// this whole feature invites.
//
// It gets a **request budget**, graduated rather than hard, so a confused child
// cannot spend indefinitely and still returns what it found.

// rolePrompt is prepended to the child's system prompt. It is a prompt, not
// a tool description, so usage guidance belongs here rather than in a schema.
var rolePrompt = map[string]string{
	"explore": "You are a read-only explorer working for another agent. Find things out and " +
		"report file paths, line numbers, and what is actually there. Do not edit anything.",
	"general": "You are a general-purpose agent working one self-contained job for another " +
		"agent. Do the whole of what you were asked, then report what you did and what is left.",
	"review": "You are a reviewer working for another agent. Report problems: correctness, " +
		"security, and anything that would break. Do not fix them; say what is wrong and why.",
}

// Factory builds child runs for the task tool.
type Factory struct {
	Cfg           *config.Config
	User          string
	ParentSession string
	ProjectRoot   string
	ProviderID    string
	ProviderCfg   config.LLMProvider
	// ResolveModel is the parent's credential resolver. A subscription provider
	// such as DeepSeek authenticates from the encrypted account grant rather
	// than the profile's api key, and without this a child sends the plain key
	// and is answered 401 while the parent succeeds on the same turn.
	ResolveModel    func(context.Context) (llm.ProviderConfig, error)
	Reasoning       llm.ReasonLevel
	ParentPrompt    string
	SkillStore      *skills.Store
	SkillContext    string
	EvaluationStore *evaluation.Store
	Store           *sessionstore.Store
	// OnChildUsage rolls a child's spend into the parent's, so a cheap fan-out and
	// an expensive one do not look identical in the event stream.
	OnChildUsage func(llm.UsageInfo)
	// WallClock bounds one child regardless of what it is doing. Backgrounding makes
	// cost worse rather than better, because a child keeps spending after the person
	// stops watching.
	WallClock time.Duration
	// NewRegistry builds the child's tool set the way its front-end builds one, so
	// a child gets the same tools as its parent. A nil seam falls back to the
	// default registry rather than failing: a child with no tools is a wasted run.
	NewRegistry func(user string, skillStore *skills.Store) *tool.Registry

	mu       sync.Mutex
	children map[string]*childRun
	counter  int
}

// childRun records what a child cost, so a completed and a stopped child can be
// told apart at the Hub.
type childRun struct {
	id       string
	kind     string
	requests int
	yielded  bool
	done     bool
}

var _ tool.SubagentFactory = (*Factory)(nil)

func (f *Factory) NewSubagent(kind string) (tool.Subagent, string, error) {
	if f.Store == nil {
		return nil, "", fmt.Errorf("no session store is available for subagents")
	}
	if _, ok := rolePrompt[kind]; !ok {
		return nil, "", fmt.Errorf("unknown subagent kind %q", kind)
	}
	f.mu.Lock()
	// The map is created here rather than at the construction site because a
	// nil map assignment is a panic, and every construction site that has ever
	// existed omitted it: production, the wiring test, and this one. Lazy
	// creation under the same lock that guards it cannot be beaten by a
	// construction path that forgets.
	if f.children == nil {
		f.children = make(map[string]*childRun)
	}
	f.counter++
	id := fmt.Sprintf("%s-sub-%s-%d", f.ParentSession, kind, f.counter)
	f.children[id] = &childRun{id: id, kind: kind}
	f.mu.Unlock()

	if err := f.Store.Save(&sessionstore.Session{
		ID:       id,
		ParentID: f.ParentSession,
		Version:  2,
		Messages: []sessionstore.Message{},
	}); err != nil {
		f.mu.Lock()
		delete(f.children, id)
		f.mu.Unlock()
		return nil, "", fmt.Errorf("create child session: %w", err)
	}
	logger.Info(fmt.Sprintf("subagent started: id=%s kind=%s parent=%s", id, kind, f.ParentSession))
	return &childAgent{factory: f, id: id, kind: kind}, id, nil
}

// SubagentFor resumes a child by id.
//
// The same session is reused, so the child's context and therefore its warm cache
// survive. It is also re-checked against its budget on every resume: a child that
// is resumed is a child that can outlive the run which started it.
func (f *Factory) SubagentFor(id string) (tool.Subagent, error) {
	f.mu.Lock()
	child, known := f.children[id]
	f.mu.Unlock()
	if !known {
		return nil, fmt.Errorf("no subagent with that id")
	}
	sess, err := f.Store.Load(id)
	if err != nil {
		return nil, fmt.Errorf("load child session: %w", err)
	}
	// A task id is not a capability. Without this a parent could name another
	// session's child and read its transcript through the brief it supplies.
	if sess.ParentID != f.ParentSession {
		return nil, fmt.Errorf("that task belongs to a different session")
	}
	return &childAgent{factory: f, id: id, kind: child.kind}, nil
}

// childAgent is one child. A per-child type rather than the factory itself,
// because the factory outlives every child it creates and Run has to know which
// session it is driving.
type childAgent struct {
	factory *Factory
	id      string
	kind    string
}

var _ tool.Subagent = (*childAgent)(nil)

func (c *childAgent) Run(ctx context.Context, brief string, budget tool.RequestBudget) (tool.SubagentResult, error) {
	f := c.factory
	if f.WallClock > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.WallClock)
		defer cancel()
	}

	newRegistry := f.NewRegistry
	if newRegistry == nil {
		newRegistry = tool.DefaultRegistryForUser
	}
	registry := newRegistry(f.User, f.SkillStore)
	// Two guards against nesting, not one: Translate refuses a nested call, and the
	// tool is not in the child's registry at all. A guard that only exists in one
	// place is a guard that a crafted call walks around.
	registry.Remove("task")

	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           c.id,
		Reasoning:           f.Reasoning,
		SystemPrompt:        childSystemPrompt(f.ParentPrompt, c.kind),
		Model:               llm.FromProvider(f.ProviderCfg.URL, f.ProviderCfg.APIKey, f.ProviderCfg.Model),
		ResolveModel:        f.ResolveModel,
		ContextLimit:        f.ProviderCfg.ContextLimit,
		MaxOutputTokens:     f.ProviderCfg.MaxOutputTokens,
		WorkDir:             f.ProjectRoot,
		ToolOutputDir:       filepath.Join(f.Cfg.SessionFile, "subagent-output"),
		MaxToolResultChars:  f.Cfg.MaxToolResultChars,
		MaxConcurrentTools:  f.Cfg.MaxConcurrentTools,
		GoalDir:             f.Cfg.SessionFile,
		ModelCatalogURL:     f.Cfg.ModelCatalogURL,
		ModelCatalogCache:   f.Cfg.ModelCatalogCache,
		ModelCatalogRefresh: time.Duration(f.Cfg.ModelCatalogRefreshHours) * time.Hour,

		SkillContext:    f.SkillContext,
		EvaluationStore: f.EvaluationStore,
		// The budget is the point of the child, and it is enforced in the loop rather
		// than here, because here is the only place that sees the turns a child takes
		// after compaction and continuations.
		MaxRequests: budget.Soft,
		YieldAt:     budget.YieldAt,
		HardGrace:   budget.HardAfter,
		// A child cannot ask the person anything. The question tool blocks on an
		// answer that cannot arrive, because the prompt it would surface is the
		// parent's chat and nobody is looking at it for a child's question.
		NeedsAuthorization: func(name string, args map[string]any) bool { return false },
		Callbacks: coordinator.Callbacks{
			// The child's own verdict is not forwarded: its spend is rolled into the
			// parent's accumulator here, and the parent reconciles on the way out.
			OnUsage: func(u llm.UsageInfo, _ llm.UsageTrust) {
				if f.OnChildUsage != nil {
					f.OnChildUsage(u)
				}
			},
		},
	}, f.Store, registry)

	result, err := runner.Run(ctx, brief)
	if err != nil {
		// A child that ran out of budget returns an error, and that is not a
		// failure to report as one: the findings it had are worth more than the
		// reason it stopped.
		if result.Final != "" {
			logger.Warn(fmt.Sprintf("subagent %s ended early: %v (with findings)", c.id, err))
		} else {
			return tool.SubagentResult{}, err
		}
	}

	yielded := isBudgetStop(err)
	f.mu.Lock()
	if rec, ok := f.children[c.id]; ok {
		rec.yielded = yielded
		rec.done = true
	}
	f.mu.Unlock()
	logger.Info(fmt.Sprintf("subagent finished: id=%s kind=%s yielded=%v chars=%d", c.id, c.kind, yielded, len(result.Final)))
	return tool.SubagentResult{Final: result.Final, Yielded: yielded}, nil
}

// isBudgetStop distinguishes "stopped at its budget" from "broke", so the parent
// is told which it got. Presenting a partial report as complete is how a model
// proceeds on a wrong belief.
func isBudgetStop(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "abandoned after") || strings.Contains(msg, "budget")
}

// childSystemPrompt is the parent's prompt plus this child's role. The parent's is
// kept so the child inherits the workspace, skills and tool rules it would have had
// in the main session; dropping it would produce a child that behaves differently
// from the agent that spawned it.
func childSystemPrompt(parent, kind string) string {
	role := rolePrompt[kind]
	if role == "" {
		role = rolePrompt["general"]
	}
	return parent + "\n\n## Your role in this task\n\n" + role
}

// Attach hands the registry a task tool wired to a factory built from
// this run's provider, project and stores.
//
// Attached rather than constructed in the registry because the tool layer cannot
// import the coordinator, and because a child that used a different provider or
// project from its parent would be a surprise rather than a delegation.
func Attach(registry *tool.Registry, factory *Factory) {
	task, ok := registry.Get("task")
	if !ok {
		logger.Warn("subagents unavailable: the task tool is not registered")
		return
	}
	wired, ok := task.(*tool.TaskTool)
	if !ok {
		logger.Warn("subagents unavailable: the task tool has an unexpected type")
		return
	}
	wired.Factory = factory
	wired.MaxConcurrent = 5
	wired.MaxDepth = 1
	if wired.WallClock == 0 {
		wired.WallClock = factory.WallClock
	}
}

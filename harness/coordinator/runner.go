package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ceraa/harness/compaction"
	"ceraa/harness/contextbuilder"
	"ceraa/harness/evaluation"
	"ceraa/harness/goal"
	"ceraa/harness/inbox"
	"ceraa/harness/instruction"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal/logger"
)

type RunnerConfig struct {
	SessionID    string
	SystemPrompt string
	// Reasoning selects the level every request asks for.
	Reasoning llm.ReasonLevel
	// ReasoningEfforts and ReasoningDefault are the model's declared thinking
	// vocabulary, resolved from the catalog when the provider was added. Empty
	// means unresolved, and the level is clamped conservatively rather than
	// guessed at.
	ReasoningEfforts   []string
	ReasoningDefault   string
	StopWhenIdle       bool
	Model              llm.ProviderConfig
	ContextLimit       int
	MaxOutputTokens    int
	WorkDir            string
	ToolOutputDir      string
	MaxToolResultChars int
	MaxConcurrentTools int
	// MaxRequests is a soft ceiling on model requests for one run, graduated rather
	// than hard: past MaxRequests the run is told to wrap up, past YieldAt it is
	// driven to a single final answer so partial findings still come back, and only
	// past YieldAt+HardGrace is it abandoned.
	//
	// A hard cut would discard whatever the run had found, which is the worst
	// outcome available: the work is paid for and thrown away. Zero means no limit.
	MaxRequests int
	// YieldAt and HardGrace are read only when MaxRequests is set.
	YieldAt   int
	HardGrace int
	// BudgetNotice is delivered to the model when the soft ceiling is crossed.
	BudgetNotice        func(requests, soft int) string
	GoalDir             string
	ModelCatalogURL     string
	ModelCatalogCache   string
	ModelCatalogRefresh time.Duration
	MemoryContext       string
	// MemoryContextFunc renders memory for the current turn and is preferred over
	// MemoryContext when set.
	//
	// Memory used to be rendered once, here, at construction. An edit to the
	// memory file therefore did not reach the model until the agent was rebuilt,
	// so for a long-lived daemon the file on disk and the memory in the prompt
	// diverged with nothing logging it. A per-turn function makes the file the
	// source of truth rather than a startup snapshot of it.
	MemoryContextFunc func() (string, error)
	// MemoryDelivery selects where memory reaches the model: "prompt" (default)
	// renders a <memory> block after the cache seam, "sidekick" suppresses the
	// block and annotates tool results at the point of use instead.
	MemoryDelivery string
	// MemoryEntries lists stored memories for sidekick delivery. Nil disables it,
	// which is what keeps the prompt path working with nothing configured.
	MemoryEntries func() ([]memory.Entry, error)
	// MemoryFile is the append-only log this run reads for what has been learned,
	// and when it is set it replaces MemoryEntries for sidekick delivery.
	//
	// A reader rather than a store because the file is the source of truth and a
	// snapshot taken at construction diverges from it silently in a long-lived
	// daemon - the same reason MemoryContextFunc is a function.
	//
	// It replaces the store because the store re-rendered and re-ranked itself
	// against a wall clock on every request, and that volatility sat ahead of the
	// cache seam: the bytes changed when nothing had been written, so the cached
	// prefix was rebuilt on every turn. An append-only log cannot do that. Nothing
	// re-ranks, nothing decays, and the tail is read rather than the whole, so the
	// cost per turn is proportional to what is delivered rather than to everything
	// ever learned.
	MemoryFile func() *memory.MemoryFile
	// MemoryTail is how many entries of the log delivery reads. Zero means
	// defaultMemoryTail, which is what a run that delivers on every call can
	// render before the run ceiling stops it.
	MemoryTail int
	// BondFunc renders the operator's identity file for sidekick delivery, which
	// is the only mode that sees it.
	//
	// It is separate from MemoryContextFunc because that one combines the bond
	// with project memory, and a combined renderer cannot be taken apart: sidekick
	// mode suppresses the whole block, so a bond arriving only inside it is a bond
	// that never arrives. The prompt modes ignore this field entirely and keep
	// reading the bond out of MemoryContextFunc.
	BondFunc func() (string, error)
	// InstructionRules compiles the project's instruction file into the rules
	// that tool-result delivery selects from. Nil disables instruction delivery
	// entirely, which is a configuration rather than a failure.
	//
	// It is a loader and not a snapshot because the file on disk has to stay the
	// source of truth rather than becoming a startup artefact: a caller wires
	// its instruction store's own refresh here and gets whatever the file says
	// at the start of the run, including an empty rule set on a project that
	// has not written one yet.
	//
	// It is a function rather than a value because nothing on the hot path is
	// allowed to read or parse a file. The runner builds one instructionSource
	// per run from this function, and that source calls it exactly once and
	// holds the result for the life of the run, so the per-step cost is a
	// snapshot read rather than an open, a read and a compile. A per-step reload
	// would be the exact defect this feature exists to remove, and it would
	// look like a working feature the whole time.
	//
	// Once per run, not once per session, is deliberate: a rule file edited
	// mid-run would otherwise put two revisions of the same project instruction
	// into one transcript with nothing marking which is current. The next run
	// picks the edit up, which is the same boundary the sidekick's once-per-run
	// bond uses and for the same reason.
	InstructionRules func() instruction.Snapshot
	SkillContext     string
	EvaluationStore  *evaluation.Store
	Inbox            *inbox.Inbox
	OperationPool    *operation.LocalManager
	Compactor        *compaction.Compactor
	ResolveModel     func(context.Context) (llm.ProviderConfig, error)
	ForcedCompaction func() bool
	// OnFileMutated is called once per successfully completed file mutation, with
	// the path. It exists so a search index can be kept current at the point the
	// write actually succeeded, rather than by a timer or a watcher.
	//
	// At the tool boundary specifically because that is the only place the written
	// path is known to be on disk, and the only place where "the agent changed a
	// file" is an event rather than an inference.
	OnFileMutated      func(path string)
	AuthorizeTool      func(context.Context, string, string, map[string]any) (bool, error)
	NeedsAuthorization func(string, map[string]any) bool
	AdjustSpec         func(*operation.Spec)
	Callbacks          Callbacks
}

type Config = RunnerConfig

type Callbacks struct {
	OnContent   func(string)
	OnReasoning func(string)
	// OnStepReasoningOnly fires after a step that ended in a tool call, which is
	// how this runner defines a final answer: a step with no calls is the answer,
	// so a step with calls was still working. A provider that puts its reasoning on
	// the content channel leaves a consumer unable to tell that text from the
	// answer by looking at it, because the two interleave and the split lands
	// mid-word. It can be told.
	OnStepReasoningOnly func()
	OnToolStart         func(string, string, map[string]any)
	OnToolDone          func(string, string, operation.ResultEvent)
	OnCompactionStart   func()
	// OnRetry fires before each provider retry wait, with the error, the attempt
	// number and the delay. A gateway backoff runs to about a minute and a silent
	// wait that long is indistinguishable from a hang.
	OnRetry          func(error, int, time.Duration)
	OnCompactionDone func(bool, error)
	// OnUsage carries the run's trust verdict beside the counts. The provider's
	// input figure can be a running total rather than this request, and a renderer
	// that is told only the number has no way to leave it out.
	OnUsage func(llm.UsageInfo, llm.UsageTrust)
	OnCost  func(llm.CostInfo, string)
	OnDone  func(string)
	OnError func(error)
}

type Result struct {
	Final         string
	Usage         llm.UsageInfo
	Cost          llm.CostInfo
	PricingStatus string
}

type Runner struct {
	cfg        RunnerConfig
	store      *sessionstore.Store
	reg        *tool.Registry
	opMgr      *operation.LocalManager
	catalog    *llm.CatalogClient
	evaluation *evaluation.Store
	cb         Callbacks

	// identityOnce guards identity, which is resolved from the provider on first
	// use and then held for the life of the run.
	identityOnce sync.Once
	identity     string

	// contextLimit is the effective budget, recalibrated whenever the model
	// changes rather than fixed at construction. Guarded because a model switch
	// can arrive on a different goroutine from the run reading it.
	contextMu     sync.RWMutex
	contextLimit  int
	contextSource llm.ContextWindowSource
	contextLogged bool

	// delivery and sidekick are the memory delivery mode and its per-run state.
	delivery memory.Delivery
	sidekick *sidekickState

	// instructions is the instruction delivery state for this run: the compiled
	// rules, the run's delivered set, and the scope matcher.
	//
	// A field beside sidekick rather than inside it, and the two are never
	// consulted together. They answer different questions - what this agent has
	// learned about the person and the work, versus what this project says
	// about these paths - and a caller is entitled to configure exactly one of
	// them. Keying instruction delivery on the sidekick being present is how a
	// project with no memory store, which is a legal configuration, silently
	// stops being told its own rules.
	//
	// Nil means no instruction source was configured, and every read below
	// answers rather than panicking: instruction delivery is optional, and a run
	// must not fail over a feature it was not given.
	instructions *instructionSource

	// trimmedSeen remembers, per run, the trimmed tool results already sent, keyed
	// by a hash of the content. Scoped to the run because that is where the repeat
	// happens: a model that reads a long file, loses the tail to the cap, and
	// reads it again within the same task. Re-reading across runs is rarer, and
	// carrying the map between them would need persistence and invalidation for a
	// smaller win.
	trimmedMu    sync.Mutex
	trimmedSeen  map[string]struct{}
	trimmedOrder []string
}

// ContextLimit reports the effective context budget and where it came from.
func (r *Runner) ContextLimit() (int, llm.ContextWindowSource) {
	r.contextMu.RLock()
	defer r.contextMu.RUnlock()
	if r.contextLimit > 0 {
		return r.contextLimit, r.contextSource
	}
	// Before the first calibration, fall back to the configured value so a caller
	// that asks early gets a real number rather than zero.
	if r.cfg.ContextLimit > 0 {
		return r.cfg.ContextLimit, llm.ContextFromProfile
	}
	return llm.FallbackContextWindow, llm.ContextFromDefault
}

// effectiveContextLimit is the budget messages are assembled against.
func (r *Runner) effectiveContextLimit() int {
	limit, _ := r.ContextLimit()
	return limit
}

// recalibrateContextLimit re-resolves the budget for the current model and pushes
// it into the compactor.
//
// This exists because the limit is a property of the model, not of the process.
// It used to be a constant baked in at construction, so switching to a model with
// a different window left the runner budgeting against the old one, and the
// catalog entry carrying the real window was recorded on the run and never read.
// That is the worst of both: the truth was fetched and discarded.
func (r *Runner) recalibrateContextLimit(meta llm.ModelMetadata, lookupErr error) llm.ContextWindowDecision {
	model := r.cfg.Model.Model
	decision := llm.ContextWindowDecision{Model: model, Configured: r.cfg.ContextLimit}
	if lookupErr == nil {
		decision.AdvertisedWindow = meta.ContextWindow
	}
	limit, source := llm.ResolveContextWindow(r.cfg.ContextLimit, meta)
	decision.Limit, decision.Source = limit, source

	r.contextMu.Lock()
	changed := limit != r.contextLimit
	r.contextLimit, r.contextSource = limit, source
	first := !r.contextLogged
	r.contextLogged = true
	r.contextMu.Unlock()

	if r.cfg.Compactor != nil {
		r.cfg.Compactor.ContextLimit = limit
	}
	// Log the first resolution and every change. A silent recalibration is how a
	// budget ends up wrong with nothing to explain it.
	if first || changed {
		logger.Info("Context budget recalibrated: " + decision.String())
	}
	return decision
}

// memoryBlock renders the persisted memories for this turn, falling back to the
// startup snapshot when no live renderer is configured.
//
// It is a separate method from memoryContext so the two delivery modes that do
// not put the block in the varying suffix can share the same render without
// copying the fallback logic.
func (r *Runner) memoryBlock() string {
	if r.cfg.MemoryContextFunc != nil {
		rendered, err := r.cfg.MemoryContextFunc()
		if err != nil {
			// A memory read failure must not fail the turn, and must not silently
			// look like "there is no memory".
			//
			// Live returns whatever rendered alongside the error, which can only
			// be the store: the file is the source that failed. Keeping the
			// startup snapshot of that file beside the live store means neither
			// source disappears because the other broke, and the log line still
			// names what was lost.
			logger.Warn("Memory render failed, keeping the snapshot and any partial render: " + err.Error())
			snapshot := strings.TrimSpace(r.cfg.MemoryContext)
			live := strings.TrimSpace(rendered)
			switch {
			case snapshot != "" && live != "":
				return snapshot + "\n" + live
			case snapshot != "":
				return snapshot
			default:
				return live
			}
		}
		if strings.TrimSpace(rendered) != "" {
			return strings.TrimSpace(rendered)
		}
	}
	return strings.TrimSpace(r.cfg.MemoryContext)
}

// memoryContext renders memory for the varying tail of the prompt.
//
// Both of the modes that do not want the block here are suppressed:
// DeliverySidekick emits nothing into the prompt at all, and DeliveryCached
// selects the stable prefix ahead of the seam instead.
func (r *Runner) memoryContext() string {
	if r.delivery == memory.DeliverySidekick || r.delivery == memory.DeliveryCached {
		return ""
	}
	return r.memoryBlock()
}

// promptCacheKey identifies the reusable prefix for the provider's cache router,
// excluding the session id.
//
// It has no production caller. The request path builds its key with
// sessionPromptCacheKey, which DOES include the session, so this variant is dead
// code that exists only to be asserted on by a test. The two docstrings
// contradicted each other for long enough that this one was assumed to be the
// live behaviour, and a cache investigation was then run on that false premise.
//
// The earlier claim here - that a brand-new session's first request "still
// reported cached tokens" - was never measured and is not the reason the live
// provider reports cached=0 on a cold start. Code Assist is the active
// transport and it does not read this value at all; it routes on req.SessionID
// via codeAssistSessionID.
func promptCacheKey(model llm.ProviderConfig) string {
	return sessionPromptCacheKey(model, "")
}

// sessionPromptCacheKey namespaces the cache by session as well as by endpoint
// and model.
//
// It was keyed on the endpoint and model alone, which is the same string for
// every session and every run this install has ever made. The provider namespaces
// its cache by that key, so all of them share one bucket, and a request is free
// to match a stale prefix left by an earlier run instead of its own.
//
// That is not a hypothetical. A measured run showed the cache reporting the same
// 3584-token hit on requests 1, 3, 5 and 7 while the conversation grew from 3876
// to 13736 tokens, and then alternating 8704 and 14848 on later ones. A prefix
// cache serving the current conversation climbs monotonically, because each
// request re-sends everything the last one did. Repeating values are other
// runs' entries, and the hit rate that follows is noise rather than a ceiling.
//
// Scoping to the session keeps a run's entries from colliding with anything
// else while still reusing the prefix across every request inside that run,
// which is where all of the saving is.
//
// The digests are kept short on purpose. OpenAI accepts at most 64 characters of
// prompt_cache_key and silently ignores anything longer, so the endpoint digest
// is truncated to fit alongside the session digest instead of being written in
// full and then clamped at the wire.
func sessionPromptCacheKey(model llm.ProviderConfig, sessionID string) string {
	digest := sha256.Sum256([]byte(model.BaseURL + "\x00" + model.Model))
	key := fmt.Sprintf("ceraa-%x", digest[:8])
	if sessionID == "" {
		return key
	}
	return key + "-" + shortHash(sessionID)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:6])
}

func New(cfg RunnerConfig, store *sessionstore.Store, reg *tool.Registry) *Runner {
	if cfg.Model.BaseURL == "" {
		cfg.Model = llm.DefaultConfig()
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 8192
	}
	if cfg.MaxToolResultChars <= 0 {
		cfg.MaxToolResultChars = 12000
	}
	if cfg.MaxConcurrentTools <= 0 {
		cfg.MaxConcurrentTools = 8
	}
	if cfg.GoalDir == "" {
		cfg.GoalDir = "sessions"
	}
	// The level is clamped once, here, so every request the runner makes carries
	// a level the model said it accepts. It is the only place the configured
	// level and the resolved vocabulary are both in hand, and doing it per
	// request would mean the transport guessing.
	cfg.Reasoning = llm.Thinking{Efforts: cfg.ReasoningEfforts, Default: cfg.ReasoningDefault, Known: len(cfg.ReasoningEfforts) > 0}.Resolved(cfg.Reasoning)
	if cfg.ToolOutputDir == "" {
		cfg.ToolOutputDir = defaultToolOutputDir(cfg.GoalDir)
	}
	opMgr := cfg.OperationPool
	if opMgr == nil {
		opMgr = operation.NewWithBuffer(&toolRunner{reg: reg}, 4096)
	}
	opMgr.SetConcurrencyLimit(cfg.MaxConcurrentTools)
	catalog := llm.NewCatalogClient(cfg.ModelCatalogURL, cfg.ModelCatalogCache, cfg.ModelCatalogRefresh)
	r := &Runner{cfg: cfg, store: store, reg: reg, opMgr: opMgr, catalog: catalog, evaluation: cfg.EvaluationStore, cb: cfg.Callbacks}
	r.delivery = memory.ParseDelivery(cfg.MemoryDelivery)
	// Any one of the three is enough to build the state. Keying it on
	// MemoryEntries alone meant a caller configured with a bond and no store -
	// which is a legal configuration, since MemoryEntries is documented as
	// optional - got no sidekick at all, and the bond went with it. That is the
	// same silent drop, one indirection further from the cause.
	if r.delivery == memory.DeliverySidekick && (cfg.MemoryEntries != nil || cfg.BondFunc != nil || cfg.MemoryFile != nil) {
		r.sidekick = newSidekickState(cfg.MemoryEntries, cfg.BondFunc, cfg.WorkDir)
		// Attached after construction because the log is a replacement for the
		// store rather than a fourth argument to it: a caller that configures both
		// is mid-migration, and the log is the half that keeps the prompt cache.
		r.sidekick.useMemoryFile(cfg.MemoryFile, cfg.MemoryTail)
	}
	// The instruction source is built here, once, beside the sidekick and not
	// inside it. Both are per-run holders of delivery state, and this is the one
	// place that knows a run has started.
	//
	// Built from the loader rather than from an already-compiled snapshot so the
	// file is read here, at the start of the run, and never again: the holder
	// loads once on first use and then serves the same snapshot for every
	// remaining step. Nothing about delivery is allowed to depend on when the
	// rule file was last written.
	if cfg.InstructionRules != nil {
		// instructionMatcher is harness/memory's glob grammar, so a rule's scope
		// is answered by the same matcher a memory's applies is. newInstructionSource
		// substitutes this default when handed a nil matcher, so the runner passing
		// nil would still work - but passing it explicitly keeps the dependency
		// visible at the construction site rather than three files away.
		r.instructions = newInstructionSource(cfg.InstructionRules, instructionMatcher)
	}
	// Seed the budget from what is known without a network call, and push it into
	// the compactor now. The run refines this from the catalog, but between
	// construction and that lookup the compactor must not be holding whatever
	// default it was constructed with.
	seed, seedSource := llm.ResolveContextWindow(cfg.ContextLimit, llm.ModelMetadata{})
	r.contextLimit, r.contextSource = seed, seedSource
	if cfg.Compactor != nil {
		cfg.Compactor.ContextLimit = seed
	}
	return r
}

type toolRunner struct {
	reg *tool.Registry
}

// defaultToolOutputDir keeps spooled tool output beside the sessions rather
// than in the OS temp dir. A pointer to a spool file is a promise the file
// will be there when the model follows it, and the temp dir is wiped on reboot
// and whenever the host feels like it. GoalDir is the sessions directory in
// every caller that sets one, so the spool sits next to the transcript that
// points at it. The store only ever reads *.json, so the subdirectory is
// invisible to it.
func defaultToolOutputDir(goalDir string) string {
	if trimmed := strings.TrimSpace(goalDir); trimmed != "" {
		return filepath.Join(trimmed, "tool-output")
	}
	return filepath.Join(os.TempDir(), "ceraa-tool-outputs")
}

func (r *toolRunner) RunTool(ctx context.Context, spec operation.Spec) (string, error) {
	t, ok := r.reg.Get(spec.Type)
	if !ok {
		return "", fmt.Errorf("unknown tool type: %s", spec.Type)
	}
	if runner, ok := t.(operation.ContextRunner); ok {
		return runner.RunContext(ctx, spec)
	}
	runner, ok := t.(tool.Runner)
	if !ok {
		return "", fmt.Errorf("tool %s does not support Run", spec.Type)
	}
	return runner.Run(spec)
}

func (r *Runner) Run(ctx context.Context, user string) (Result, error) {
	if r.store == nil {
		return Result{}, fmt.Errorf("session store is required")
	}
	if r.reg == nil {
		return Result{}, fmt.Errorf("tool registry is required")
	}
	// Installed once per run rather than at construction, because the runner is
	// the thing being constructed and the compactor arrives in the config ahead
	// of it. From here on, a compaction triggered anywhere inside this run
	// summarises over the same request the turns are sending.
	r.attachLiveRequest()
	release, err := r.store.AcquireRun(r.cfg.SessionID)
	if err != nil {
		return Result{}, fmt.Errorf("acquire session run: %w", err)
	}
	defer release()
	sess, err := r.store.Load(r.cfg.SessionID)
	if err != nil {
		return Result{}, fmt.Errorf("load session: %w", err)
	}
	if sessionstore.Recover(sess) {
		if err := r.store.Save(sess); err != nil {
			return Result{}, fmt.Errorf("recover session: %w", err)
		}
	}
	if len(sess.Messages) == 0 {
		sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleSystem, Content: r.systemPrompt()})
	} else if sess.Messages[0].Role == llm.RoleSystem && sess.Messages[0].Compaction == nil {
		// The system prompt carries the model-identity block, so a session opened
		// on one provider kept naming that provider after a switch: the stored copy
		// said Hermes while the run was on xAI. Nothing forced a rewrite, because
		// this was written only when the session was empty.
		//
		// Refreshed rather than appended, and only when it actually differs. The
		// runner strips this message from every request (see messages), so the
		// stored copy is a record rather than an input, which is what makes a stale
		// record safe to correct and a rewritten one free of cache cost. Guarding on
		// inequality keeps an unchanged prompt from rewriting the file every turn.
		if current := r.systemPrompt(); sess.Messages[0].Content != current {
			sess.Messages[0].Content = current
		}
	}
	if user != "" {
		sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleUser, Content: user})
	}
	runID := fmt.Sprintf("run_%d", time.Now().UnixNano())
	now := time.Now()
	sess.Runs = append(sess.Runs, sessionstore.Run{ID: runID, User: user, Status: sessionstore.RunRunning, CreatedAt: now, UpdatedAt: now})
	if err := r.store.Save(sess); err != nil {
		return Result{}, fmt.Errorf("save run: %w", err)
	}
	var runUsage llm.UsageInfo
	var runBreakdown llm.TokenBreakdown
	// runRequests is the per-request split, persisted with the run on finish.
	runRequests := make([]llm.RequestTokenBreakdown, 0, 16)
	// runTrust is the verdict on whether the provider's counts describe these
	// requests. Held per run so cost and the reported cache ratio can be withheld
	// when they do not. Starts trusted because an unmeasured run is not evidence
	// of a bad provider.
	var runTrust = llm.UsageTrust{Trusted: true}
	var runCost llm.CostInfo
	if r.catalog == nil {
		r.catalog = llm.NewCatalogClient(r.cfg.ModelCatalogURL, r.cfg.ModelCatalogCache, r.cfg.ModelCatalogRefresh)
	}
	pricingMetadata, pricingErr := r.catalog.Lookup(ctx, r.cfg.Model.Model)
	r.recalibrateContextLimit(pricingMetadata, pricingErr)
	pricingStatus := pricingMetadata.PricingStatus(pricingErr)
	pricingUsable := pricingStatus == "current"

	// accounting owns the run's totals and the decision about whether they may be
	// priced. See runAccounting for why an untrustworthy run yields no cost.
	accounting := newRunAccounting(pricingMetadata, pricingUsable)
	for i := range sess.Runs {
		if sess.Runs[i].ID == runID {
			sess.Runs[i].PricingModel = r.cfg.Model.Model
			sess.Runs[i].ProviderHost = r.cfg.Model.BaseURL
			sess.Runs[i].PricingStatus = pricingStatus
			sess.Runs[i].PricingFetchedAt = pricingMetadata.PricingFetchedAt
			sess.Runs[i].ModelContextWindow = pricingMetadata.ContextWindow
			sess.Runs[i].ModelMaxTokens = pricingMetadata.MaxTokens
			sess.Runs[i].ReasoningOptions = pricingMetadata.ReasoningOptions
		}
	}
	if err := r.store.Save(sess); err != nil {
		return Result{PricingStatus: pricingStatus}, fmt.Errorf("save pricing metadata: %w", err)
	}
	if r.cfg.Compactor != nil {
		r.cfg.Compactor.OnUsage = func(delta llm.UsageInfo) {
			accounting.Add(delta, runBreakdown, nil)
			runUsage, runCost, runTrust = accounting.Usage(), accounting.Cost(), accounting.Trust()
			if r.cb.OnUsage != nil {
				r.cb.OnUsage(runUsage, runTrust)
			}
			if r.cb.OnCost != nil {
				r.cb.OnCost(runCost, pricingStatus)
			}
		}
	}

	finish := func(status sessionstore.RunStatus, runErr error) error {
		for i := range sess.Runs {
			if sess.Runs[i].ID == runID {
				sess.Runs[i].Status = status
				sess.Runs[i].UpdatedAt = time.Now()
				sess.Runs[i].Usage = runUsage
				sess.Runs[i].Cost = runCost
				sess.Runs[i].InputBreakdown = runBreakdown
				sess.Runs[i].RequestBreakdown = runRequests
				sess.Runs[i].UsageTrust = runTrust
				sess.Runs[i].ProviderHost = r.cfg.Model.BaseURL
				sess.Runs[i].PricingStatus = pricingStatus
				sess.Runs[i].PricingFetchedAt = pricingMetadata.PricingFetchedAt
				if runErr != nil {
					sess.Runs[i].Error = runErr.Error()
				}
				// Recorded here rather than at start, because the counts are only
				// final once the run has finished delivering.
				//
				// This is the whole measurement story for now: a run record that says
				// which mode ran and what it delivered, so a later analysis can
				// compare outcomes instead of inferring the mode from the build.
				delivery := r.DeliveryReport()
				sess.Runs[i].MemoryDelivery = delivery.Mode
				sess.Runs[i].MemoryGeneralSent = delivery.GeneralInjected
				sess.Runs[i].MemoryTargetedSent = delivery.TargetedInjects
				sess.Runs[i].MemoryBelowGate = delivery.SuppressedByGate
				sess.Runs[i].MemoryBondSent = delivery.BondInjected
			}
		}
		if r.evaluation != nil {
			requests := 0
			for _, run := range sess.Runs {
				if run.ID == runID {
					requests = run.Requests
					break
				}
			}
			_ = r.evaluation.Record(evaluation.RunMetrics{
				ID:        runID,
				Success:   status == sessionstore.RunCompleted,
				Tokens:    runUsage.TotalTokens,
				Requests:  requests,
				Cost:      runCost.TotalUSD,
				CostKnown: runCost.Known,
			})
			if runCost.Known {
				if _, err := r.evaluation.Baseline("default"); errors.Is(err, evaluation.ErrBaselineNotFound) {
					_ = r.evaluation.SetBaselineFromRuns("default")
				}
			}
		}
		if err := r.store.Save(sess); err != nil {
			return err
		}
		if runErr != nil && r.cb.OnError != nil {
			r.cb.OnError(runErr)
		}
		return runErr
	}

	step := 0
	// Graduated request budget. Counted here because this is the one place every
	// turn passes through, including turns after compaction and goal continuations,
	// so a cap placed at the call site would miss them.
	soft, yieldAt, hardAt := r.budgetPoints()
	budgetWarned, budgetYielded := false, false
	goalUsageBase := 0
	goalContinuations := 0
	correctiveToolRetryUsed := false
	finalContentRetryUsed := false
	forceFinalResponse := false
	for {
		if soft > 0 {
			switch {
			case step >= hardAt:
				// Past the grace after being told to stop. Abandon it, but say so
				// rather than returning nothing, because the caller has to be able to
				// tell "this child gave up" from "this child finished".
				logger.Warn(fmt.Sprintf("run abandoned for %s: %d requests, hard limit %d", r.cfg.SessionID, step, hardAt))
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed,
					fmt.Errorf("abandoned after %d requests", step))
			case step >= yieldAt && !budgetYielded:
				// Force one final text-only answer. The point is to convert what was
				// found into a report before giving up on it.
				budgetYielded = true
				forceFinalResponse = true
				sess.Messages = append(sess.Messages, sessionstore.Message{
					Role:    llm.RoleUser,
					Content: "Stop working and report now: what you found, what you changed, and what is unfinished. Do not call more tools.",
				})
				if err := r.store.Save(sess); err != nil {
					return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
				}
			case step >= soft && !budgetWarned:
				budgetWarned = true
				notice := fmt.Sprintf("You have used %d requests in this run (soft budget: %d). "+
					"Wrap up now: finish the current step and give your final report. At %d requests the run is "+
					"stopped and asked to yield whatever it has.", step, soft, yieldAt)
				if r.cfg.BudgetNotice != nil {
					notice = r.cfg.BudgetNotice(step, soft)
				}
				sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleUser, Content: notice})
				if err := r.store.Save(sess); err != nil {
					return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
				}
			}
		}
		for i := range sess.Runs {
			if sess.Runs[i].ID == runID {
				sess.Runs[i].Step = step
				sess.Runs[i].Requests++
				sess.Runs[i].Usage = runUsage
				sess.Runs[i].Cost = runCost
				sess.Runs[i].InputBreakdown = runBreakdown
				sess.Runs[i].RequestBreakdown = runRequests
				sess.Runs[i].UsageTrust = runTrust
				sess.Runs[i].ProviderHost = r.cfg.Model.BaseURL
				sess.Runs[i].UpdatedAt = time.Now()
			}
		}
		if err := r.store.Save(sess); err != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}
		if err := r.handleInbox(ctx); err != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunInterrupted, err)
		}
		if r.cfg.Compactor != nil {
			forced := r.cfg.ForcedCompaction != nil && r.cfg.ForcedCompaction()
			if forced || r.cfg.Compactor.NeedsCompaction(sess) {
				if r.cb.OnCompactionStart != nil {
					r.cb.OnCompactionStart()
				}
				ok, err := r.cfg.Compactor.Compact(ctx, sess)
				runBreakdown.Compaction += r.cfg.Compactor.LastPromptTokens
				if r.cb.OnCompactionDone != nil {
					r.cb.OnCompactionDone(ok, err)
				}
				if err != nil {
					if forced {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, fmt.Errorf("compaction: %w", err))
					}
				} else if ok {
					for i := range sess.Runs {
						if sess.Runs[i].ID == runID {
							sess.Runs[i].Usage = runUsage
							sess.Runs[i].InputBreakdown = runBreakdown
							sess.Runs[i].RequestBreakdown = runRequests
							sess.Runs[i].UsageTrust = runTrust
							sess.Runs[i].ProviderHost = r.cfg.Model.BaseURL
						}
					}
					if err := r.store.Save(sess); err != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
					}
				}
			}
		}
		modelConfig := r.cfg.Model
		if r.cfg.ResolveModel != nil {
			resolved, resolveErr := r.cfg.ResolveModel(ctx)
			if resolveErr != nil {
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, fmt.Errorf("resolve provider credentials: %w", resolveErr))
			}
			modelConfig = resolved
		}
		// Applied after resolution, because the provider config is rebuilt per run
		// and a hook set on the caller's copy would not survive that.
		if r.cb.OnRetry != nil {
			modelConfig.OnRetry = r.cb.OnRetry
		}
		client := llm.New(modelConfig)
		toolDefs := r.toolDefs()
		requestToolChoice := ""
		if forceFinalResponse {
			requestToolChoice = "none"
		}
		request := llm.StreamRequest{
			Messages:        r.messages(sess, toolDefs),
			Tools:           toolDefs,
			Reason:          r.cfg.Reasoning,
			MaxTokens:       r.cfg.MaxOutputTokens,
			ToolChoice:      requestToolChoice,
			PromptCacheKey:  sessionPromptCacheKey(modelConfig, r.cfg.SessionID),
			SystemCacheSeam: r.systemCacheSeam(),
			SessionID:       r.cfg.SessionID,
		}
		breakdown := llm.EstimateTokenBreakdown(request.Messages, request.Tools)
		countRequestBeforeUse(&runBreakdown, breakdown)
		if traceCache {
			logCachePrefix(step, request.Messages, request.Tools, breakdown)
		}
		// Kept per request rather than only summed into the run total. The sum
		// sizes the problem; the list says which request created it, which is what
		// makes an opportunity actionable instead of merely large.
		runRequests = append(runRequests, llm.RequestTokenBreakdown{Step: step, Breakdown: breakdown})
		var content strings.Builder
		var reasoning strings.Builder
		// How much of the reasoning has already been handed to the callback, so
		// the next slice can be taken rather than the whole thing resent.
		reasoningSent := 0
		var reasoningDetails []json.RawMessage
		var calls []llm.StreamedToolCall
		var usage llm.UsageInfo
		err = client.Stream(ctx, request, func(delta llm.ProviderResponse) {
			if delta.Content != "" {
				content.WriteString(delta.Content)
				if r.cb.OnContent != nil {
					r.cb.OnContent(delta.Content)
				}
			}
			if delta.Reasoning != "" {
				reasoning.WriteString(delta.Reasoning)
				// Only the newly arrived part, matching OnContent above. This passed
				// the whole accumulated string, and every consumer appends what it
				// is handed, so each delta reprinted everything seen so far: one
				// greeting became a screenful of the same sentence, each repetition
				// one delta longer than the last.
				if r.cb.OnReasoning != nil && reasoning.Len() > reasoningSent {
					r.cb.OnReasoning(reasoning.String()[reasoningSent:])
					reasoningSent = reasoning.Len()
				}
			}
			if len(delta.ReasoningDetails) > 0 {
				reasoningDetails = append(reasoningDetails, append(json.RawMessage(nil), delta.ReasoningDetails...))
			}
			if len(delta.ToolCalls) > 0 {
				calls = llm.MergeStreamedToolCalls(calls, delta.ToolCalls)
			}
			if delta.Usage != nil {
				usage.Add(*delta.Usage)
				accounting.Add(*delta.Usage, runBreakdown, &delta.UsageRaw)
				runUsage, runCost, runTrust = accounting.Usage(), accounting.Cost(), accounting.Trust()
				if r.cb.OnUsage != nil {
					r.cb.OnUsage(runUsage, runTrust)
				}
			}
		})
		if r.cb.OnCost != nil {
			r.cb.OnCost(runCost, pricingStatus)
		}
		for i := range sess.Runs {
			if sess.Runs[i].ID == runID {
				sess.Runs[i].Usage = runUsage
				sess.Runs[i].Cost = runCost
				sess.Runs[i].InputBreakdown = runBreakdown
				sess.Runs[i].RequestBreakdown = runRequests
				sess.Runs[i].UsageTrust = runTrust
				sess.Runs[i].ProviderHost = r.cfg.Model.BaseURL
			}
		}
		if saveErr := r.store.Save(sess); saveErr != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
		}
		if err != nil {
			if r.cfg.Compactor != nil && (r.cfg.Compactor.IsOverflow(sess) || llm.IsContextOverflow(err)) {
				if r.cb.OnCompactionStart != nil {
					r.cb.OnCompactionStart()
				}
				ok, compactErr := r.cfg.Compactor.Compact(ctx, sess)
				runBreakdown.Compaction += r.cfg.Compactor.LastPromptTokens
				if r.cb.OnCompactionDone != nil {
					r.cb.OnCompactionDone(ok, compactErr)
				}
				if compactErr == nil && ok {
					for i := range sess.Runs {
						if sess.Runs[i].ID == runID {
							sess.Runs[i].Usage = runUsage
							sess.Runs[i].InputBreakdown = runBreakdown
							sess.Runs[i].RequestBreakdown = runRequests
							sess.Runs[i].UsageTrust = runTrust
							sess.Runs[i].ProviderHost = r.cfg.Model.BaseURL
						}
					}
					if saveErr := r.store.Save(sess); saveErr != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
					}
					continue
				}
			}
			status := sessionstore.RunFailed
			if ctx.Err() != nil {
				status = sessionstore.RunCanceled
			}
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(status, err)
		}

		if forceFinalResponse {
			calls = nil
			if llm.ContainsTextToolCall(content.String()) {
				content.Reset()
			}
		}
		if len(calls) == 0 && !forceFinalResponse {
			if textCall, ok := llm.ParseTextToolCall(content.String()); ok {
				calls = []llm.StreamedToolCall{textCall}
				content.Reset()
			} else if llm.ContainsTextToolCall(content.String()) {
				content.Reset()
				if !correctiveToolRetryUsed {
					correctiveToolRetryUsed = true
					sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleUser, Content: "Your previous response emitted a tool call as text. Use the actual tool-calling mechanism and do not return tool-call tags in content."})
					if err := r.store.Save(sess); err != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
					}
					step++
					continue
				}
			}
		}
		// This step is continuing work rather than answering, so anything it put on
		// the content channel was reasoning. Told before the calls are dispatched so
		// a consumer can correct its own state while the step is still in hand.
		//
		// Only when there ARE calls. Firing this unconditionally was firing it on the
		// final step too, where the content channel was carrying the answer and not
		// reasoning: the renderer folded the answer into a thought and released the
		// message the answer was streaming in, so the final publish had nothing to
		// edit and sent the same text as a second message. It only reproduced when the
		// last step also carried tool calls, which is why it looked intermittent.
		if len(calls) > 0 && r.cb.OnStepReasoningOnly != nil {
			r.cb.OnStepReasoningOnly()
		}
		calls = llm.EnsureToolCallIDs(calls, fmt.Sprintf("%s_%d", runID, step))
		calls = r.uniqueToolCallIDs(calls, sess)
		for i := range calls {
			if repaired, changed := llm.RepairToolArgumentsJSON(calls[i].Arguments); changed {
				calls[i].Arguments = repaired
			}
		}
		if len(calls) == 0 {
			answer := content.String()
			rescued := false
			if strings.TrimSpace(answer) == "" && finalContentRetryUsed && forceFinalResponse {
				if extracted, ok := llm.ExtractSingleArgValue(reasoning.String()); ok {
					answer = extracted
					rescued = true
				}
			}
			if strings.TrimSpace(answer) == "" {
				if !finalContentRetryUsed {
					finalContentRetryUsed = true
					forceFinalResponse = true
					sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleUser, Content: "Your previous response contained reasoning but no final answer. Return the final answer only. Do not call tools or include reasoning."})
					if err := r.store.Save(sess); err != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
					}
					step++
					continue
				}
				answer = r.emptyFinalFallback(sess, runID)
			}
			forceFinalResponse = false
			messageReasoning := reasoning.String()
			if rescued {
				messageReasoning = ""
			}
			messageReasoningDetails := llm.MergeReasoningDetails(reasoningDetails)
			sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleAssistant, Content: answer, Reasoning: messageReasoning, ReasoningDetails: messageReasoningDetails, Usage: usage})
			if err := r.store.Save(sess); err != nil {
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
			}
			if !r.cfg.StopWhenIdle && goalContinuations < 100 {
				if goalStore, goalErr := goal.New(r.cfg.GoalDir); goalErr == nil {
					if current, exists, loadErr := goalStore.Load(r.cfg.SessionID); loadErr == nil && exists && current.Status == goal.StatusActive {
						current.TokensUsed += runUsage.TotalTokens - goalUsageBase
						goalUsageBase = runUsage.TotalTokens
						current.Iterations++
						if current.TokenBudget > 0 && current.TokensUsed >= current.TokenBudget {
							current.Status = goal.StatusBlocked
							current.Reason = "token budget exhausted"
							_, _ = goalStore.Save(r.cfg.SessionID, current)
						} else {
							if _, saveErr := goalStore.Save(r.cfg.SessionID, current); saveErr == nil {
								goalContinuations++
								sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleUser, Content: "Continue working autonomously on the active goal. Make progress, verify the result, and call update_goal when it is completed or blocked. Active goal: " + current.Objective})
								if saveErr := r.store.Save(sess); saveErr != nil {
									return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
								}
								step++
								continue
							}
						}
					}
				}
			}
			if r.cb.OnDone != nil {
				r.cb.OnDone(answer)
			}
			return Result{Final: answer, Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunCompleted, nil)
		}

		assistant := sessionstore.Message{Role: llm.RoleAssistant, Content: content.String(), Reasoning: reasoning.String(), ReasoningDetails: llm.MergeReasoningDetails(reasoningDetails), Usage: usage, ToolCalls: make([]sessionstore.ToolCallRef, 0, len(calls))}
		for _, call := range calls {
			assistant.ToolCalls = append(assistant.ToolCalls, sessionstore.ToolCallRef{ID: call.ID, Name: call.Name, Args: call.Arguments, ThoughtSignature: call.ThoughtSignature})
		}
		sess.Messages = append(sess.Messages, assistant)
		// From here the assistant has declared its tool calls, so every exit has to
		// leave an answer for each one. Both failures below used to return with the
		// calls unanswered, and a request carrying a tool_call with no matching tool
		// result is rejected outright, so one lost turn made the session unusable
		// from the next one onward.
		if err := r.store.Save(sess); err != nil {
			r.recoverUnanswered(sess, runID, calls)
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}

		prepared, results, corrective, err := r.prepareTools(ctx, sess, runID, calls)
		if err != nil {
			r.recoverUnanswered(sess, runID, calls)
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}
		if corrective {
			if correctiveToolRetryUsed {
				forceFinalResponse = true
			} else {
				correctiveToolRetryUsed = true
			}
		}
		pending := make(map[string]struct{}, len(prepared))
		// The tool name is only known here, while the call is still in hand, and
		// the result arrives as a bare operation id. Without this the tally could
		// only count failures, never the denominator they are rates against.
		toolByCall := make(map[string]string, len(prepared))
		for _, item := range prepared {
			pending[item.call.ID] = struct{}{}
			toolByCall[item.call.ID] = item.tool.Name()
		}
		timeout := time.NewTimer(180 * time.Second)
		for len(pending) > 0 {
			select {
			case result := <-r.opMgr.Results():
				if _, ok := pending[result.OpID]; !ok {
					continue
				}
				delete(pending, result.OpID)
				// Every tool outcome reaches this point, so it is the one place the
				// tally can be complete: a call counted whatever happened to it, and
				// a failure filed under a class that separates a missing target from
				// a harness bug.
				if name := toolByCall[result.OpID]; name != "" {
					recordToolCall(name)
					if result.Error != "" {
						recordToolError(name, result.Error)
					}
				}
				// Annotated only on the model-facing path. persistResult keeps the
				// bare result in the session file, because that record is what the
				// tool actually returned and a later reader should not have to know
				// which memories happened to be in the store at the time, or which
				// of the project's own rules had fired by then.
				//
				// The order of the two note calls is fixed and not cosmetic: bond
				// and memory before project rules, because a reader - and a model -
				// takes identity and project context before project-specific rules
				// about the file in front of it. Neither is allowed to see the
				// other's work, which is what keeps them separately fixable.
				//
				// This line runs BEFORE persistResult, on purpose. The note is
				// rendered from the result that just arrived, not from the session
				// record, and rendering after the persist would read an operation
				// row whose Status and Error had already been rewritten from this
				// same event - two answers to "did this fail" taken a few lines
				// apart.
				results[result.OpID] = r.resultText(runID, result) + r.sidekickNote(sess, result) + r.instructionNote(sess, result)
				if err := r.persistResult(sess, runID, result); err != nil {
					return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
				}
				r.noteFileMutation(sess, result)
				r.notifyToolDone(sess, result)
			case <-ctx.Done():
				r.opMgr.CancelAll()
				for id := range pending {
					results[id] = "Error: operation canceled before a durable result was recorded"
					if err := r.persistUnknown(sess, runID, id, ctx.Err()); err != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
					}
				}
				r.appendResults(sess, calls, results)
				if saveErr := r.store.Save(sess); saveErr != nil {
					return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
				}
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunCanceled, ctx.Err())
			case <-timeout.C:
				r.opMgr.CancelAll()
				for id := range pending {
					results[id] = "Error: operation outcome is unknown after timeout"
					if err := r.persistUnknown(sess, runID, id, fmt.Errorf("operation timeout")); err != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
					}
				}
				r.appendResults(sess, calls, results)
				if saveErr := r.store.Save(sess); saveErr != nil {
					return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
				}
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, fmt.Errorf("tool execution timeout"))
			}
		}
		if !timeout.Stop() {
			select {
			case <-timeout.C:
			default:
			}
		}
		r.appendResults(sess, calls, results)
		if err := r.store.Save(sess); err != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}
		step++
	}
}

func (r *Runner) handleInbox(ctx context.Context) error {
	if r.cfg.Inbox == nil {
		return nil
	}
	select {
	case sub := <-r.cfg.Inbox.Output():
		return handleInboxEvent(sub, nil, nil, &r.cfg, r.cb)
	default:
		return nil
	}
}

func (r *Runner) toolDefs() []llm.ToolDef {
	if r.reg == nil {
		return nil
	}
	// Offered, not All. A tool kept registered so old history can still execute
	// it must not appear in the catalogue, because showing the model both shapes
	// teaches it the one that caused the problem.
	items := r.reg.Offered()
	definitions := make([]llm.ToolDef, 0, len(items))
	for _, item := range items {
		definitions = append(definitions, llm.ToolDef{Name: item.Name(), Description: item.Description(), Parameters: item.Schema()})
	}
	return definitions
}

func (r *Runner) uniqueToolCallIDs(calls []llm.StreamedToolCall, sess *sessionstore.Session) []llm.StreamedToolCall {
	used := make(map[string]struct{})
	for _, message := range sess.Messages {
		for _, call := range message.AllToolCalls() {
			used[call.ID] = struct{}{}
		}
		if message.Role == llm.RoleTool {
			used[message.ToolCallID] = struct{}{}
		}
	}
	for i := range calls {
		base := calls[i].ID
		if base == "" {
			base = fmt.Sprintf("call_%d", i)
		}
		candidate := base
		for suffix := 1; ; suffix++ {
			if _, exists := used[candidate]; !exists {
				break
			}
			candidate = fmt.Sprintf("%s_%d", base, suffix)
		}
		calls[i].ID = candidate
		used[candidate] = struct{}{}
	}
	return calls
}

func (r *Runner) messages(sess *sessionstore.Session, tools []llm.ToolDef) []llm.Item {
	history := *sess
	if len(history.Messages) > 0 && history.Messages[0].Role == llm.RoleSystem && history.Messages[0].Compaction == nil {
		history.Messages = history.Messages[1:]
	}
	budget := r.effectiveContextLimit()
	limit := budget - r.cfg.MaxOutputTokens - llm.EstimateToolDefinitionTokens(tools)
	if limit <= 0 {
		limit = budget - r.cfg.MaxOutputTokens
	}
	if limit <= 0 {
		// The model window is smaller than our own reserve. Budget whatever is
		// left rather than a remembered constant that may be larger still.
		limit = budget / 2
	}
	request := contextbuilder.New(&history, r.systemPrompt()).BuildWithLimit(limit)
	request.Messages = normalizeDispatcherMessages(request.Messages)
	return request.Messages
}

// LiveRequest returns the request the running agent would send for this session
// right now: the same system prompt, the same pinned context, the same history
// and the same tool catalogue.
//
// It is what an in-band compaction appends its instruction to, so the summary
// comes from the agent that did the work, over the cache prefix the turn just
// paid to open. The fields a run needs but a summary does not - the reasoning
// level, the output cap - are deliberately left out here and set by the
// compactor.
func (r *Runner) LiveRequest(sess *sessionstore.Session) llm.StreamRequest {
	tools := r.toolDefs()
	return llm.StreamRequest{
		Messages:        r.messages(sess, tools),
		Tools:           tools,
		PromptCacheKey:  sessionPromptCacheKey(r.cfg.Model, r.cfg.SessionID),
		SystemCacheSeam: r.systemCacheSeam(),
		SessionID:       r.cfg.SessionID,
	}
}

// LiveRequestFor builds the request a runner with this config would send,
// without a runner.
//
// It exists for callers that only ever compact: the Mini App's button has no
// run in flight, and without this it is stuck on the cold summariser. The
// store is accepted because request assembly may one day need it; nil is fine
// today.
func LiveRequestFor(cfg RunnerConfig, reg *tool.Registry, store *sessionstore.Store) func(context.Context, *sessionstore.Session) llm.StreamRequest {
	runner := New(cfg, store, reg)
	return func(_ context.Context, sess *sessionstore.Session) llm.StreamRequest {
		return runner.LiveRequest(sess)
	}
}

// attachLiveRequest gives the compactor the runner's own request assembly, so a
// compaction triggered from inside a run produces the same bytes the run sends.
//
// It is done here rather than at construction because the compactor is built
// before the runner is - the config carries it in, and the runner is what turns
// that config into requests. A caller that already installed a builder keeps
// it, which is what lets a caller wrap or replace the default.
func (r *Runner) attachLiveRequest() {
	if r.cfg.Compactor == nil || r.cfg.Compactor.LiveRequest != nil {
		return
	}
	r.cfg.Compactor.LiveRequest = func(_ context.Context, sess *sessionstore.Session) llm.StreamRequest {
		return r.LiveRequest(sess)
	}
}

func normalizeDispatcherMessages(items []llm.Item) []llm.Item {
	callNames := make(map[string]string)
	for i := range items {
		switch items[i].Kind {
		case "tool_call":
			items[i] = normalizeLegacyRunToolCall(items[i])
			items[i] = splitLegacyBrowserCall(items[i])
			if items[i].Name != "" {
				callNames[items[i].CallID] = items[i].Name
			}
		case "tool_calls":
			for j := range items[i].ToolCalls {
				items[i].ToolCalls[j] = normalizeLegacyRunToolCallRef(items[i].ToolCalls[j])
				items[i].ToolCalls[j] = splitLegacyBrowserCallRef(items[i].ToolCalls[j])
				if items[i].ToolCalls[j].Name != "" {
					callNames[items[i].ToolCalls[j].ID] = items[i].ToolCalls[j].Name
				}
			}
		}
	}
	for i := range items {
		if items[i].Kind == "text" && items[i].Role == llm.RoleTool {
			if name := callNames[items[i].CallID]; name != "" {
				items[i].Name = name
			}
		}
	}
	return items
}

func normalizeLegacyRunToolCall(item llm.Item) llm.Item {
	if item.Name != "run" {
		return item
	}
	var envelope struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(item.Args), &envelope); err != nil || envelope.Tool == "" || len(envelope.Arguments) == 0 {
		return item
	}
	item.Name = envelope.Tool
	item.Args = string(envelope.Arguments)
	return item
}

func normalizeLegacyRunToolCallRef(call llm.ToolCall) llm.ToolCall {
	if call.Name != "run" {
		return call
	}
	var envelope struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(call.Args), &envelope); err != nil || envelope.Tool == "" || len(envelope.Arguments) == 0 {
		return call
	}
	call.Name = envelope.Tool
	call.Args = string(envelope.Arguments)
	return call
}

// todayContext tells the model the current date.
//
// It was a literal "This is year 2026" in the prompt file, which is correct
// exactly until 1 January and silently wrong after that. The model also needs
// more than a year: "is that certificate expiring" is unanswerable without a
// month and day.
func todayContext() string {
	now := time.Now()
	return fmt.Sprintf("<current-date>\nToday is %s.\n</current-date>", now.Format("2006-01-02"))
}

// workspaceContext tells the model where its commands actually run.
//
// Every filesystem tool resolves a relative path against this directory, and the
// model is not told that anywhere: the convention is documented for us in
// AGENTS.md and appears in no prompt the model reads. So it guesses, and a
// plausible guess looks authoritative until the tool answers "no such file or
// directory". Observed: asked to review a project, MiniMax reached for
// /home/assistant/auxlo-neo/… because that is a home directory that exists,
// while the project was at /workspace/auxlo-neo.
//
// The directory is whatever this session was given — each chat may select its own
// project — so nothing here is pinned to a particular repository. Naming it alone
// is also not enough, because the model has a strong prior that files live under
// $HOME, which is what produced the observed failure.
func (r *Runner) workspaceContext() string {
	workdir := strings.TrimSpace(r.cfg.WorkDir)
	if workdir == "" {
		return ""
	}
	return fmt.Sprintf(`<workspace>
Commands and file paths run in %s.
- A relative path resolves against that directory. Prefer relative paths.
- Do not invent absolute paths, and do not assume files live under your home directory.
- To find something, list the directory before reading, rather than guessing a path.
</workspace>`, workdir)
}

func (r *Runner) systemPrompt() string {
	parts, _ := r.systemPromptParts()
	return strings.Join(parts, "\n\n")
}

// systemPromptParts returns the ordered prompt sections and how many of them are
// stable.
//
// The count is returned rather than inferred from an index because the leading
// identity block is optional: with it absent the date moves from index 2 to index
// 1, and an index-based rule then caches the date and re-reads the entire prefix
// every time the day changes.
func (r *Runner) systemPromptParts() ([]string, int) {
	parts := []string{r.cfg.SystemPrompt}
	// Identity comes before the vendored prompt text: the model has to be able to
	// answer "which model am I" from its own instructions rather than guessing,
	// and it cannot know that unless it is told.
	identityAdded := false
	if identity := strings.TrimSpace(r.modelIdentity()); identity != "" {
		parts = append([]string{identity}, parts...)
		identityAdded = true
	}
	// Order is by how rarely a part changes, least volatile first, because the
	// seam below makes this a hard boundary rather than a gradient.
	//
	// It was the other way round, and the ordering reasoning in the comment above
	// was about the wrong thing: placing a volatile fact late inside the cached
	// prefix does cost less, but anything past the seam is not cached at all, so
	// "late" there means "never". The workspace and the approved skills were
	// therefore paying full price on every request while changing monthly and on
	// approval respectively, sitting downstream of a date that changes daily.
	//
	// Moving them ahead of the seam puts them in the cache. The cost is that
	// switching project invalidates the prefix, which is a rare event against a
	// saving on every request.
	stableCount := 1
	if identityAdded {
		stableCount = 2
	}
	if workspace := r.workspaceContext(); workspace != "" {
		parts = append(parts, workspace)
		stableCount++
	}
	if strings.TrimSpace(r.cfg.SkillContext) != "" {
		// The preamble is part of the same stable part, not a separate one, so
		// the instruction to read a skill and the list of skills it applies to
		// cannot be cached apart from each other.
		parts = append(parts, skills.CatalogPreamble+"\n\n"+strings.TrimSpace(r.cfg.SkillContext))
		stableCount++
	}
	// DeliveryCached puts memory ahead of the seam. That is the one mode in
	// which memory is always visible, including on turns with no tool call, and
	// it is only safe here because the extractor writes memory at compaction
	// rather than mid-session: a change rebuilds the prefix, so doing it during
	// a turn would re-bill the whole cache every time.
	if r.delivery == memory.DeliveryCached {
		if rendered := r.memoryBlock(); rendered != "" {
			parts = append(parts, rendered)
			stableCount++
		}
	}
	// The date changes daily and memory changes whenever the agent writes, so both
	// belong past the seam, where a change costs only the part that changed.
	if date := todayContext(); date != "" {
		parts = append(parts, date)
	}
	if rendered := r.memoryContext(); rendered != "" {
		parts = append(parts, rendered)
	}
	return parts, stableCount
}

// systemCacheSeam is the byte offset in the system prompt where stable text ends
// and volatile text begins.
//
// The runner is the only place that knows the seam, because only it knows which
// parts change: identity and the vendored prompt do not, while the date, approved
// skills and live memory do. Getting this wrong in either direction is expensive
// rather than merely wrong, since the system prompt is under a thousand tokens and
// a breakpoint on a volatile block re-reads the whole prefix after every edit.
func (r *Runner) systemCacheSeam() int {
	parts, stableCount := r.systemPromptParts()
	stable := 0
	for i := 0; i < stableCount && i < len(parts); i++ {
		stable += len(parts[i])
		if i < len(parts)-1 {
			stable += 2 // the join separator in systemPrompt
		}
	}
	return stable
}

// promptCacheKey is the per-run accessor used by tests and diagnostics. It
// includes the session, because the request path does; a diagnostic that
// disagreed with the wire would be worse than none.
func (r *Runner) promptCacheKey() string {
	return sessionPromptCacheKey(r.cfg.Model, r.cfg.SessionID)
}

// modelIdentity names the model and provider serving this run, resolved once and
// then held.
//
// ResolveModel is consulted rather than the static Model config because the
// provider is resolved per run: a session that switches provider mid-flight would
// otherwise be told it is the model it started on.
//
// It is memoised rather than recomputed per step for two reasons. ResolveModel can
// read the credential store, so a long run paid a store read on every step. More
// importantly, the error fallback silently renders the static config's host
// instead of the resolved one, so a transient resolution failure would rewrite the
// first token of the system prompt mid-run and invalidate the provider's cached
// prefix from byte zero. Pinning makes the block provably constant within a run.
//
// The block stays at the front of the prompt on purpose. Moving it later would
// not help the cache at all: a provider or model change invalidates the cache
// regardless, because the model is in the request body.
func (r *Runner) modelIdentity() string {
	r.identityOnce.Do(func() { r.identity = r.resolveModelIdentity() })
	return r.identity
}

func (r *Runner) resolveModelIdentity() string {
	model := r.cfg.Model.Model
	provider := r.cfg.Model.BaseURL
	codex := r.cfg.Model.CodexNative
	// Resolved once: ResolveModel can read the credential store, so calling it
	// twice per run does real work twice.
	if r.cfg.ResolveModel != nil {
		if resolved, err := r.cfg.ResolveModel(context.Background()); err == nil {
			if strings.TrimSpace(resolved.Model) != "" {
				model = resolved.Model
			}
			if strings.TrimSpace(resolved.BaseURL) != "" {
				provider = resolved.BaseURL
			}
			codex = resolved.CodexNative
		}
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	var out strings.Builder
	out.WriteString("<model-identity>\n")
	out.WriteString("You are running as model `" + model + "`")
	if codex {
		out.WriteString(" on the ChatGPT Codex subscription")
	} else if host := providerHost(provider); host != "" {
		out.WriteString(" served by " + host)
	}
	out.WriteString(".\n")
	out.WriteString("Answer questions about which model or provider is running from this, and do not guess. If a tool result contradicts it, trust this line and say so.\n")
	out.WriteString("</model-identity>")
	return out.String()
}

// providerHost reduces a base URL to a host for display.
func providerHost(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	rest := trimmed
	if index := strings.Index(rest, "://"); index >= 0 {
		rest = rest[index+3:]
	}
	if index := strings.IndexAny(rest, "/?#"); index >= 0 {
		rest = rest[:index]
	}
	return rest
}

func toolCatalogArguments(schema map[string]any) string {
	properties, _ := schema["properties"].(map[string]any)
	required := make(map[string]bool)
	switch values := schema["required"].(type) {
	case []string:
		for _, key := range values {
			required[key] = true
		}
	case []any:
		for _, value := range values {
			if key, ok := value.(string); ok {
				required[key] = true
			}
		}
	}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	arguments := make([]string, 0, len(keys))
	for _, key := range keys {
		argumentType := "value"
		if property, ok := properties[key].(map[string]any); ok {
			if name, ok := property["type"].(string); ok && name != "" {
				argumentType = name
			}
		}
		if !required[key] {
			argumentType += "?"
		}
		arguments = append(arguments, key+": "+argumentType)
	}
	return "{" + strings.Join(arguments, ", ") + "}"
}

type preparedTool struct {
	call    llm.StreamedToolCall
	spec    operation.Spec
	tool    tool.Tool
	args    map[string]any
	rawArgs string
}

var canonicalToolAliases = map[string]string{
	"cat":            "read",
	"read_file":      "read",
	"fs_read":        "read",
	"workspace_read": "read",
	"shell":          "bash",
	"exec":           "bash",
	"file_write":     "write",
	"file_edit":      "edit_line",
	"edit":           "edit_line",
	"apply_patch":    "batch_edit_file",
	"web_fetch":      "webfetch",
}

var canonicalToolArgumentAliases = map[string]map[string]string{
	"read":            {"file": "path", "filename": "path", "file_path": "path"},
	"write":           {"file": "path", "filename": "path", "file_path": "path"},
	"edit_line":       {"file": "path", "filename": "path", "file_path": "path"},
	"batch_edit_file": {"file": "path", "filename": "path", "file_path": "path"},
	"bash":            {"cmd": "command", "script": "command", "shell_command": "command"},
	"webfetch":        {"uri": "url", "link": "url"},
}

// Sanitize the name every tool call carries before the registry lookup, here
// and nowhere else.
//
// This function is the single funnel every tool call on every transport passes
// through before lookup: direct calls, envelope calls, retries, replays. A
// model that fuses two calls into one name, leaks <tool_call:...> framing tags
// into it, or echoes the display spelling with spaces reaches the registry as
// garbage, and garbage here means every tool fails the same way. Sanitizing at
// this one point covers all current tools and every tool registered in future:
// a new tool needs a Register call and nothing else, and its garbled names
// resolve the day it exists. Do NOT add per-tool or per-transport name handling
// at call sites; it drifts, it duplicates, and the last copy someone added for
// one model is the one that breaks the next. Future sessions take note: if a
// new failure shape appears, extend SanitizeToolCallNameCandidates in
// harness/llm/model.go and extend the test next to it, not a branch here.
func canonicalToolName(reg *tool.Registry, requested string) string {
	requested = strings.TrimSpace(requested)
	if _, ok := reg.Get(requested); ok {
		return requested
	}
	if canonical, ok := canonicalToolAliases[requested]; ok {
		if _, active := reg.Get(canonical); active {
			return canonical
		}
	}
	// The name the model sent matches nothing: it may be fused, tagged, or
	// spaced. Each candidate is tried as a name and as an alias, first
	// registered hit wins, so get_goal</tool_call:6124c78e>...bash resolves
	// to get_goal while a genuinely unknown name still falls through to the
	// unknown-tool error below.
	for _, candidate := range llm.SanitizeToolCallNameCandidates(requested) {
		if _, ok := reg.Get(candidate); ok {
			return candidate
		}
		if canonical, ok := canonicalToolAliases[candidate]; ok {
			if _, active := reg.Get(canonical); active {
				return canonical
			}
		}
	}
	return requested
}

func repairToolArguments(toolName string, args map[string]any) bool {
	aliases, ok := canonicalToolArgumentAliases[toolName]
	if !ok {
		return false
	}
	changed := false
	keys := make([]string, 0, len(aliases))
	for alias := range aliases {
		keys = append(keys, alias)
	}
	sort.Strings(keys)
	for _, alias := range keys {
		value, exists := args[alias]
		if !exists {
			continue
		}
		canonical := aliases[alias]
		if _, exists := args[canonical]; !exists {
			args[canonical] = value
		}
		delete(args, alias)
		changed = true
	}
	return changed
}

func activeToolNames(reg *tool.Registry) string {
	items := reg.All()
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name())
	}
	return strings.Join(names, ", ")
}

// emptyFinalFallback reports what a run actually did when the model ends
// silent: no text, retry exhausted. Tool calls that completed are progress,
// and telling the user nothing happened when the transcript proves otherwise
// reads as the agent dying mid-task. The fallback names the completed calls
// and carries the latest result excerpt; a run with no completed calls keeps
// the honest nothing-happened line.
func (r *Runner) emptyFinalFallback(sess *sessionstore.Session, runID string) string {
	var names []string
	seen := map[string]bool{}
	latest := ""
	for _, op := range sess.Operations {
		if op.RunID != runID || op.Status != sessionstore.StatusCompleted {
			continue
		}
		if !seen[op.Tool] {
			seen[op.Tool] = true
			names = append(names, op.Tool)
		}
		if strings.TrimSpace(op.Output) != "" {
			latest = op.Output
		}
	}
	if len(names) == 0 {
		return "The model completed without producing a final response."
	}
	count := 0
	for _, op := range sess.Operations {
		if op.RunID == runID && op.Status == sessionstore.StatusCompleted {
			count++
		}
	}
	answer := fmt.Sprintf("Worked through %d tool call(s) (%s) with no final summary.", count, strings.Join(names, ", "))
	if excerpt := strings.TrimSpace(latest); excerpt != "" {
		if len(excerpt) > 400 {
			excerpt = excerpt[:400] + "..."
		}
		answer += " Latest result: " + excerpt
	}
	return answer
}

func (r *Runner) prepareTools(ctx context.Context, sess *sessionstore.Session, runID string, calls []llm.StreamedToolCall) ([]preparedTool, map[string]string, bool, error) {
	prepared := make([]preparedTool, 0, len(calls))
	results := make(map[string]string, len(calls))
	corrective := false
	for _, call := range calls {
		op := sessionstore.Operation{ID: call.ID, Tool: call.Name, Status: sessionstore.StatusPending, RunID: runID, ToolCallID: call.ID}
		sess.Operations = append(sess.Operations, op)
		results[call.ID] = ""
		toolName := canonicalToolName(r.reg, call.Name)
		args := make(map[string]any)
		if call.Name == "run" {
			if !json.Valid([]byte(call.Arguments)) {
				corrective = true
				results[call.ID] = "Error: invalid JSON arguments for run; use the run schema and canonical tool catalog"
				if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], "invalid JSON arguments"); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			var envelope struct {
				Tool      string         `json:"tool"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(call.Arguments), &envelope); err != nil || strings.TrimSpace(envelope.Tool) == "" || envelope.Arguments == nil {
				corrective = true
				results[call.ID] = "Error: run requires tool and arguments; use a canonical tool from the tool catalog with its listed argument keys"
				if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], "run requires tool and arguments"); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			toolName = canonicalToolName(r.reg, envelope.Tool)
			args = envelope.Arguments
		} else {
			if !json.Valid([]byte(call.Arguments)) {
				corrective = true
				results[call.ID] = "Error: invalid JSON arguments for " + toolName
				if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], "invalid JSON arguments"); err != nil {
					return nil, nil, false, err
				}
				continue
			}
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args == nil {
				corrective = true
				results[call.ID] = "Error: " + toolName + " requires a JSON object"
				if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], "arguments must be a JSON object"); err != nil {
					return nil, nil, false, err
				}
				continue
			}
		}
		repairToolArguments(toolName, args)
		rawArgs, err := json.Marshal(args)
		if err != nil {
			corrective = true
			results[call.ID] = "Error: invalid arguments for " + toolName + ": " + err.Error()
			if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], err.Error()); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		t, ok := r.reg.Get(toolName)
		if !ok {
			corrective = true
			message := "Error: unknown tool " + toolName + "; use canonical tools: " + activeToolNames(r.reg)
			results[call.ID] = message
			if err := r.persistPreparedFailure(sess, runID, call.ID, message, "unknown tool"); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		// Coerced after the registry lookup because it needs the tool's schema,
		// and before validation because coercion exists to prevent the failures
		// validation reports. Aliases ran earlier: they need only the name.
		if coerceToolArguments(t, args) {
			logger.Debug("coerced tool argument types for " + toolName)
		}
		if err := tool.ValidateArguments(t.Schema(), args); err != nil {
			corrective = true
			message := "Error: invalid arguments for " + toolName + ": " + err.Error() + "; use arguments " + toolCatalogArguments(t.Schema())
			results[call.ID] = message
			if err := r.persistPreparedFailure(sess, runID, call.ID, message, err.Error()); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		spec, err := t.Translate(args)
		if err != nil {
			corrective = true
			message := "Error: " + err.Error() + "; use arguments " + toolCatalogArguments(t.Schema())
			results[call.ID] = message
			if err := r.persistPreparedFailure(sess, runID, call.ID, message, err.Error()); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		if spec.Args == nil {
			spec.Args = map[string]any{}
		}
		spec.Args["goal_dir"] = r.cfg.GoalDir
		if r.cfg.WorkDir != "" {
			spec.Args["workdir"] = r.cfg.WorkDir
		}
		spec.Args["session_id"] = r.cfg.SessionID
		spec.Args["request_id"] = call.ID
		// The archive sidecar lives beside the session file, so the recall
		// tool resolves it from the store's own directory: the same rule the
		// compactor writes by, stated once here instead of once per tool.
		if r.store != nil {
			spec.Args["archive_dir"] = r.store.Dir()
		}
		if r.cfg.AdjustSpec != nil {
			r.cfg.AdjustSpec(&spec)
		}
		for i := range sess.Operations {
			if sess.Operations[i].ID == call.ID {
				sess.Operations[i].Tool = toolName
				sess.Operations[i].Args = args
				sess.Operations[i].SpecType = spec.Type
				sess.Operations[i].SpecArgs = spec.Args
				break
			}
		}
		prepared = append(prepared, preparedTool{call: call, spec: spec, tool: t, args: args, rawArgs: string(rawArgs)})
	}
	if err := r.store.Save(sess); err != nil {
		return nil, nil, false, err
	}

	dispatched := make([]preparedTool, 0, len(prepared))
	start := func(item preparedTool) error {
		if r.cb.OnToolStart != nil {
			r.cb.OnToolStart(item.call.ID, item.tool.Name(), map[string]any{"tool": item.tool.Name(), "args": item.rawArgs})
		}
		for i := range sess.Operations {
			if sess.Operations[i].ID == item.call.ID {
				sess.Operations[i].Status = sessionstore.StatusRunning
				break
			}
		}
		if err := r.store.Save(sess); err != nil {
			return err
		}
		op := operation.Operation{ID: item.call.ID, Spec: item.spec, Status: operation.StatusRunning}
		if sequential, ok := item.tool.(tool.SequentialTool); ok && sequential.Sequential() {
			r.opMgr.AddSequential(op, sequentialQueueFor(item.tool))
		} else {
			r.opMgr.Add(op)
		}
		dispatched = append(dispatched, item)
		return nil
	}
	type authDecision struct {
		allow bool
		err   error
	}
	decisions := make(map[string]authDecision, len(prepared))
	allowed := make(chan struct {
		id    string
		allow bool
		err   error
	}, len(prepared))
	authCount := 0
	for _, item := range prepared {
		needsAuth := r.cfg.AuthorizeTool != nil && (r.cfg.NeedsAuthorization == nil || r.cfg.NeedsAuthorization(item.tool.Name(), item.args))
		if !needsAuth {
			decisions[item.call.ID] = authDecision{allow: true}
			continue
		}
		authCount++
		go func(item preparedTool) {
			allow, err := r.cfg.AuthorizeTool(ctx, item.call.ID, item.tool.Name(), item.args)
			allowed <- struct {
				id    string
				allow bool
				err   error
			}{id: item.call.ID, allow: allow, err: err}
		}(item)
	}
	for i := 0; i < authCount; i++ {
		decision := <-allowed
		decisions[decision.id] = authDecision{allow: decision.allow, err: decision.err}
	}
	authorized := make([]preparedTool, 0, len(prepared))
	for _, item := range prepared {
		decision := decisions[item.call.ID]
		if decision.err != nil || !decision.allow {
			message := "Error: permission denied by user"
			if decision.err != nil {
				message = "Error: permission request failed: " + decision.err.Error()
			}
			results[item.call.ID] = message
			if err := r.persistPreparedFailure(sess, runID, item.call.ID, message, message); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		authorized = append(authorized, item)
	}
	// A sequential tool runs in its own lane, and the rest of the batch runs
	// beside it rather than behind it.
	//
	// One sequential tool used to make the whole batch sequential, on the
	// reasoning that a batch is a pipeline the model expects in order. That is
	// true of the calls the model sequenced against each other and false of the
	// rest: a batch of one edit and two reads is not a pipeline, and the reads
	// lost their parallelism to a file write they do not depend on. Lanes are
	// per resource now, so an edit and a browser action do not queue either.
	//
	// Order within a lane is still arrival order, which is what keeps a browser
	// pipeline intact: the calls that must be sequential go to the same lane.
	for _, item := range authorized {
		if err := start(item); err != nil {
			return nil, nil, false, err
		}
	}
	return dispatched, results, corrective, nil
}

// sequentialQueueFor is the lane a sequential tool runs in, or the default lane
// when it names none. The default is the conservative choice: a tool that does
// not say what it contends with is assumed to contend with everything.
func sequentialQueueFor(t tool.Tool) string {
	if named, ok := t.(tool.SequentialQueueTool); ok {
		if lane := strings.TrimSpace(named.SequentialQueue()); lane != "" {
			return lane
		}
	}
	return operation.DefaultSequentialQueue
}

// fileMutatingSpecs are the operations whose whole purpose is to change a file.
// The index is refreshed for these and nothing else.
var fileMutatingSpecs = map[string]bool{
	"write":           true,
	"edit_line":       true,
	"edit":            true,
	"batch_edit_file": true,
}

// noteFileMutation reports a completed file write so a search index can follow it.
//
// Only on success. A failed write leaves the old contents on disk, and indexing
// them would put a file into the index that the agent believes it has changed.
func (r *Runner) noteFileMutation(sess *sessionstore.Session, result operation.ResultEvent) {
	if r.cfg.OnFileMutated == nil {
		return
	}
	if result.Status != operation.StatusCompleted {
		return
	}
	for _, op := range sess.Operations {
		if op.ID != result.OpID || !fileMutatingSpecs[op.SpecType] {
			continue
		}
		path, _ := op.SpecArgs["path"].(string)
		if strings.TrimSpace(path) == "" {
			return
		}
		r.cfg.OnFileMutated(path)
		return
	}
}

func (r *Runner) notifyToolDone(sess *sessionstore.Session, result operation.ResultEvent) {
	if r.cb.OnToolDone == nil {
		return
	}
	name := ""
	for _, op := range sess.Operations {
		if op.ID == result.OpID {
			name = op.Tool
			break
		}
	}
	r.cb.OnToolDone(result.OpID, name, result)
}

func (r *Runner) persistPreparedFailure(sess *sessionstore.Session, runID, id, output, errorText string) error {
	for i := range sess.Operations {
		if sess.Operations[i].ID == id {
			sess.Operations[i].Status = sessionstore.StatusFailed
			sess.Operations[i].Output = output
			sess.Operations[i].Error = errorText
			sess.Operations[i].ResultPresent = true
			if err := r.store.Save(sess); err != nil {
				return err
			}
			r.notifyToolDone(sess, operation.ResultEvent{OpID: id, Status: operation.StatusFailed, Output: output, Error: errorText})
			return nil
		}
	}
	return fmt.Errorf("operation %s not found", id)
}

func (r *Runner) persistResult(sess *sessionstore.Session, runID string, result operation.ResultEvent) error {
	for i := range sess.Operations {
		if sess.Operations[i].ID != result.OpID {
			continue
		}
		sess.Operations[i].Output = r.resultText(runID, result)
		sess.Operations[i].Error = result.Error
		sess.Operations[i].ResultPresent = true
		if len(result.Images) > 0 {
			sess.Operations[i].Images = append([]string(nil), result.Images...)
		}
		switch result.Status {
		case operation.StatusCompleted:
			sess.Operations[i].Status = sessionstore.StatusCompleted
		case operation.StatusCanceled:
			sess.Operations[i].Status = sessionstore.StatusCanceled
		default:
			sess.Operations[i].Status = sessionstore.StatusFailed
		}
		return r.store.Save(sess)
	}
	return fmt.Errorf("operation %s not found", result.OpID)
}

func (r *Runner) persistUnknown(sess *sessionstore.Session, runID, id string, cause error) error {
	for i := range sess.Operations {
		if sess.Operations[i].ID != id {
			continue
		}
		sess.Operations[i].Status = sessionstore.StatusUnknown
		sess.Operations[i].Error = cause.Error()
		sess.Operations[i].Output = "Error: operation outcome is unknown after interruption"
		sess.Operations[i].ResultPresent = true
		if err := r.store.Save(sess); err != nil {
			return err
		}
		status := operation.StatusFailed
		if errors.Is(cause, context.Canceled) {
			status = operation.StatusCanceled
		}
		r.notifyToolDone(sess, operation.ResultEvent{OpID: id, Status: status, Output: sess.Operations[i].Output, Error: cause.Error()})
		return nil
	}
	return fmt.Errorf("operation %s not found", id)
}

func (r *Runner) appendResults(sess *sessionstore.Session, calls []llm.StreamedToolCall, results map[string]string) {
	for _, call := range calls {
		result, recorded := results[call.ID]
		if !recorded {
			// Every function call must be answered by exactly one tool message, so this
			// cannot be skipped. It used to be, on the reasoning that a skipped call is
			// an absent one, which is exactly wrong: the assistant message above still
			// declares it.
			//
			// An entry prepareTools made itself is a decline and is already recorded as
			// an empty string, so reaching here means the call was dispatched and never
			// reported. The outcome is genuinely unknown and the text says so.
			result = recoveryResult(call.Name, callStarted(sess, call.ID))
		}
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:              llm.RoleTool,
			ToolCallID:        call.ID,
			ToolName:          call.Name,
			ToolResult:        result,
			ToolResultPresent: true,
			ResultStatus:      r.statusFor(sess, call.ID),
			ToolImages:        r.imagesFor(sess, call.ID),
		})
	}
}

// imagesFor returns the image paths an operation attached, for a read that landed
// on a picture. Read alongside statusFor rather than threaded through the results
// map, which carries text only.
func (r *Runner) imagesFor(sess *sessionstore.Session, id string) []string {
	for _, op := range sess.Operations {
		if op.ID == id {
			return op.Images
		}
	}
	return nil
}

func (r *Runner) statusFor(sess *sessionstore.Session, id string) sessionstore.OperationStatus {
	for _, op := range sess.Operations {
		if op.ID == id {
			return op.Status
		}
	}
	return sessionstore.StatusUnknown
}

func (r *Runner) resultText(runID string, result operation.ResultEvent) string {
	output := result.Output
	if result.OutputPath != "" {
		if output != "" {
			output += "\n"
		}
		output += "Full output: " + result.OutputPath
	}
	formatted := r.formatOutput(runID, result.OpID, output)
	if result.Error != "" {
		if formatted != "" {
			return formatted + "\nError: " + result.Error
		}
		return "Error: " + result.Error
	}
	return formatted
}

// sidekickNote is the injected block for a completed operation, or "".
func (r *Runner) sidekickNote(sess *sessionstore.Session, result operation.ResultEvent) string {
	if r.sidekick == nil {
		return ""
	}
	return r.sidekick.annotate(sess, result)
}

// instructionNote renders the AGENTS.md rules that bear on the operation that
// just finished, or "" when nothing applies.
//
// Marking comes after the render, and it makes no difference today: Select applies
// the note ceiling itself, so Selection.Rules is already exactly the set that
// Annotate renders and there is nothing to mark that was not sent. The order is
// kept because it is the one that stays correct if that changes - if Select ever
// hands back every matched rule and leaves the ceiling to Annotate, marking first
// would retire rules the ceiling then dropped, and they would be delivered
// nowhere in the session. It costs one statement to keep that from being a bug.
//
// TestARuleTheBudgetHeldIsStillDeliveredOnTheNextMatchingOperation is what holds
// the invariant, from the outside rather than from the call order.
func (r *Runner) instructionNote(sess *sessionstore.Session, result operation.ResultEvent) string {
	if r.instructions == nil {
		return ""
	}
	ev := r.instructionTrigger(sess, result)
	selection := r.instructions.Select(ev)
	note := instruction.Annotate(ev, selection)
	if note == "" {
		return ""
	}
	ids := make([]instruction.ID, 0, len(selection.Rules))
	for _, rule := range selection.Rules {
		ids = append(ids, rule.ID)
	}
	r.instructions.Seen().Mark(ids...)
	return note
}

// instructionTrigger reads what the completed operation was about.
//
// It shares target extraction with the memory sidekick rather than deriving paths
// a second time: the shell walk skips the program token, drops flags and globs,
// and refuses anything outside the workdir, and a second copy of that would drift
// from the first within a release.
func (r *Runner) instructionTrigger(sess *sessionstore.Session, result operation.ResultEvent) instruction.Trigger {
	var op sessionstore.Operation
	found := false
	for _, candidate := range sess.Operations {
		if candidate.ID == result.OpID {
			op = candidate
			found = true
			break
		}
	}
	if !found {
		return instruction.Trigger{Failed: result.Status != operation.StatusCompleted}
	}
	return InstructionTrigger(op.SpecType, op.SpecArgs, op.Tool, r.cfg.WorkDir, result.Status != operation.StatusCompleted)
}

// ForgetDeliveredMemory releases every memory this session has been annotated
// with, so each one is eligible again on the next matching operation.
//
// This is the seam for a compaction epoch, and it belongs here rather than in the
// compactor because the compactor owns the transcript, not the knowledge of which
// notes are still in it: a compaction drops the tool results that carried them, so
// the memory behind a note is no longer in context and delivering it once more is
// not a repeat. Call it with the session being compacted, on both sides of the
// fold, and the delivered list on the session file goes with it.
func (r *Runner) ForgetDeliveredMemory(sess *sessionstore.Session) {
	if r.sidekick == nil {
		return
	}
	r.sidekick.mu.Lock()
	defer r.sidekick.mu.Unlock()
	r.sidekick.forgetDelivered(sess)
}

// MarkDeliveredMemory records memories as already delivered without injecting
// them, for a caller carrying a set across an epoch boundary or a restart.
//
// The ids are the same ones Annotate was given: memory.DeliveryID of the entry,
// which is its store id. Nothing validates them against the store, because a
// caller restoring state it wrote earlier has nothing to check it against and a
// remembered delivery that no longer exists costs nothing - the memory simply
// stays quiet.
func (r *Runner) MarkDeliveredMemory(sess *sessionstore.Session, ids ...string) {
	if r.sidekick == nil || sess == nil {
		return
	}
	r.sidekick.mu.Lock()
	defer r.sidekick.mu.Unlock()
	r.sidekick.remember(ids...)
	r.sidekick.persist(sess, ids...)
}

// DeliveryReport says how memory was delivered for the run that just finished.
func (r *Runner) DeliveryReport() memory.DeliveryReport {
	report := r.sidekick.deliveryReport()
	report.Mode = string(r.delivery)
	report.PromptBlock = r.delivery == memory.DeliveryPrompt && r.memoryContext() != ""
	return report
}

func (r *Runner) formatOutput(runID, operationID, output string) string {
	// Repeated content is collapsed before the cap is consulted, because the
	// repeats that actually cost anything are not the trimmed ones.
	//
	// A 21k file trims to a 4k notice, and that notice then sits comfortably under
	// the cap, so every later read of the same file passes straight through and
	// re-sends it. Measured on a real session: one file read five times, its
	// identical 4,145-byte notice sent seven times, 25k of context for content
	// the model already had. Gating this on the trim branch missed all of it.
	if r.alreadySent(output) {
		notice := r.repeatNotice(runID, operationID, output)
		recordToolOutput(len(output), len(notice))
		return notice
	}
	limit := r.cfg.MaxToolResultChars
	if limit <= 0 || len(output) <= limit {
		// Counted here rather than after the trim, so the denominator is every
		// result that reached the model, not only the ones that were cut. A trim
		// rate measured only over trimmed results is 100% by construction.
		recordToolOutput(len(output), len(output))
		return output
	}
	dir := r.cfg.ToolOutputDir
	if dir == "" {
		dir = defaultToolOutputDir(r.cfg.GoalDir)
	}
	path := filepath.Join(dir, runID, r.safeResultName(operationID)+".txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if err := os.WriteFile(path, []byte(output), 0o600); err == nil {
			trimmed := r.boundedView(output, limit, offloadNotice(path, len(output)))
			recordToolOutput(len(output), len(trimmed))
			return trimmed
		}
	}
	trimmed := r.boundedView(output, limit, fmt.Sprintf("Large tool output (%d bytes); storage failed.", len(output)))
	recordToolOutput(len(output), len(trimmed))
	return trimmed
}

// boundedView builds what the model actually sees of an oversized result: a
// shape line, a head, and the tail.
//
// The tail was deliberately left alone by two earlier attempts at adding a head,
// and both are worth recording because this one is not a third try at either.
// Halving the budget between the two ends covers the same fraction of the file
// as the tail alone and weakens the end, which is the answer for log output.
// Filling the whole cap reaches further but sent 2.9x the content and cost more
// than collapsing repeated reads saves. Both took bytes from the tail.
//
// This one does not. At the shipped 12,000 cap the tail is 4,000 and the notice
// is ~210, so 7,784 bytes were simply unused. The head is funded from that, the
// tail keeps every byte it had, and the total still lands under the cap.
//
// What it is for is the case the tail cannot serve at all: a JSON array arrives
// starting mid-object, so the model never sees the bracket, the first key, or
// the schema. A line-1 error in a 4,000-line build log is likewise absent. Both
// are answered by the first few hundred bytes.
func (r *Runner) boundedView(output string, limit int, notice string) string {
	head := headWindow(output, limit)
	var b strings.Builder
	b.WriteString(notice)
	b.WriteString("\n")
	b.WriteString(shapeLine(output))
	if head != "" {
		b.WriteString("\nHead:\n")
		b.WriteString(head)
	}
	b.WriteString("\nTail:\n")
	b.WriteString(tailWindow(output, limit))
	return b.String()
}

// shapeLine describes a blob without quoting it.
//
// Size and line count alone leave the model unable to tell a JSON array from a
// log, and it needs that to decide whether to grep the offloaded path or read
// straight through. Keys are schema rather than content, so they are reported and
// values are not: the line is about the shape of what was cut, not a preview of
// what is in it. Bounded, because a shape that does not fit is not a summary.
func shapeLine(output string) string {
	var b strings.Builder
	b.WriteString("Shape: ")
	fmt.Fprintf(&b, "%s, %d lines", humanChars(len(output)), countLines(output))
	if keys := topLevelKeys(output); len(keys) > 0 {
		kind := "object"
		if strings.HasPrefix(strings.TrimSpace(output), "[") {
			kind = "array"
		}
		fmt.Fprintf(&b, ", JSON %s, keys: %s", kind, strings.Join(keys, ", "))
	}
	return b.String()
}

// topLevelKeys returns the first few field names of a JSON document, or nil when
// it is not JSON.
//
// Scanned rather than unmarshalled: this runs on the hot path for every trimmed
// result, and the point is to describe a blob that may be too large or malformed
// to parse at all.
//
// The scan starts at the first object brace rather than at the first brace of any
// kind, because the common shape is an array of objects - a lockfile, an API
// response, a search result - where the keys that describe the schema are one
// level deeper than the array. Scanning from depth 1 of the document itself found
// nothing there and reported a JSON array as having no keys, which is the case
// this line exists to describe.
func topLevelKeys(output string) []string {
	trimmed := strings.TrimLeft(output, " \t\r\n")
	if trimmed == "" {
		return nil
	}
	if c := trimmed[0]; c != '{' && c != '[' {
		return nil
	}
	start := strings.IndexByte(trimmed, '{')
	if start < 0 || start > 512 {
		return nil
	}
	const want = 8
	keys := make([]string, 0, want)
	depth := 0
	inString := false
	escaped := false
	expectKey := false
	var current strings.Builder
	for i := start; i < len(trimmed) && len(keys) < want; i++ {
		if i-start > 4096 {
			break
		}
		c := trimmed[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
			if !inString {
				// Cleared on every closing quote, not only on one that turned out to
				// be a key. Leaving a value behind made the next key concatenate onto
				// it, so a document with "name":"c-0000" then "path" reported a field
				// called c-0000path.
				key := current.String()
				current.Reset()
				if expectKey {
					expectKey = false
					// Only a plausible field name is reported. Anything else is the
					// value half of a pair, and calling it a key would be a lie.
					if isFieldName(key) {
						keys = append(keys, key)
					}
				}
			}
		case inString:
			current.WriteByte(c)
		case c == '{':
			depth++
			expectKey = depth == 1
		case c == ',':
			if depth == 1 {
				expectKey = true
			}
		case c == '}':
			depth--
			if depth <= 0 {
				return keys
			}
		}
	}
	return keys
}

func isFieldName(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	return n
}

// headWindow returns the opening bytes of a blob, funded from the cap the tail
// was not already using.
//
// Sized as an eighth of the cap and not a share of the tail. Two earlier layouts
// took bytes from the tail and were reverted with measurements: halving between
// the ends weakens the end, which is the answer for log output, and filling the
// cap sent 2.9x the content and cost more than collapsing repeated reads saves.
// Reading a number that was already spare keeps the tail whole.
func headWindow(output string, limit int) string {
	used := len(offloadNotice("", len(output)))
	budget := limit - (limit / 3) - used
	n := budget / 6
	if n < 256 {
		n = 256
	}
	if n > 1600 {
		n = 1600
	}
	if len(output) <= n*2 {
		// Short enough that head and tail would overlap and read as the same
		// lines twice.
		return ""
	}
	cut := output[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i+1]
	}
	return cut
}

// offloadNotice tells the model the content exists, where it is, and that the
// location is usable.
//
// The last part is the one that was missing. A real session was handed this path
// five times for the same unchanged file and never once ran grep against it,
// re-reading the identical tail each time instead, while the answer sat in the
// part of the file it could not see. A path is only useful if the reader knows
// it can open it.
func offloadNotice(path string, size int) string {
	return fmt.Sprintf(
		"Large tool output saved to %s (%d bytes). It is greppable from bash, and you can read any part of it from that path; re-reading the source will only return this notice again.",
		path, size)
}

func tailWindow(output string, limit int) string {
	n := limit / 3
	if n < 1024 {
		n = 1024
	}
	if n > len(output) {
		n = len(output)
	}
	return runeSafe(output[len(output)-n:])
}

// runeSafe backs a cut back to a rune boundary.
//
// Cutting mid-sequence produces invalid UTF-8, which a strict decoder rejects
// and a lenient one turns into replacement characters. Slicing on bytes is the
// cheap way to trim and the easy way to get this wrong, and it now happens twice
// per result instead of once.
func runeSafe(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

// repeatNotice points at the content instead of resending it.
//
// The file is written on a repeat too, not only on the first send, so the path
// quoted here always resolves. It matters because the earlier copy can be gone
// from the model's context by compaction, and a reference to a file that is not
// there is worse than the repetition it saves.
func (r *Runner) repeatNotice(runID, operationID, output string) string {
	dir := r.cfg.ToolOutputDir
	if dir == "" {
		dir = defaultToolOutputDir(r.cfg.GoalDir)
	}
	path := filepath.Join(dir, runID, r.safeResultName(operationID)+".txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Sprintf("Identical to a tool result earlier in this run (%d bytes); not repeated.", len(output))
	}
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		return fmt.Sprintf("Identical to a tool result earlier in this run (%d bytes); not repeated.", len(output))
	}
	return fmt.Sprintf(
		"Identical to a tool result earlier in this run (%d bytes); not repeated. Full content: %s",
		len(output), path)
}

// safeResultName keeps an operation ID from escaping the output directory.
func (r *Runner) safeResultName(operationID string) string {
	safe := filepath.Base(operationID)
	if safe == "." || safe == string(filepath.Separator) || safe == "" {
		return "result"
	}
	return safe
}

// alreadySent reports whether this exact content has already gone to the model in
// this run, recording it the first time.
//
// Keyed on content, not operation, because the repeat is the same file read twice
// and the two calls carry different IDs.
//
// The floor keeps short results out of it. A model re-reading a 200-byte file is
// usually checking something specific, and collapsing that would trade real
// information for a few hundred bytes. A thousand bytes is where a repeat starts
// costing more than the certainty is worth.
//
// Bounded because a long run keeps adding to this and it holds strings.
func (r *Runner) alreadySent(output string) bool {
	const minRepeatWorthCollapsing = 1000
	if len(output) < minRepeatWorthCollapsing {
		return false
	}
	sum := sha256.Sum256([]byte(output))
	key := string(sum[:])
	r.trimmedMu.Lock()
	defer r.trimmedMu.Unlock()
	if r.trimmedSeen == nil {
		r.trimmedSeen = make(map[string]struct{})
	}
	if _, seen := r.trimmedSeen[key]; seen {
		return true
	}
	const maxRemembered = 64
	if len(r.trimmedOrder) >= maxRemembered {
		delete(r.trimmedSeen, r.trimmedOrder[0])
		r.trimmedOrder = r.trimmedOrder[1:]
	}
	r.trimmedSeen[key] = struct{}{}
	r.trimmedOrder = append(r.trimmedOrder, key)
	return false
}

// budgetPoints resolves the three thresholds of the graduated request budget.
//
// The yield point defaults to 1.5x the soft ceiling and the grace to 5 requests,
// matching omp's subagent budget. A child told to wrap up needs a turn or two to
// do it, and cutting it off at the yield point would discard exactly the report the
// graduation exists to preserve.
func (r *Runner) budgetPoints() (soft, yieldAt, hardAt int) {
	soft = r.cfg.MaxRequests
	if soft <= 0 {
		return 0, 0, 0
	}
	yieldAt = r.cfg.YieldAt
	if yieldAt <= soft {
		yieldAt = soft + soft/2
	}
	grace := r.cfg.HardGrace
	if grace <= 0 {
		grace = 5
	}
	return soft, yieldAt, yieldAt + grace
}

// logCachePrefix fingerprints the request and reports the first element where it
// differs from the previous one.
//
// A hit rate says how much was reused. It cannot say where the reuse stopped, and
// those need different fixes: a break at element 0 is something rewriting the
// system prompt, a break at the last element is the provider refusing to extend
// its own cache. Guessing between those two produced one confident wrong answer
// already, so the instrument reports the position instead.
var lastCachePrefix string

func logCachePrefix(step int, items []llm.Item, tools []llm.ToolDef, bd llm.TokenBreakdown) {
	// The comparable prefix is everything up to and including the last message
	// that is identical to the previous request. Tool definitions are hashed
	// separately because they change on their own schedule.
	var sb strings.Builder
	for _, t := range tools {
		sb.WriteString(t.Name)
		sb.WriteString(t.Description)
	}
	toolSum := sha256.Sum256([]byte(sb.String()))
	toolHash := fmt.Sprintf("%x", toolSum[:4])
	roles := make([]string, 0, len(items))
	for _, it := range items {
		roles = append(roles, fmt.Sprintf("%s/%d", it.Kind, len(it.Text)+len(it.Output)+len(it.Args)))
	}
	cur := strings.Join(roles, ",")
	diverged := "n/a"
	if lastCachePrefix != "" {
		diverged = "?"
		a, b := strings.Split(lastCachePrefix, ","), strings.Split(cur, ",")
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		for i := 0; i < n; i++ {
			if a[i] != b[i] {
				diverged = fmt.Sprintf("item%d", i)
				break
			}
		}
		if diverged == "?" {
			diverged = fmt.Sprintf("none(first %d identical)", n)
		}
	}
	lastCachePrefix = cur
	logger.Info(fmt.Sprintf("pfx step=%d items=%d tools=%d toolhash=%s sys=%d conv=%d toolout=%d divergence=%s",
		step, len(items), len(tools), toolHash, bd.System, bd.Conversation, bd.ToolOutput, diverged))
}

// splitLegacyBrowserCall rewrites a recorded combined-browser call into the
// per-action tool that replaced it.
//
// The split exists because the model kept bundling arguments belonging to
// different actions into one call, and the strongest thing that can be done
// about that is to stop offering a schema where the bundling is expressible.
// Sessions recorded before the split still contain `browser` calls, and those
// stay in the history of a long conversation. Left alone they teach the model
// the shape that caused the problem, so the history is rewritten to the new
// names on the way out.
//
// Only the name and the action change. The arguments are already what the
// per-action tool would have been given, minus the action itself, which is the
// tool's whole identity now.
func splitLegacyBrowserCall(item llm.Item) llm.Item {
	if item.Name != "browser" {
		return item
	}
	action, args := takeBrowserAction(item.Args)
	if !splitBrowserAction(action) {
		return item
	}
	item.Name = "browser_" + action
	item.Args = args
	return item
}

// splitBrowserAction reports whether an action has its own tool.
//
// It is a list rather than a blanket rename because the split is not total: the
// text and html readers were removed, and a recorded call that used one must
// keep the combined name. The hidden browser tool is still executable precisely
// so those calls still run, and renaming them to a tool that does not exist
// would turn a working history into a dead one.
var splitBrowserActions = map[string]bool{
	"open": true, "open_file": true, "click": true, "type": true,
	"screenshot": true, "eval": true, "status": true, "close": true,
}

func splitBrowserAction(action string) bool { return splitBrowserActions[action] }

func splitLegacyBrowserCallRef(call llm.ToolCall) llm.ToolCall {
	if call.Name != "browser" {
		return call
	}
	action, args := takeBrowserAction(call.Args)
	if !splitBrowserAction(action) {
		return call
	}
	call.Name = "browser_" + action
	call.Args = args
	return call
}

// takeBrowserAction lifts the action out of a combined browser argument object
// and returns the rest. An unparseable object, or one without an action, comes
// back untouched so the call is left exactly as recorded rather than
// half-rewritten.
func takeBrowserAction(raw string) (string, string) {
	if strings.TrimSpace(raw) == "" {
		return "", raw
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", raw
	}
	action, ok := args["action"].(string)
	if !ok {
		return "", raw
	}
	switch action {
	case "open":
		url, hasURL := args["url"].(string)
		path, hasPath := args["path"].(string)
		if hasURL == hasPath || (hasURL && strings.TrimSpace(url) == "") || (hasPath && strings.TrimSpace(path) == "") {
			return "", raw
		}
		target := "url"
		if hasPath {
			target = "path"
			action = "open_file"
		}
		for key := range args {
			if key != "action" && key != target && key != "wait_ms" {
				return "", raw
			}
		}
	case "click", "close", "eval", "html", "screenshot", "status", "text", "type":
	default:
		return "", raw
	}
	delete(args, "action")
	rest, err := json.Marshal(args)
	if err != nil {
		return "", raw
	}
	return action, string(rest)
}

package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sooraa/harness/compaction"
	"sooraa/harness/contextbuilder"
	"sooraa/harness/inbox"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/tool"
)

type RunnerConfig struct {
	SessionID           string
	SystemPrompt        string
	MaxSteps            int
	Reasoning           llm.ReasonLevel
	StopWhenIdle        bool
	Model               llm.ProviderConfig
	ContextLimit        int
	MaxOutputTokens     int
	WorkDir             string
	ToolOutputDir       string
	MaxToolResultChars  int
	ModelCatalogURL     string
	ModelCatalogCache   string
	ModelCatalogRefresh time.Duration
	Inbox               *inbox.Inbox
	OperationPool       *operation.LocalManager
	Compactor           *compaction.Compactor
	ForcedCompaction    func() bool
	AuthorizeTool       func(context.Context, string, string, map[string]any) (bool, error)
	NeedsAuthorization  func(string, map[string]any) bool
	AdjustSpec          func(*operation.Spec)
	Callbacks           Callbacks
}

type Config = RunnerConfig

type Callbacks struct {
	OnContent         func(string)
	OnReasoning       func(string)
	OnToolStart       func(tool.Tool, map[string]any)
	OnToolDone        func(operation.ResultEvent)
	OnCompactionStart func()
	OnCompactionDone  func(bool, error)
	OnUsage           func(llm.UsageInfo)
	OnCost            func(llm.CostInfo, string)
	OnDone            func(string)
	OnError           func(error)
}

type Result struct {
	Final         string
	Usage         llm.UsageInfo
	Cost          llm.CostInfo
	PricingStatus string
}

type Runner struct {
	cfg     RunnerConfig
	store   *sessionstore.Store
	reg     *tool.Registry
	opMgr   *operation.LocalManager
	catalog *llm.CatalogClient
	cb      Callbacks
}

func New(cfg RunnerConfig, store *sessionstore.Store, reg *tool.Registry) *Runner {
	if cfg.Model.BaseURL == "" {
		cfg.Model = llm.DefaultConfig()
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 10
	}
	if cfg.ContextLimit <= 0 {
		cfg.ContextLimit = 236000
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 8192
	}
	if cfg.MaxToolResultChars <= 0 {
		cfg.MaxToolResultChars = 12000
	}
	if cfg.ToolOutputDir == "" {
		cfg.ToolOutputDir = filepath.Join(os.TempDir(), "sooraa-tool-outputs")
	}
	opMgr := cfg.OperationPool
	if opMgr == nil {
		opMgr = operation.NewWithBuffer(&toolRunner{reg: reg}, 4096)
	}
	catalog := llm.NewCatalogClient(cfg.ModelCatalogURL, cfg.ModelCatalogCache, cfg.ModelCatalogRefresh)
	return &Runner{cfg: cfg, store: store, reg: reg, opMgr: opMgr, catalog: catalog, cb: cfg.Callbacks}
}

type toolRunner struct {
	reg *tool.Registry
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
		sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleSystem, Content: r.cfg.SystemPrompt})
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
	var runCost llm.CostInfo
	if r.catalog == nil {
		r.catalog = llm.NewCatalogClient(r.cfg.ModelCatalogURL, r.cfg.ModelCatalogCache, r.cfg.ModelCatalogRefresh)
	}
	pricingMetadata, pricingErr := r.catalog.Lookup(ctx, r.cfg.Model.Model)
	pricingStatus := pricingMetadata.PricingStatus(pricingErr)
	pricingUsable := pricingStatus == "current"
	for i := range sess.Runs {
		if sess.Runs[i].ID == runID {
			sess.Runs[i].PricingModel = r.cfg.Model.Model
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
			runUsage.Add(delta)
			if pricingUsable {
				if cost, ok := pricingMetadata.Cost(delta); ok {
					runCost.Add(cost)
				}
			}
			if r.cb.OnUsage != nil {
				r.cb.OnUsage(runUsage)
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
				sess.Runs[i].PricingStatus = pricingStatus
				sess.Runs[i].PricingFetchedAt = pricingMetadata.PricingFetchedAt
				if runErr != nil {
					sess.Runs[i].Error = runErr.Error()
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

	for step := 0; step < r.cfg.MaxSteps; step++ {
		for i := range sess.Runs {
			if sess.Runs[i].ID == runID {
				sess.Runs[i].Step = step
				sess.Runs[i].Requests++
				sess.Runs[i].Usage = runUsage
				sess.Runs[i].Cost = runCost
				sess.Runs[i].InputBreakdown = runBreakdown
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
						}
					}
					if err := r.store.Save(sess); err != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
					}
				}
			}
		}
		if compaction.Prune(sess) > 0 {
			if err := r.store.Save(sess); err != nil {
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
			}
		}

		client := llm.New(r.cfg.Model)
		toolDefs := r.toolDefs()
		request := llm.StreamRequest{
			Messages:  r.messages(sess, toolDefs),
			Tools:     toolDefs,
			Reason:    r.cfg.Reasoning,
			MaxTokens: r.cfg.MaxOutputTokens,
		}
		breakdown := llm.EstimateTokenBreakdown(request.Messages, request.Tools)
		var content strings.Builder
		var reasoning strings.Builder
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
				if r.cb.OnReasoning != nil {
					r.cb.OnReasoning(reasoning.String())
				}
			}
			if len(delta.ToolCalls) > 0 {
				calls = llm.MergeStreamedToolCalls(calls, delta.ToolCalls)
			}
			if delta.Usage != nil {
				usage.Add(*delta.Usage)
				runUsage.Add(*delta.Usage)
				if r.cb.OnUsage != nil {
					r.cb.OnUsage(runUsage)
				}
			}
		})
		if pricingUsable {
			if cost, ok := pricingMetadata.Cost(usage); ok {
				runCost.Add(cost)
			}
		}
		if r.cb.OnCost != nil {
			r.cb.OnCost(runCost, pricingStatus)
		}
		runBreakdown.Add(breakdown)
		for i := range sess.Runs {
			if sess.Runs[i].ID == runID {
				sess.Runs[i].Usage = runUsage
				sess.Runs[i].Cost = runCost
				sess.Runs[i].InputBreakdown = runBreakdown
			}
		}
		if saveErr := r.store.Save(sess); saveErr != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
		}
		if err != nil {
			if r.cfg.Compactor != nil && (r.cfg.Compactor.IsOverflow(sess) || strings.Contains(strings.ToLower(err.Error()), "context") || strings.Contains(strings.ToLower(err.Error()), "token")) {
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
						}
					}
					if saveErr := r.store.Save(sess); saveErr != nil {
						return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, saveErr)
					}
					step--
					continue
				}
			}
			status := sessionstore.RunFailed
			if ctx.Err() != nil {
				status = sessionstore.RunCanceled
			}
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(status, err)
		}

		calls = llm.EnsureToolCallIDs(calls, fmt.Sprintf("%s_%d", runID, step))
		calls = r.uniqueToolCallIDs(calls, sess)
		if len(calls) == 0 {
			answer := content.String()
			sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleAssistant, Content: answer, Reasoning: reasoning.String()})
			if err := r.store.Save(sess); err != nil {
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
			}
			if r.cb.OnDone != nil {
				r.cb.OnDone(answer)
			}
			return Result{Final: answer, Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunCompleted, nil)
		}

		assistant := sessionstore.Message{Role: llm.RoleAssistant, Content: content.String(), Reasoning: reasoning.String(), ToolCalls: make([]sessionstore.ToolCallRef, 0, len(calls))}
		for _, call := range calls {
			assistant.ToolCalls = append(assistant.ToolCalls, sessionstore.ToolCallRef{ID: call.ID, Name: call.Name, Args: call.Arguments})
		}
		sess.Messages = append(sess.Messages, assistant)
		if err := r.store.Save(sess); err != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}

		prepared, results, err := r.prepareTools(ctx, sess, runID, calls)
		if err != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}
		pending := make(map[string]struct{}, len(prepared))
		for _, item := range prepared {
			pending[item.call.ID] = struct{}{}
		}
		timeout := time.NewTimer(180 * time.Second)
		for len(pending) > 0 {
			select {
			case result := <-r.opMgr.Results():
				if _, ok := pending[result.OpID]; !ok {
					continue
				}
				delete(pending, result.OpID)
				results[result.OpID] = r.resultText(runID, result)
				if err := r.persistResult(sess, runID, result); err != nil {
					return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
				}
				if r.cb.OnToolDone != nil {
					r.cb.OnToolDone(result)
				}
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
	}
	err = fmt.Errorf("maximum steps reached without a final answer")
	return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
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
	defs := make([]llm.ToolDef, 0, len(r.reg.All()))
	for _, t := range r.reg.All() {
		defs = append(defs, llm.ToolDef{Name: t.Name(), Description: t.Description(), Parameters: t.Schema()})
	}
	return defs
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
	limit := r.cfg.ContextLimit - r.cfg.MaxOutputTokens - llm.EstimateToolDefinitionTokens(tools)
	if limit <= 0 {
		limit = r.cfg.ContextLimit - r.cfg.MaxOutputTokens
	}
	if limit <= 0 {
		limit = 216000
	}
	return contextbuilder.New(&history, r.cfg.SystemPrompt).BuildWithLimit(limit).Messages
}

type preparedTool struct {
	call llm.StreamedToolCall
	spec operation.Spec
	tool tool.Tool
}

func (r *Runner) prepareTools(ctx context.Context, sess *sessionstore.Session, runID string, calls []llm.StreamedToolCall) ([]preparedTool, map[string]string, error) {
	prepared := make([]preparedTool, 0, len(calls))
	results := make(map[string]string, len(calls))
	for _, call := range calls {
		op := sessionstore.Operation{ID: call.ID, Tool: call.Name, Status: sessionstore.StatusPending, RunID: runID, ToolCallID: call.ID}
		sess.Operations = append(sess.Operations, op)
		results[call.ID] = ""
		var args map[string]any
		if !json.Valid([]byte(call.Arguments)) {
			results[call.ID] = "Error: invalid JSON arguments for " + call.Name
			if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], "invalid JSON arguments"); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			results[call.ID] = "Error: invalid arguments for " + call.Name + ": " + err.Error()
			if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], err.Error()); err != nil {
				return nil, nil, err
			}
			continue
		}
		t, ok := r.reg.Get(call.Name)
		if !ok {
			results[call.ID] = "Error: unknown tool " + call.Name
			if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], "unknown tool"); err != nil {
				return nil, nil, err
			}
			continue
		}
		spec, err := t.Translate(args)
		if err != nil {
			results[call.ID] = "Error: " + err.Error()
			if err := r.persistPreparedFailure(sess, runID, call.ID, results[call.ID], err.Error()); err != nil {
				return nil, nil, err
			}
			continue
		}
		if spec.Args == nil {
			spec.Args = map[string]any{}
		}
		if r.cfg.WorkDir != "" {
			spec.Args["workdir"] = r.cfg.WorkDir
		}
		spec.Args["session_id"] = r.cfg.SessionID
		spec.Args["request_id"] = call.ID
		if r.cfg.AdjustSpec != nil {
			r.cfg.AdjustSpec(&spec)
		}
		for i := range sess.Operations {
			if sess.Operations[i].ID == call.ID {
				sess.Operations[i].Args = args
				sess.Operations[i].SpecType = spec.Type
				sess.Operations[i].SpecArgs = spec.Args
				break
			}
		}
		prepared = append(prepared, preparedTool{call: call, spec: spec, tool: t})
	}
	if err := r.store.Save(sess); err != nil {
		return nil, nil, err
	}

	dispatched := make([]preparedTool, 0, len(prepared))
	start := func(item preparedTool) error {
		if r.cb.OnToolStart != nil {
			r.cb.OnToolStart(item.tool, map[string]any{"tool": item.call.Name, "args": item.call.Arguments})
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
		r.opMgr.Add(operation.Operation{ID: item.call.ID, Spec: item.spec, Status: operation.StatusRunning})
		dispatched = append(dispatched, item)
		return nil
	}
	authCount := 0
	allowed := make(chan struct {
		item  preparedTool
		allow bool
		err   error
	}, len(prepared))
	for _, item := range prepared {
		var args map[string]any
		_ = json.Unmarshal([]byte(item.call.Arguments), &args)
		needsAuth := r.cfg.AuthorizeTool != nil && (r.cfg.NeedsAuthorization == nil || r.cfg.NeedsAuthorization(item.call.Name, args))
		if !needsAuth {
			if err := start(item); err != nil {
				return nil, nil, err
			}
			continue
		}
		authCount++
		go func(item preparedTool, args map[string]any) {
			allow, err := r.cfg.AuthorizeTool(ctx, item.call.ID, item.call.Name, args)
			allowed <- struct {
				item  preparedTool
				allow bool
				err   error
			}{item: item, allow: allow, err: err}
		}(item, args)
	}
	for i := 0; i < authCount; i++ {
		decision := <-allowed
		if decision.err != nil || !decision.allow {
			message := "Error: permission denied by user"
			if decision.err != nil {
				message = "Error: permission request failed: " + decision.err.Error()
			}
			results[decision.item.call.ID] = message
			if err := r.persistPreparedFailure(sess, runID, decision.item.call.ID, message, message); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err := start(decision.item); err != nil {
			return nil, nil, err
		}
	}
	return dispatched, results, nil
}

func (r *Runner) persistPreparedFailure(sess *sessionstore.Session, runID, id, output, errorText string) error {
	for i := range sess.Operations {
		if sess.Operations[i].ID == id {
			sess.Operations[i].Status = sessionstore.StatusFailed
			sess.Operations[i].Output = output
			sess.Operations[i].Error = errorText
			sess.Operations[i].ResultPresent = true
			return r.store.Save(sess)
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
		return r.store.Save(sess)
	}
	return fmt.Errorf("operation %s not found", id)
}

func (r *Runner) appendResults(sess *sessionstore.Session, calls []llm.StreamedToolCall, results map[string]string) {
	for _, call := range calls {
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:              llm.RoleTool,
			ToolCallID:        call.ID,
			ToolResult:        results[call.ID],
			ToolResultPresent: true,
			ResultStatus:      r.statusFor(sess, call.ID),
		})
	}
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
	if result.Error != "" {
		return "Error: " + result.Error
	}
	return r.formatOutput(runID, result.OpID, result.Output)
}

func (r *Runner) formatOutput(runID, operationID, output string) string {
	limit := r.cfg.MaxToolResultChars
	if limit <= 0 || len(output) <= limit {
		return output
	}
	dir := r.cfg.ToolOutputDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "sooraa-tool-outputs")
	}
	safeID := filepath.Base(operationID)
	if safeID == "." || safeID == string(filepath.Separator) || safeID == "" {
		safeID = "result"
	}
	path := filepath.Join(dir, runID, safeID+".txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		if err := os.WriteFile(path, []byte(output), 0o600); err == nil {
			tailLength := limit / 3
			if tailLength < 1024 {
				tailLength = 1024
			}
			tail := output[len(output)-tailLength:]
			return fmt.Sprintf("Large tool output saved to %s (%d bytes).\nTail:\n%s", path, len(output), tail)
		}
	}
	tailLength := limit / 3
	if tailLength < 1024 {
		tailLength = 1024
	}
	return fmt.Sprintf("Large tool output (%d bytes); storage failed.\nTail:\n%s", len(output), output[len(output)-tailLength:])
}

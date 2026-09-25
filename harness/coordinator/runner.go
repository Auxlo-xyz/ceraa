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
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/contextbuilder"
	"ceraa/harness/evaluation"
	"ceraa/harness/goal"
	"ceraa/harness/inbox"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

type RunnerConfig struct {
	SessionID           string
	SystemPrompt        string
	Reasoning           llm.ReasonLevel
	StopWhenIdle        bool
	Model               llm.ProviderConfig
	ContextLimit        int
	MaxOutputTokens     int
	WorkDir             string
	ToolOutputDir       string
	MaxToolResultChars  int
	MaxConcurrentTools  int
	GoalDir             string
	ModelCatalogURL     string
	ModelCatalogCache   string
	ModelCatalogRefresh time.Duration
	MemoryContext       string
	SkillContext        string
	EvaluationStore     *evaluation.Store
	Inbox               *inbox.Inbox
	OperationPool       *operation.LocalManager
	Compactor           *compaction.Compactor
	ResolveModel        func(context.Context) (llm.ProviderConfig, error)
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
	OnToolStart       func(string, string, map[string]any)
	OnToolDone        func(string, string, operation.ResultEvent)
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
	cfg        RunnerConfig
	store      *sessionstore.Store
	reg        *tool.Registry
	opMgr      *operation.LocalManager
	catalog    *llm.CatalogClient
	evaluation *evaluation.Store
	cb         Callbacks
}

func promptCacheKey(model llm.ProviderConfig, sessionID string) string {
	digest := sha256.Sum256([]byte(model.BaseURL + "\x00" + model.Model + "\x00" + sessionID))
	return fmt.Sprintf("ceraa-%x", digest)
}

func (r *Runner) promptCacheKey() string {
	return promptCacheKey(r.cfg.Model, r.cfg.SessionID)
}

func New(cfg RunnerConfig, store *sessionstore.Store, reg *tool.Registry) *Runner {
	if cfg.Model.BaseURL == "" {
		cfg.Model = llm.DefaultConfig()
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
	if cfg.MaxConcurrentTools <= 0 {
		cfg.MaxConcurrentTools = 8
	}
	if cfg.GoalDir == "" {
		cfg.GoalDir = "sessions"
	}
	if cfg.ToolOutputDir == "" {
		cfg.ToolOutputDir = filepath.Join(os.TempDir(), "ceraa-tool-outputs")
	}
	opMgr := cfg.OperationPool
	if opMgr == nil {
		opMgr = operation.NewWithBuffer(&toolRunner{reg: reg}, 4096)
	}
	opMgr.SetConcurrencyLimit(cfg.MaxConcurrentTools)
	catalog := llm.NewCatalogClient(cfg.ModelCatalogURL, cfg.ModelCatalogCache, cfg.ModelCatalogRefresh)
	return &Runner{cfg: cfg, store: store, reg: reg, opMgr: opMgr, catalog: catalog, evaluation: cfg.EvaluationStore, cb: cfg.Callbacks}
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
		sess.Messages = append(sess.Messages, sessionstore.Message{Role: llm.RoleSystem, Content: r.systemPrompt()})
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
	goalUsageBase := 0
	goalContinuations := 0
	correctiveToolRetryUsed := false
	finalContentRetryUsed := false
	forceFinalResponse := false
	for {
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

		modelConfig := r.cfg.Model
		if r.cfg.ResolveModel != nil {
			resolved, resolveErr := r.cfg.ResolveModel(ctx)
			if resolveErr != nil {
				return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, fmt.Errorf("resolve provider credentials: %w", resolveErr))
			}
			modelConfig = resolved
		}
		client := llm.New(modelConfig)
		toolDefs := r.toolDefs()
		requestToolChoice := ""
		if forceFinalResponse {
			requestToolChoice = "none"
		}
		request := llm.StreamRequest{
			Messages:       r.messages(sess, toolDefs),
			Tools:          toolDefs,
			Reason:         r.cfg.Reasoning,
			MaxTokens:      r.cfg.MaxOutputTokens,
			ToolChoice:     requestToolChoice,
			PromptCacheKey: promptCacheKey(modelConfig, r.cfg.SessionID),
			SessionID:      r.cfg.SessionID,
		}
		breakdown := llm.EstimateTokenBreakdown(request.Messages, request.Tools)
		var content strings.Builder
		var reasoning strings.Builder
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
				if r.cb.OnReasoning != nil {
					r.cb.OnReasoning(reasoning.String())
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
				answer = "The model completed without producing a final response."
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
							_ = goalStore.Save(r.cfg.SessionID, current)
						} else {
							if saveErr := goalStore.Save(r.cfg.SessionID, current); saveErr == nil {
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
		if err := r.store.Save(sess); err != nil {
			return Result{Usage: runUsage, Cost: runCost, PricingStatus: pricingStatus}, finish(sessionstore.RunFailed, err)
		}

		prepared, results, corrective, err := r.prepareTools(ctx, sess, runID, calls)
		if err != nil {
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
	items := r.reg.All()
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
	limit := r.cfg.ContextLimit - r.cfg.MaxOutputTokens - llm.EstimateToolDefinitionTokens(tools)
	if limit <= 0 {
		limit = r.cfg.ContextLimit - r.cfg.MaxOutputTokens
	}
	if limit <= 0 {
		limit = 216000
	}
	request := contextbuilder.New(&history, r.systemPrompt()).BuildWithLimit(limit)
	request.Messages = normalizeDispatcherMessages(request.Messages)
	return request.Messages
}

func normalizeDispatcherMessages(items []llm.Item) []llm.Item {
	callNames := make(map[string]string)
	for i := range items {
		switch items[i].Kind {
		case "tool_call":
			items[i] = normalizeLegacyRunToolCall(items[i])
			if items[i].Name != "" {
				callNames[items[i].CallID] = items[i].Name
			}
		case "tool_calls":
			for j := range items[i].ToolCalls {
				items[i].ToolCalls[j] = normalizeLegacyRunToolCallRef(items[i].ToolCalls[j])
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

func (r *Runner) systemPrompt() string {
	parts := []string{r.cfg.SystemPrompt}
	if strings.TrimSpace(r.cfg.SkillContext) != "" {
		parts = append(parts, "<approved-skills>\n"+strings.TrimSpace(r.cfg.SkillContext)+"\n</approved-skills>")
	}
	if strings.TrimSpace(r.cfg.MemoryContext) != "" {
		parts = append(parts, strings.TrimSpace(r.cfg.MemoryContext))
	}
	return strings.Join(parts, "\n\n")
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
	"file_edit":      "edit",
	"web_fetch":      "webfetch",
}

var canonicalToolArgumentAliases = map[string]map[string]string{
	"read":     {"file": "path", "filename": "path", "file_path": "path"},
	"write":    {"file": "path", "filename": "path", "file_path": "path"},
	"edit":     {"file": "path", "filename": "path", "file_path": "path"},
	"bash":     {"cmd": "command", "script": "command", "shell_command": "command"},
	"webfetch": {"uri": "url", "link": "url"},
}

func canonicalToolName(reg *tool.Registry, requested string) string {
	requested = strings.TrimSpace(requested)
	if _, ok := reg.Get(requested); ok {
		return requested
	}
	canonical, ok := canonicalToolAliases[requested]
	if !ok {
		return requested
	}
	if _, active := reg.Get(canonical); active {
		return canonical
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
	start := func(item preparedTool, forceSequential bool) error {
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
		if forceSequential {
			r.opMgr.AddSequential(op)
		} else if sequential, ok := item.tool.(tool.SequentialTool); ok && sequential.Sequential() {
			r.opMgr.AddSequential(op)
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
	hasSequential := false
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
		if sequential, ok := item.tool.(tool.SequentialTool); ok && sequential.Sequential() {
			hasSequential = true
		}
	}
	if hasSequential {
		for _, item := range authorized {
			if err := start(item, hasSequential); err != nil {
				return nil, nil, false, err
			}
		}
	} else {
		for _, item := range authorized {
			if err := start(item, hasSequential); err != nil {
				return nil, nil, false, err
			}
		}
	}
	return dispatched, results, corrective, nil
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
			// Every function call must be answered by exactly one tool message.
			// prepareTools can decline a call, and appending an empty result for it
			// produced a second message for the same call id, which providers
			// reject and which left the call permanently unanswered.
			continue
		}
		sess.Messages = append(sess.Messages, sessionstore.Message{
			Role:              llm.RoleTool,
			ToolCallID:        call.ID,
			ToolName:          call.Name,
			ToolResult:        result,
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

func (r *Runner) formatOutput(runID, operationID, output string) string {
	limit := r.cfg.MaxToolResultChars
	if limit <= 0 || len(output) <= limit {
		return output
	}
	dir := r.cfg.ToolOutputDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "ceraa-tool-outputs")
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

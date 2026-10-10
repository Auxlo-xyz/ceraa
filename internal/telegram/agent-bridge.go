package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ceraa/harness/bond"
	"ceraa/harness/compaction"
	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/permission"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal/buddyauth"
	"ceraa/internal/claudeauth"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/deepseekauth"
	"ceraa/internal/geminiauth"
	"ceraa/internal/hermesauth"
	"ceraa/internal/logger"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
	"ceraa/internal/prompt"
	"ceraa/internal/qoderauth"
	"ceraa/internal/secret"
	"ceraa/internal/xaiauth"
)

// memoryEntries lists the store for sidekick delivery.
//
// A closure rather than a slice, so an edit to the store during a long run reaches
// the next tool call instead of being frozen at construction.
func memoryEntries(store *memory.Store, projectID, sessionID string) func() ([]memory.Entry, error) {
	if store == nil {
		return nil
	}
	return func() ([]memory.Entry, error) {
		return store.List(projectID, sessionID)
	}
}

func runAgentContext(parent context.Context, sessionID string, user string, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string), onStepReasoningOnly func()) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runAgentWithConfigContext(parent, sessionID, user, cfg, onContent, onReasoning, onTool, onAnswer, onStepReasoningOnly)
}

func runAgentWithConfigContext(parent context.Context, sessionID string, user string, cfg *config.Config, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string), onStepReasoningOnly func()) error {
	return runAgentWithConfigContextAt(parent, sessionID, user, cfg, "", onContent, onReasoning, onTool, onAnswer, onStepReasoningOnly)
}

func runAgentWithConfigContextAt(parent context.Context, sessionID string, user string, cfg *config.Config, workdirOverride string, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string), onStepReasoningOnly func()) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if cfg.MemoryFile == "" {
		cfg.MemoryFile = "memory"
	}
	if cfg.SkillFile == "" {
		cfg.SkillFile = "skills"
	}
	if cfg.EvaluationFile == "" {
		cfg.EvaluationFile = "evaluations.json"
	}
	store, err := sessionstore.New(cfg.SessionFile)
	if err != nil {
		return err
	}
	memoryStore, err := memory.New(cfg.MemoryFile)
	if err != nil {
		return err
	}
	skillStore, err := skills.New(cfg.SkillFile)
	if err != nil {
		return err
	}
	evaluationStore, err := evaluation.New(cfg.EvaluationFile)
	if err != nil {
		return err
	}
	projectRoot := workdirOverride
	if projectRoot == "" {
		projectRoot = defaultState.projectForSession(sessionID)
	}
	if projectRoot == "" {
		return errors.New("no project selected")
	}
	userText := user
	_, agentMode := defaultState.values()
	if agentMode == "plan" {
		userText = "[PLAN MODE] Do not modify files or execute commands. Produce a concrete implementation plan and ask before any state-changing action.\n\n" + userText
	}
	// One source of truth: config.json names the active provider, and this is the
	// only place a run learns it.
	//
	// This used to prefer the Telegram state and then let cfg.ActiveProvider
	// override it, so a run could use a different provider from the one the menu
	// had just saved, with no error anywhere. There is now nothing to override.
	providerID := cfg.EffectiveProviderID()
	providerConfig := activeProviderConfig(cfg, providerID)
	modelConfig := llm.FromProvider(providerConfig.URL, providerConfig.APIKey, providerConfig.Model)
	modelConfig.PromptCacheMode = providerConfig.PromptCacheMode
	modelConfig.PromptCacheTTL = providerConfig.PromptCacheTTL
	modelConfig.GeminiThoughtSignatures = telegramProviderUsesGeminiThoughtSignatures(providerID, providerConfig)
	compactor := newCompactor(cfg, providerID, sessionID)
	if learn, enabled := memory.MakeLearner(modelConfig, memoryStore, projectRoot); enabled {
		compactor.Learn = learn
	}
	// The bond rides the same checkpoint: a compaction is the only place a
	// transcript has been condensed on purpose, so it is the only place a
	// context worth reading exists, and it costs nothing per turn.
	if bl, enabled := bond.MakeLearner(modelConfig, paths.BondFile()); enabled {
		compactor.Learn = compaction.ChainLearn(compactor.Learn, bl)
	}
	resolveModel := providerModelResolver(providerConfig, sessionID)
	compactor.ResolveModel = resolveModel
	// Startup snapshot, kept as a fallback and as the first render. The live
	// renderer below re-reads the file every turn, so an edit to
	// memory/AGENTS.md reaches the model without a restart.
	memoryContext, _ := memoryStore.RenderContext(projectRoot, sessionID, 2000)
	liveMemory := memory.Live(memoryStore, projectRoot, sessionID, filepath.Join(projectRoot, memory.DefaultMemoryFile), 4000)
	skillContext, _ := skillStore.CatalogContext(2000)
	reasoning := llm.ReasonLevel(effectiveReasoning(providerConfig, defaultState))
	// Recorded because a provider may reject or ignore a level, and the only way
	// to tell a rejected level from a model that chose not to think is to know
	// what was asked for.
	logger.Info(fmt.Sprintf("agent run: provider=%s model=%s reasoning=%q native_gemini=%v host=%s",
		providerID, providerConfig.Model, string(reasoning), llm.UsesGeminiNative(providerConfig.Model, providerConfig.URL), providerConfig.URL))
	registry := telegramRegistry(cfg, user, skillStore)
	// Subagents are attached here, where a run knows its provider, its project and
	// its stores. Without this the task tool is registered and answers every call
	// with "no factory is attached", which is the same failure the browser tool had:
	// a tool the model can see and use that cannot do anything.
	attachSubagents(registry, &subagentFactory{
		cfg:             cfg,
		user:            user,
		parentSession:   sessionID,
		projectRoot:     projectRoot,
		providerID:      providerID,
		providerCfg:     providerConfig,
		resolveModel:    resolveModel,
		reasoning:       reasoning,
		parentPrompt:    prompt.System,
		skillStore:      skillStore,
		evaluationStore: evaluationStore,
		store:           store,
		memoryContext:   memoryContext,
		skillContext:    skillContext,
		WallClock:       15 * time.Minute,
	})
	// view_image follows the same resolved provider as the run, so a media call
	// cannot drift onto a different model than the one doing the work.
	attachVisionToRegistry(registry, resolveModel)
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           sessionID,
		Reasoning:           reasoning,
		ReasoningEfforts:    providerConfig.ReasoningEfforts,
		ReasoningDefault:    providerConfig.ReasoningDefault,
		SystemPrompt:        prompt.System,
		Model:               modelConfig,
		ResolveModel:        resolveModel,
		ContextLimit:        providerConfig.ContextLimit,
		MaxOutputTokens:     providerConfig.MaxOutputTokens,
		WorkDir:             projectRoot,
		ToolOutputDir:       cfg.ToolOutputDir,
		MaxToolResultChars:  cfg.MaxToolResultChars,
		MaxConcurrentTools:  cfg.MaxConcurrentTools,
		GoalDir:             cfg.SessionFile,
		ModelCatalogURL:     cfg.ModelCatalogURL,
		ModelCatalogCache:   cfg.ModelCatalogCache,
		ModelCatalogRefresh: time.Duration(cfg.ModelCatalogRefreshHours) * time.Hour,
		MemoryContext:       memoryContext,
		MemoryContextFunc:   memory.Combine(memory.LiveBond(paths.BondFile(), 2000), liveMemory),
		MemoryDelivery:      cfg.MemoryDelivery,
		MemoryEntries:       memoryEntries(memoryStore, projectRoot, sessionID),
		SkillContext:        skillContext,
		EvaluationStore:     evaluationStore,
		Compactor:           compactor,
		ForcedCompaction:    func() bool { return consumeForcedCompaction(sessionID) },
		NeedsAuthorization: func(name string, args map[string]any) bool {
			return needsPermission(name, fmt.Sprint(args))
		},
		AuthorizeTool: func(ctx context.Context, callID, name string, args map[string]any) (bool, error) {
			decision, err := permission.Default.Create(ctx, sessionID, callID, name, 5*time.Minute, func() {
				event.Default.Publish(event.Event{Type: event.TypePermission, SessionID: sessionID, Data: map[string]any{"tool": name, "args": args, "request_id": callID}})
			})
			if err != nil {
				return false, err
			}
			return decision == "allowOnce" || decision == "allowAlways", nil
		},
		Callbacks: coordinator.Callbacks{
			// Structured events go on the bus for every surface, not just the
			// one that asked for this run. A subscriber that has no renderer of
			// its own still has to be able to watch, which is what the Mini App
			// does; it gets a delta and a call id rather than a line that was
			// already flattened into a sentence.
			OnContent: func(content string) {
				event.Default.Publish(event.Event{Type: event.TypeContent, SessionID: sessionID, Data: map[string]any{"delta": content}})
				if onContent != nil {
					onContent(content)
				}
			},
			OnReasoning: func(reasoning string) {
				event.Default.Publish(event.Event{Type: event.TypeThinking, SessionID: sessionID, Data: map[string]any{"delta": reasoning}})
				if onReasoning != nil {
					onReasoning(reasoning)
				}
			},
			OnStepReasoningOnly: func() {
				if onStepReasoningOnly != nil {
					onStepReasoningOnly()
				}
			},
			OnToolStart: func(callID, name string, args map[string]any) {
				event.Default.Publish(event.Event{Type: event.TypeToolStart, SessionID: sessionID, Data: map[string]any{"call_id": callID, "tool": name, "args": args}})
				if onTool != nil {
					// The tool name is carried ahead of the label so the line can
					// read "💻 bash <command>" rather than the harness's sentence
					// about the call. parseToolActivity splits the two back apart.
					onTool(callID, "🔄 "+toolNameToken(name)+toolDisplayName(name)+toolArgsSummary(name, args))
				}
			},
			OnToolDone: func(callID, name string, result operation.ResultEvent) {
				status := "✅"
				label := toolDisplayName(name)
				if result.Status != operation.StatusCompleted {
					status = "⚠️"
				}
				event.Default.Publish(event.Event{Type: event.TypeToolEnd, SessionID: sessionID, Data: map[string]any{
					"call_id": callID, "tool": name, "label": label,
					"status": string(result.Status), "outcome": toolResultLabel(result),
				}})
				if onTool != nil {
					// The counts ride at the end of the line and are pulled off by
					// parseToolActivity. They go here rather than into the label
					// because the rendered word is the tool name, so a label
					// suffix would be parsed and then never shown.
					counts := ""
					if delta := formatLineDelta(result); delta != "" {
						counts = " " + lineDeltaTokenPrefix + " " + delta
					}
					onTool(callID, fmt.Sprintf("%s %s%s %s%s", status, toolNameToken(name), label, toolResultLabel(result), counts))
				}
			},
			OnRetry: func(err error, attempt int, delay time.Duration) {
				if onTool == nil {
					return
				}
				// Shown rather than logged, because a gateway backoff can be a
				// minute and the alternative is a silent wait.
				onTool("retry", fmt.Sprintf("\u23f3 Upstream error, retrying (%d) in %s: %s",
					attempt, delay.Round(time.Second), shortProviderError(err)))
			},
			OnCompactionStart: func() {
				if onTool != nil {
					onTool("compaction", "⚡ Compacting context...")
				}
			},
			OnCompactionDone: func(ok bool, err error) {
				if onTool != nil {
					if ok {
						onTool("compaction", "✅ Compaction completed")
					} else if err != nil {
						onTool("compaction", "⚠️ Compaction skipped")
					}
				}
			},
			// Usage and cost still stay off the bus. That was written when the
			// only production subscriber answered permission and question
			// requests, so TypeTokens and TypeCost fired on every usage delta of
			// every run into nobody; the subscriber changed, the noise did not.
			// Both are per-run totals in the session record now, which is where
			// the Mini App reads them from, so a live surface pays nothing to
			// know what a run cost.
			OnDone: func(answer string) {
				if answer != "" {
					event.Default.Publish(event.Event{Type: event.TypeComplete, SessionID: sessionID, Data: map[string]any{"text": answer}})
				}
				if onAnswer != nil && answer != "" {
					onAnswer(answer)
				}
			},
		},
	}, store, registry)
	_, err = runner.Run(parent, userText)
	if err != nil {
		event.Default.Publish(event.Event{Type: event.TypeError, SessionID: sessionID, Data: map[string]any{"message": err.Error()}})
	}
	return err
}

// telegramRegistry builds the tool set for a Telegram run.
//
// send_media is added here and not to the default registry: it needs a
// destination, and on the command line there is nowhere to send to. The sender
// closes over the token and the chat, so the model is handed a tool that can
// deliver a file without ever being able to read either.
//
// The destination is the authorised user, which for a single-user bot is the
// chat the user is already in. Resolving it here rather than threading it
// through the run means there is exactly one place that decides where output
// goes.
// resolveMediaToken is the credential the send_media tool delivers with. It
// resolves in the same order the daemon does: environment first, config
// second. A stale config value must never shadow a working environment one,
// or the bot talks while its own media tool answers 401 on the same chat.
func resolveMediaToken(cfg *config.Config) string {
	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		return token
	}
	return cfg.Telegram.BotToken
}

func telegramRegistry(cfg *config.Config, user string, skillStore *skills.Store) *tool.Registry {
	registry := tool.DefaultRegistryForUser(user, skillStore)
	token := resolveMediaToken(cfg)
	chatID := cfg.Telegram.AllowedUserID
	if chatID == 0 {
		chatID = defaultState.allowedUserID()
	}
	if chatID == 0 {
		if parsed, err := strconv.ParseInt(os.Getenv("TELEGRAM_ALLOWED_USER_ID"), 10, 64); err == nil {
			chatID = parsed
		}
	}
	if token != "" && chatID != 0 {
		registry.Register(&tool.SendMediaTool{Sender: newTelegramMediaSender(token, chatID)})
	}
	return registry
}

// attachVisionToRegistry gives view_image the run's resolved model, so a media
// call follows the active provider instead of whatever was configured at build
// time.
func attachVisionToRegistry(registry *tool.Registry, resolve func(ctx context.Context) (llm.ProviderConfig, error)) {
	attachVision(registry, resolve, func(string) string { return "" })
}

// effectiveReasoning picks the reasoning level for a run. A provider profile may
// pin one; otherwise the runtime value chosen from the Thinking menu applies, and
// an empty result leaves the provider on its own default.
func effectiveReasoning(provider config.LLMProvider, state *bridgeState) string {
	if trimmed := strings.TrimSpace(provider.Reasoning); trimmed != "" {
		return trimmed
	}
	if state != nil {
		return state.reasoning()
	}
	return ""
}

// telegramProviderUsesGeminiThoughtSignatures is kept as a named seam for the
// runner's tests, but the rule itself lives in the transport and depends only
// on the model and host. It used to return false for the "default" provider,
// which silently disabled the signature round-trip for any env-configured
// Gemini model and made every follow-up turn fail with a 400.
func telegramProviderUsesGeminiThoughtSignatures(_ string, provider config.LLMProvider) bool {
	return llm.UsesGeminiThoughtSignatures(provider.Model, provider.URL)
}

func activeProviderConfig(cfg *config.Config, providerID string) config.LLMProvider {
	result := cfg.LLM
	profile, ok := cfg.ProviderProfile(providerID)
	if custom, exists := defaultState.providerProfiles()[providerID]; exists {
		profile = custom
		ok = true
	}
	if !ok {
		return result
	}
	if profile.URL != "" {
		result.URL = profile.URL
	}
	if profile.APIKey != "" {
		result.APIKey = profile.APIKey
	}
	if profile.Model != "" {
		result.Model = profile.Model
	}
	if profile.Reasoning != "" {
		result.Reasoning = profile.Reasoning
	}
	if profile.ContextLimit > 0 {
		result.ContextLimit = profile.ContextLimit
	}
	if profile.MaxOutputTokens > 0 {
		result.MaxOutputTokens = profile.MaxOutputTokens
	}
	if profile.PromptCacheMode != "" {
		result.PromptCacheMode = profile.PromptCacheMode
	}
	if profile.PromptCacheTTL != "" {
		result.PromptCacheTTL = profile.PromptCacheTTL
	}
	if profile.Subscription != "" {
		result.Subscription = profile.Subscription
	}
	if profile.HermesProvider != "" {
		result.HermesProvider = profile.HermesProvider
	}
	if result.APIKey == "" && providerID != "default" {
		if key, err := secret.Get("provider_api_key_" + providerID); err == nil {
			result.APIKey = key
		}
	}
	return result
}

// minimaxRegionFor picks the MiniMax region a profile belongs to.
//
// The two regions are separate account systems, so a CN profile has to be told
// rather than defaulted to global, where its credential does not exist.
func minimaxRegionFor(provider config.LLMProvider) minimaxauth.Region {
	if strings.EqualFold(strings.TrimSpace(provider.URL), llm.MiniMaxCNBaseURL) {
		return minimaxauth.RegionCN
	}
	return minimaxauth.RegionGlobal
}

// qoderCredentialSource adapts the stored Qoder grant to the transport's
// credential interface, including the refresh the transport attempts once on
// an auth rejection.
type qoderCredentialSource struct{}

func (qoderCredentialSource) QoderUID(context.Context) (string, error) {
	if uid := qoderauth.UID(); uid != "" {
		return uid, nil
	}
	return "", fmt.Errorf("Qoder account has no stored user id: sign in again")
}

func (qoderCredentialSource) QoderToken(ctx context.Context) (string, error) {
	return qoderauth.AccessToken(ctx)
}

func (qoderCredentialSource) QoderMachineID(context.Context) (string, error) {
	return qoderauth.MachineID()
}

func (qoderCredentialSource) RefreshQoderToken(ctx context.Context) (string, error) {
	return qoderauth.NewManager().RefreshToken(ctx)
}

// firstNonEmptyStr returns the first non-blank value.
func firstNonEmptyStr(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func providerModelResolver(provider config.LLMProvider, sessionID string) func(context.Context) (llm.ProviderConfig, error) {
	// One manager for the process: it owns the in-flight login flows and the
	// refresh lock, so a per-request instance would let two refreshes race and
	// revoke each other's rotated token.
	codexTokens := codexauth.NewManager()
	return func(ctx context.Context) (llm.ProviderConfig, error) {
		model := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
		model.PromptCacheMode = provider.PromptCacheMode
		model.PromptCacheTTL = provider.PromptCacheTTL
		switch provider.Subscription {
		case llm.CodexSubscription:
			// The credential comes from the encrypted store, not the profile, and
			// the transport refuses to run without it.
			if provider.URL == "" {
				provider.URL = llm.CodexBaseURL
			}
			model.BaseURL = provider.URL
			model.CodexNative = true
			model.CodexTokens = codexTokens
			return model, nil
		case "xai":
			// The credential comes from the encrypted store, not the profile. Without
			// this case the provider fell through to the default, which reads an API
			// key from the profile, found none, and sent an unauthenticated request.
			credential, err := xaiauth.Resolve(ctx, nil)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			model.BaseURL = firstNonEmptyStr(credential.BaseURL, llm.XaiBaseURL)
			model.APIKey = credential.AccessToken
			model.XaiNative = true
			model.XaiTokens = xaiauth.NewTokenSource()
			return model, nil
		case "minimax":
			credential, err := minimaxauth.Resolve(ctx, minimaxauth.NewStore(minimaxRegionFor(provider)))
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// Host and header come from the grant, not the stored profile: the
			// agent token is accepted only by the agent backend, and a profile
			// left on the platform URL signs in correctly and then 401s on every
			// request asking for an API secret key this login cannot have.
			model.BaseURL = minimaxauth.BaseURLFor(credential.Region)
			model.MessagesAuthStyle = minimaxauth.MessagesAuthFor(credential.Region)
			model.APIKey = credential.AccessToken
			// Served by the Messages API, so it needs the Anthropic transport.
			model.AnthropicNative = true
			model.AnthropicTokens = minimaxauth.NewTokenSource(credential.Region)
			return model, nil
		case "deepseek":
			if !deepseekauth.SignedIn() {
				return llm.ProviderConfig{}, fmt.Errorf("DeepSeek account is not signed in: sign in from the provider menu first")
			}
			model.BaseURL = llm.DeepSeekBaseURL
			model.MessagesAuthStyle = llm.DeepSeekMessagesAuth
			model.AnthropicNative = true
			model.AnthropicTokens = deepseekauth.NewTokenSource()
			return model, nil
		case "claude":
			if !claudeauth.SignedIn() {
				return llm.ProviderConfig{}, fmt.Errorf("Claude account is not signed in: sign in from the provider menu first")
			}
			// Native endpoint, Bearer plus the OAuth beta pair. The grant is
			// accepted only at the inference origin.
			model.BaseURL = llm.ClaudeBaseURL
			model.MessagesAuthStyle = llm.ClaudeMessagesAuth
			model.AnthropicNative = true
			model.AnthropicTokens = claudeauth.NewTokenSource()
			return model, nil
		case "cline":
			credential, err := clineauth.Resolve(ctx, nil)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			model.BaseURL = firstNonEmptyStr(provider.URL, llm.ClineGatewayBase)
			model.APIKey = credential.APIKey()
			// The gateway answers 401 without this header set, and Go's default
			// "Go-http-client/1.1" was being sent.
			if len(model.ExtraHeaders) == 0 || !clineauth.HasClineHeaders(model.ExtraHeaders) {
				model.ExtraHeaders = clineauth.RequestHeaders(sessionID)
			}
			return model, nil
		case "opencode":
			credential, err := opencodeauth.CredentialFor(ctx, provider.APIKey)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// Zen is OpenAI-compatible, so it needs no transport of its own.
			return opencodeauth.ApplyCredential(model, credential, sessionID), nil
		case "gemini":
			if !geminiauth.SignedIn() {
				return llm.ProviderConfig{}, fmt.Errorf("Gemini account is not signed in: sign in from the provider menu first")
			}
			model.BaseURL = firstNonEmptyStr(provider.URL, llm.CodeAssistBaseURL)
			model.CodeAssistNative = true
			model.CodeAssistTokens = geminiauth.NewTokenSource()
			return model, nil
		case "workbuddy":
			manager := buddyauth.NewWorkBuddyManager()
			if !manager.Authenticated() {
				return llm.ProviderConfig{}, fmt.Errorf("WorkBuddy account is not signed in: sign in from the provider menu first")
			}
			token, err := manager.AccessToken(ctx)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			model.BaseURL = llm.NormalizeWorkBuddyURL(firstNonEmptyStr(provider.URL, llm.WorkBuddyBaseURL))
			model.APIKey = token
			model.ExtraHeaders = llm.WorkBuddyRequestHeaders(provider.Model)
			return model, nil
		case "qoder":
			// The encrypted endpoint needs per-request signing, so the
			// credential source travels with the config and the transport
			// resolves identity (and refreshes it) per attempt.
			if !qoderauth.Authenticated() {
				return llm.ProviderConfig{}, fmt.Errorf("Qoder account is not signed in: sign in from the provider menu first")
			}
			model.BaseURL = firstNonEmptyStr(provider.URL, llm.QoderBaseURL)
			model.QoderNative = true
			model.QoderCredentials = qoderCredentialSource{}
			return model, nil
		case "hermes":
			store := hermesauth.NewStore()
			credential, err := store.Resolve(ctx, provider.HermesProvider)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// Shared with the CLI entry point, so the three transports cannot drift
			// apart again.
			return hermesauth.ApplyCredential(model, credential, store)
		default:
			return model, nil
		}
	}
}

func newCompactor(cfg *config.Config, providerID, sessionID string) *compaction.Compactor {
	provider := activeProviderConfig(cfg, providerID)
	pcfg := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	pcfg.PromptCacheMode = provider.PromptCacheMode
	pcfg.PromptCacheTTL = provider.PromptCacheTTL
	pcfg.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(provider.Model, provider.URL)
	compactor := compaction.New(pcfg)
	compactor.ContextLimit = provider.ContextLimit
	compactor.MaxOutputTokens = provider.MaxOutputTokens
	compactor.ArchiveDir = cfg.SessionFile
	if enabled, configured := defaultState.autoCompaction(); configured {
		compactor.Settings.Auto = enabled
	}
	return compactor
}

// needsPermission decides which calls must be approved before they run.
//
// The browser tool is deliberately absent. It reads and renders pages, and the
// approval prompt would fire on every screenshot, which makes UI work unusable;
// a browser cannot modify this host's filesystem, and a page can only be fetched
// over http or https, which is checked when the call is translated.
func needsPermission(toolName, args string) bool {
	switch toolName {
	case "write", "edit_line", "batch_edit_file", "delete", "move", "copy":
		return true
	case "bash":
		lower := strings.ToLower(args)
		risky := []string{"rm ", "rm\t", "unlink ", "shred ", "mv ", "cp ", "truncate ", "tee ", "git clean", "git reset --hard", "git checkout --", "git restore", "mkfs", "dd if=", "chmod ", "chown ", "kill ", "pkill ", "shutdown", "reboot", "systemctl ", "docker ", "kubectl ", "sudo ", "curl ", "wget ", "git push", " -delete", ":(){:|:&};:"}
		for _, r := range risky {
			if strings.Contains(lower, r) {
				return true
			}
		}
	}
	return false
}

// shortProviderError keeps a retry notice to one readable line.
//
// The full error carries the whole provider body, which in a status line inside a
// chat message is unreadable and, on some providers, long enough to break layout.
func shortProviderError(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '{'); i > 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 80 {
		msg = msg[:80] + "..."
	}
	if msg == "" {
		return "unknown"
	}
	return msg
}

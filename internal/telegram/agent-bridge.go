package telegram

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/permission"
	"ceraa/harness/search"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal/config"
	"ceraa/internal/hermesauth"
	"ceraa/internal/logger"
	"ceraa/internal/prompt"
	"ceraa/internal/secret"
)

func runAgent(sessionID string, user string, onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
	return runAgentContext(context.Background(), sessionID, user, nil, onReasoning, onTool, onAnswer)
}

func runAgentContext(parent context.Context, sessionID string, user string, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runAgentWithConfigContext(parent, sessionID, user, cfg, onContent, onReasoning, onTool, onAnswer)
}

func runAgentWithConfig(sessionID string, user string, cfg *config.Config, onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
	return runAgentWithConfigContext(context.Background(), sessionID, user, cfg, nil, onReasoning, onTool, onAnswer)
}

func runAgentWithConfigContext(parent context.Context, sessionID string, user string, cfg *config.Config, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
	return runAgentWithConfigContextAt(parent, sessionID, user, cfg, "", onContent, onReasoning, onTool, onAnswer)
}

func runAgentWithConfigContextAt(parent context.Context, sessionID string, user string, cfg *config.Config, workdirOverride string, onContent func(string), onReasoning func(string), onTool func(string, string), onAnswer func(string)) error {
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
	_, agentMode, _ := defaultState.values()
	if agentMode == "plan" {
		userText = "[PLAN MODE] Do not modify files or execute commands. Produce a concrete implementation plan and ask before any state-changing action.\n\n" + userText
	}
	providerID := defaultState.provider()
	if cfg.ActiveProvider != "" {
		providerID = cfg.ActiveProvider
	} else if cfg.ProviderProfiles == nil {
		providerID = "default"
	}
	providerConfig := activeProviderConfig(cfg, providerID)
	modelConfig := llm.FromProvider(providerConfig.URL, providerConfig.APIKey, providerConfig.Model)
	modelConfig.PromptCacheMode = providerConfig.PromptCacheMode
	modelConfig.PromptCacheTTL = providerConfig.PromptCacheTTL
	modelConfig.GeminiThoughtSignatures = telegramProviderUsesGeminiThoughtSignatures(providerID, providerConfig)
	compactor := newCompactor(cfg, providerID, sessionID)
	resolveModel := providerModelResolver(providerConfig)
	compactor.ResolveModel = resolveModel
	memoryContext, _ := memoryStore.RenderContext(projectRoot, sessionID, 2000)
	skillContext, _ := skillStore.ActiveContext(2000)
	reasoning := llm.ReasonLevel(effectiveReasoning(providerConfig, defaultState))
	// Recorded because a provider may reject or ignore a level, and the only way
	// to tell a rejected level from a model that chose not to think is to know
	// what was asked for.
	logger.Info(fmt.Sprintf("agent run: provider=%s model=%s reasoning=%q native_gemini=%v",
		providerID, providerConfig.Model, string(reasoning), llm.UsesGeminiNative(providerConfig.Model, providerConfig.URL)))
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           sessionID,
		Reasoning:           reasoning,
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
		SkillContext:        skillContext,
		EvaluationStore:     evaluationStore,
		Compactor:           compactor,
		AdjustSpec: func(spec *operation.Spec) {
			if spec.Type == "fts_search" {
				spec.Args["path"] = projectRoot
				spec.Args["db_path"] = search.ProjectIndexPath(cfg.WorkspaceRootPath(), projectRoot)
			}
		},
		ForcedCompaction: func() bool { return consumeForcedCompaction(sessionID) },
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
			OnContent: func(content string) {
				if onContent != nil {
					onContent(content)
				}
			},
			OnReasoning: func(reasoning string) {
				if onReasoning != nil {
					onReasoning(reasoning)
				}
			},
			OnToolStart: func(callID, name string, args map[string]any) {
				if onTool != nil {
					onTool(callID, "🔄 "+toolDisplayName(name)+toolArgsSummary(name, args))
				}
			},
			OnToolDone: func(callID, name string, result operation.ResultEvent) {
				status := "✅"
				label := toolDisplayName(name)
				if result.Status != operation.StatusCompleted {
					status = "⚠️"
				}
				if onTool != nil {
					onTool(callID, fmt.Sprintf("%s %s %s", status, label, toolResultLabel(result)))
				}
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
			OnUsage: func(usage llm.UsageInfo) {
				tokens := usage.InputTokens + usage.OutputTokens
				event.Default.Publish(event.Event{Type: event.TypeTokens, SessionID: sessionID, Data: map[string]any{"tokens": tokens}})
			},
			OnCost: func(cost llm.CostInfo, status string) {
				text := "unavailable"
				if cost.Known {
					text = fmt.Sprintf("$%.6f", cost.TotalUSD)
				} else if status != "" && status != "unavailable" {
					text += " (" + status + ")"
				}
				event.Default.Publish(event.Event{Type: event.TypeCost, SessionID: sessionID, Data: map[string]any{"cost": text, "status": status}})
			},
			OnDone: func(answer string) {
				if onAnswer != nil && answer != "" {
					onAnswer(answer)
				}
			},
		},
	}, store, telegramRegistry(cfg, user, memoryStore, skillStore))
	_, err = runner.Run(parent, userText)
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
func telegramRegistry(cfg *config.Config, user string, memoryStore *memory.Store, skillStore *skills.Store) *tool.Registry {
	registry := tool.DefaultRegistryForUser(user, memoryStore, skillStore)
	token := cfg.Telegram.BotToken
	if token == "" {
		token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
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

func providerModelResolver(provider config.LLMProvider) func(context.Context) (llm.ProviderConfig, error) {
	return func(ctx context.Context) (llm.ProviderConfig, error) {
		model := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
		model.PromptCacheMode = provider.PromptCacheMode
		model.PromptCacheTTL = provider.PromptCacheTTL
		switch provider.Subscription {
		case "hermes":
			credential, err := hermesauth.NewStore().Resolve(ctx, provider.HermesProvider)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			if credential.BaseURL == "" {
				return llm.ProviderConfig{}, fmt.Errorf("hermes provider %q has no inference base URL", credential.Provider)
			}
			if credential.Transport != "chat_completions" {
				return llm.ProviderConfig{}, fmt.Errorf("hermes provider %q requires the %s transport; native chat adapter is not available yet", credential.Provider, credential.Transport)
			}
			model.BaseURL = credential.BaseURL
			model.APIKey = credential.AccessToken
			// The transport depends on the resolved host, which the native Hermes
			// adapter may have changed.
			model.GeminiNative = llm.UsesGeminiNative(model.Model, model.BaseURL)
			model.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(model.Model, model.BaseURL)
			return model, nil
		default:
			return model, nil
		}
	}
}

func compactionCacheKey(providerURL, model, sessionID string) string {
	digest := sha256.Sum256([]byte(providerURL + "\x00" + model + "\x00" + sessionID))
	return fmt.Sprintf("ceraa-compaction-%x", digest)
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
	compactor.CacheKey = compactionCacheKey(provider.URL, provider.Model, sessionID)
	_, _, variant := defaultState.values()
	if enabled, configured := defaultState.autoCompaction(); configured {
		compactor.Settings.Auto = enabled
	}
	if variant == "compact" {
		compactor.Settings.Keep.Tokens = 4000
		compactor.Settings.Buffer = 30000
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
	case "write", "edit", "delete", "move", "copy":
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

func clearSession(sessionID string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := sessionstore.New(cfg.SessionFile)
	if err != nil {
		return err
	}
	return store.Clear(sessionID)
}

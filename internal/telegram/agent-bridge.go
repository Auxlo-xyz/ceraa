package telegram

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"sooraa/harness/compaction"
	"sooraa/harness/coordinator"
	"sooraa/harness/evaluation"
	"sooraa/harness/event"
	"sooraa/harness/llm"
	"sooraa/harness/memory"
	"sooraa/harness/operation"
	"sooraa/harness/permission"
	"sooraa/harness/search"
	"sooraa/harness/sessionstore"
	"sooraa/harness/skills"
	"sooraa/harness/tool"
	"sooraa/internal/config"
	"sooraa/internal/prompt"
	"sooraa/internal/secret"
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
	memoryContext, _ := memoryStore.RenderContext(projectRoot, sessionID, 2000)
	skillContext, _ := skillStore.ActiveContext(2000)
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           sessionID,
		SystemPrompt:        prompt.System,
		Model:               modelConfig,
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
	}, store, tool.DefaultRegistryForUser(user, memoryStore, skillStore))
	_, err = runner.Run(parent, userText)
	return err
}

func telegramProviderUsesGeminiThoughtSignatures(providerID string, provider config.LLMProvider) bool {
	if providerID == "" || providerID == "default" {
		return false
	}
	model := strings.ToLower(provider.Model)
	return strings.Contains(model, "gemini") || strings.Contains(model, "gemma")
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
	if profile.ContextLimit > 0 {
		result.ContextLimit = profile.ContextLimit
	}
	if profile.MaxOutputTokens > 0 {
		result.MaxOutputTokens = profile.MaxOutputTokens
	}
	if result.APIKey == "" && providerID != "default" {
		if key, err := secret.Get("provider_api_key_" + providerID); err == nil {
			result.APIKey = key
		}
	}
	return result
}

func compactionCacheKey(providerURL, model, sessionID string) string {
	digest := sha256.Sum256([]byte(providerURL + "\x00" + model + "\x00" + sessionID))
	return fmt.Sprintf("sooraa-compaction-%x", digest)
}

func newCompactor(cfg *config.Config, providerID, sessionID string) *compaction.Compactor {
	provider := activeProviderConfig(cfg, providerID)
	pcfg := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	pcfg.PromptCacheMode = provider.PromptCacheMode
	pcfg.PromptCacheTTL = provider.PromptCacheTTL
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

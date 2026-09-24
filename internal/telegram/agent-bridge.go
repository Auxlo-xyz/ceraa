package telegram

import (
	"context"
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
	"sooraa/harness/sessionstore"
	"sooraa/harness/skills"
	"sooraa/harness/tool"
	"sooraa/internal/config"
	"sooraa/internal/prompt"
)

func runAgent(sessionID string, user string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgentContext(context.Background(), sessionID, user, onReasoning, onTool, onAnswer)
}

func runAgentContext(parent context.Context, sessionID string, user string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runAgentWithConfigContext(parent, sessionID, user, cfg, onReasoning, onTool, onAnswer)
}

func runAgentWithConfig(sessionID string, user string, cfg *config.Config, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgentWithConfigContext(context.Background(), sessionID, user, cfg, onReasoning, onTool, onAnswer)
}

func runAgentWithConfigContext(parent context.Context, sessionID string, user string, cfg *config.Config, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
	return runAgentWithConfigContextAt(parent, sessionID, user, cfg, "", onReasoning, onTool, onAnswer)
}

func runAgentWithConfigContextAt(parent context.Context, sessionID string, user string, cfg *config.Config, workdirOverride string, onReasoning func(string), onTool func(string), onAnswer func(string)) error {
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
	userText := user
	_, agentMode, _, _ := defaultState.values()
	if agentMode == "plan" {
		userText = "[PLAN MODE] Do not modify files or execute commands. Produce a concrete implementation plan and ask before any state-changing action.\n\n" + userText
	}
	compactor := newCompactor(cfg)
	memoryContext, _ := memoryStore.RenderContext(projectRoot, sessionID, 2000)
	skillContext, _ := skillStore.ActiveContext(2000)
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           sessionID,
		SystemPrompt:        prompt.System,
		MaxSteps:            cfg.MaxSteps,
		Model:               llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model),
		ContextLimit:        cfg.LLM.ContextLimit,
		MaxOutputTokens:     cfg.LLM.MaxOutputTokens,
		WorkDir:             projectRoot,
		ToolOutputDir:       cfg.ToolOutputDir,
		MaxToolResultChars:  cfg.MaxToolResultChars,
		ModelCatalogURL:     cfg.ModelCatalogURL,
		ModelCatalogCache:   cfg.ModelCatalogCache,
		ModelCatalogRefresh: time.Duration(cfg.ModelCatalogRefreshHours) * time.Hour,
		MemoryContext:       memoryContext,
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
			OnContent: func(content string) {
				event.Default.Publish(event.Event{Type: event.TypeComplete, SessionID: sessionID, Data: map[string]any{"content": content}})
			},
			OnReasoning: func(reasoning string) {
				if onReasoning != nil {
					onReasoning(reasoning)
				}
				event.Default.Publish(event.Event{Type: event.TypeThinking, SessionID: sessionID, Data: map[string]any{"full": reasoning}})
			},
			OnToolStart: func(t tool.Tool, args map[string]any) {
				name, _ := args["tool"].(string)
				argText, _ := args["args"].(string)
				if len(argText) > 200 {
					argText = argText[:200] + "..."
				}
				if onTool != nil {
					onTool("🔧 " + name + " " + argText)
				}
				event.Default.Publish(event.Event{Type: event.TypeToolStart, SessionID: sessionID, Data: args})
			},
			OnToolDone: func(result operation.ResultEvent) {
				status := "✓"
				if result.Status != operation.StatusCompleted {
					status = "✗"
				}
				preview := result.Output
				if len(preview) > 300 {
					preview = preview[:300] + "..."
				}
				if onTool != nil {
					onTool(fmt.Sprintf("%s %s → %s", status, result.OpID, preview))
				}
				event.Default.Publish(event.Event{Type: event.TypeToolEnd, SessionID: sessionID, Data: map[string]any{"op_id": result.OpID, "output": result.Output, "status": string(result.Status)}})
			},
			OnCompactionStart: func() {
				if onTool != nil {
					onTool("⚡ Compacting context...")
				}
				event.Default.Publish(event.Event{Type: event.TypeCompactProgress, SessionID: sessionID, Data: map[string]any{"progress": "start"}})
			},
			OnCompactionDone: func(ok bool, err error) {
				if onTool != nil {
					if ok {
						onTool("✓ Compaction done, recent preserved")
					} else if err != nil {
						onTool("⚠️ Compaction skipped: " + err.Error())
					}
				}
				event.Default.Publish(event.Event{Type: event.TypeCompact, SessionID: sessionID, Data: map[string]any{"done": ok}})
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
				event.Default.Publish(event.Event{Type: event.TypeComplete, SessionID: sessionID, Data: map[string]any{"content": answer}})
				if onAnswer != nil && answer != "" {
					onAnswer(answer)
				}
			},
		},
	}, store, tool.DefaultRegistryForUser(user, memoryStore, skillStore))
	_, err = runner.Run(parent, userText)
	return err
}

func newCompactor(cfg *config.Config) *compaction.Compactor {
	pcfg := llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model)
	compactor := compaction.New(pcfg)
	compactor.ContextLimit = cfg.LLM.ContextLimit
	compactor.MaxOutputTokens = cfg.LLM.MaxOutputTokens
	_, _, variant, _ := defaultState.values()
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
		risky := []string{"rm -rf", "sudo ", "chmod 777", "mkfs", "dd if=", ":(){:|:&};:", "curl ", "wget ", "git push"}
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

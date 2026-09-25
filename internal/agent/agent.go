package agent

import (
	"context"
	"fmt"
	"time"

	"sooraa/harness/compaction"
	"sooraa/harness/coordinator"
	"sooraa/harness/evaluation"
	"sooraa/harness/llm"
	"sooraa/harness/memory"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
	"sooraa/harness/skills"
	"sooraa/harness/tool"
	"sooraa/internal/config"
	"sooraa/internal/prompt"
)

func Run(user string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return RunWithConfig(user, cfg)
}

func RunWithConfig(user string, cfg *config.Config) error {
	return RunWithConfigContext(context.Background(), user, cfg)
}

func RunWithConfigContext(ctx context.Context, user string, cfg *config.Config) error {
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
		return fmt.Errorf("session store: %w", err)
	}
	memoryStore, err := memory.New(cfg.MemoryFile)
	if err != nil {
		return fmt.Errorf("memory store: %w", err)
	}
	skillStore, err := skills.New(cfg.SkillFile)
	if err != nil {
		return fmt.Errorf("skill store: %w", err)
	}
	evaluationStore, err := evaluation.New(cfg.EvaluationFile)
	if err != nil {
		return fmt.Errorf("evaluation store: %w", err)
	}
	provider := cfg.ActiveLLM()
	pcfg := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	pcfg.PromptCacheMode = provider.PromptCacheMode
	pcfg.PromptCacheTTL = provider.PromptCacheTTL
	compactor := compaction.New(pcfg)
	compactor.ContextLimit = provider.ContextLimit
	compactor.MaxOutputTokens = provider.MaxOutputTokens
	compactor.CacheKey = "sooraa-compaction-cli"
	memoryContext, _ := memoryStore.RenderContext("cli", "cli", 2000)
	skillContext, _ := skillStore.ActiveContext(2000)
	modelConfig := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	modelConfig.PromptCacheMode = provider.PromptCacheMode
	modelConfig.PromptCacheTTL = provider.PromptCacheTTL
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           "cli",
		SystemPrompt:        prompt.System,
		Model:               modelConfig,
		ContextLimit:        provider.ContextLimit,
		MaxOutputTokens:     provider.MaxOutputTokens,
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
		Callbacks: coordinator.Callbacks{
			OnContent: func(content string) { fmt.Print(content) },
			OnToolStart: func(_, name string, _ map[string]any) {
				fmt.Printf("\n» %s\n", name)
			},
			OnToolDone: func(_, name string, result operation.ResultEvent) {
				fmt.Printf("%s %s → %d chars\n", result.Status, name, len(result.Output))
			},
			OnCompactionStart: func() { fmt.Println("⚡ Compacting context...") },
		},
	}, store, tool.DefaultRegistryForUser(user, memoryStore, skillStore))
	_, err = runner.Run(ctx, user)
	return err
}

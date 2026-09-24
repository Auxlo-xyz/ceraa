package agent

import (
	"context"
	"fmt"

	"sooraa/harness/compaction"
	"sooraa/harness/coordinator"
	"sooraa/harness/llm"
	"sooraa/harness/operation"
	"sooraa/harness/sessionstore"
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
	store, err := sessionstore.New(cfg.SessionFile)
	if err != nil {
		return fmt.Errorf("session store: %w", err)
	}
	compactor := compaction.New(llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model))
	compactor.ContextLimit = cfg.LLM.ContextLimit
	compactor.MaxOutputTokens = cfg.LLM.MaxOutputTokens
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:       "cli",
		SystemPrompt:    prompt.System,
		MaxSteps:        cfg.MaxSteps,
		Model:           llm.FromProvider(cfg.LLM.URL, cfg.LLM.APIKey, cfg.LLM.Model),
		ContextLimit:    cfg.LLM.ContextLimit,
		MaxOutputTokens: cfg.LLM.MaxOutputTokens,
		Compactor:       compactor,
		Callbacks: coordinator.Callbacks{
			OnContent: func(content string) { fmt.Print(content) },
			OnToolStart: func(_ tool.Tool, args map[string]any) {
				if name, ok := args["tool"].(string); ok {
					fmt.Printf("\n» %s\n", name)
				}
			},
			OnToolDone: func(result operation.ResultEvent) {
				fmt.Printf("%s %s → %d chars\n", result.Status, result.OpID, len(result.Output))
			},
			OnCompactionStart: func() { fmt.Println("⚡ Compacting context...") },
		},
	}, store, tool.DefaultRegistry())
	_, err = runner.Run(ctx, user)
	return err
}

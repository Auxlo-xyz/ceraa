package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/hermesauth"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/paths"
	"ceraa/internal/prompt"
	"ceraa/internal/secret"
	"ceraa/internal/xaiauth"
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
	// Open the encrypted credential store before anything can need a credential.
	//
	// The Telegram daemon does this, the CLI did not, so a provider profile chosen
	// in Telegram was visible here but its stored token was not: the CLI resolved
	// the provider and then failed with no credential, which reads as a broken
	// login rather than an uninitialised store. Subscription transports read their
	// tokens from here, so without it no subscription provider can work from the
	// CLI at all.
	if err := secret.Init(filepath.Join(paths.DataDir(), "secret.key"), filepath.Join(paths.DataDir(), "secrets.json")); err != nil {
		return fmt.Errorf("secret store: %w", err)
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
	pcfg.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(provider.Model, provider.URL)
	compactor := compaction.New(pcfg)
	compactor.ContextLimit = provider.ContextLimit
	compactor.MaxOutputTokens = provider.MaxOutputTokens
	compactor.CacheKey = "ceraa-compaction-cli"
	// Startup snapshot, kept as a fallback and as the first render. The live
	// renderer below re-reads the file every turn.
	memoryContext, _ := memoryStore.RenderContext("cli", "cli", 2000)
	liveMemory := memory.Live(memoryStore, "cli", "cli", filepath.Join(cfg.WorkspaceRootPath(), memory.DefaultMemoryFile), 4000)
	skillContext, _ := skillStore.ActiveContext(2000)
	modelConfig := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	modelConfig.PromptCacheMode = provider.PromptCacheMode
	modelConfig.PromptCacheTTL = provider.PromptCacheTTL
	modelConfig.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(provider.Model, provider.URL)
	resolveModel := func(ctx context.Context) (llm.ProviderConfig, error) {
		switch provider.Subscription {
		case llm.CodexSubscription:
			if provider.URL == "" {
				provider.URL = llm.CodexBaseURL
			}
			modelConfig.BaseURL = provider.URL
			modelConfig.CodexNative = true
			modelConfig.CodexTokens = codexauth.NewManager()
			return modelConfig, nil
		case "hermes":
			credential, err := hermesauth.NewStore().Resolve(ctx, provider.HermesProvider)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			if credential.Transport != "chat_completions" {
				return llm.ProviderConfig{}, fmt.Errorf("hermes provider %q requires the %s transport; native chat adapter is not available yet", credential.Provider, credential.Transport)
			}
			modelConfig.BaseURL = credential.BaseURL
			modelConfig.APIKey = credential.AccessToken
			// The transport depends on the resolved host, which the native Hermes
			// adapter may have changed.
			modelConfig.GeminiNative = llm.UsesGeminiNative(modelConfig.Model, modelConfig.BaseURL)
			modelConfig.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(modelConfig.Model, modelConfig.BaseURL)
			return modelConfig, nil
		case "cline":
			credential, err := clineauth.Resolve(ctx, nil)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// A Cline Account is a hosted gateway with an OpenAI-compatible
			// chat/completions, so it needs no transport of its own.
			modelConfig.BaseURL = firstNonEmptyStr(provider.URL, llm.ClineGatewayBase)
			modelConfig.APIKey = credential.APIKey()
			// The gateway answers 401 without this header set, and Go's default
			// "Go-http-client/1.1" was being sent. A profile's own headers win, so
			// someone who has set them deliberately is not overridden.
			if len(modelConfig.ExtraHeaders) == 0 || !clineauth.HasClineHeaders(modelConfig.ExtraHeaders) {
				// The gateway correlates a request with a conversation through this. The
				// CLI's run id is fixed, which is the truth here: the header names the
				// run, and there is one.
				modelConfig.ExtraHeaders = clineauth.RequestHeaders(agentRunID)
			}
			return modelConfig, nil
		case "minimax":
			credential, err := minimaxauth.Resolve(ctx, minimaxauth.NewStore(regionForProvider(provider)))
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			modelConfig.BaseURL = firstNonEmptyStr(credential.BaseURL, llm.MiniMaxBaseURL)
			modelConfig.APIKey = credential.AccessToken
			// Served by the Messages API, so it needs the Anthropic transport
			// rather than chat completions.
			modelConfig.AnthropicNative = true
			modelConfig.AnthropicTokens = minimaxauth.NewTokenSource(credential.Region)
			return modelConfig, nil
		case "xai":
			credential, err := xaiauth.Resolve(ctx, nil)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			modelConfig.BaseURL = firstNonEmptyStr(credential.BaseURL, llm.XaiBaseURL)
			modelConfig.APIKey = credential.AccessToken
			modelConfig.XaiNative = true
			modelConfig.XaiTokens = xaiauth.NewTokenSource()
			return modelConfig, nil
		default:
			// A plain profile can still need the Messages transport. Anthropic and
			// MiniMax both serve /v1/messages rather than chat completions, and a
			// profile configured for them with a plain API key would otherwise be
			// sent to the wrong endpoint and 404 on a path that reads as a bad
			// model id. Keyed off the host rather than the profile name so a
			// self-hosted Messages-compatible gateway is picked up too.
			if llm.UsesAnthropicMessages(modelConfig.Model, modelConfig.BaseURL) {
				modelConfig.AnthropicNative = true
			}
			return modelConfig, nil
		}
	}
	compactor.ResolveModel = resolveModel
	registry := tool.DefaultRegistryForUser(user, memoryStore, skillStore)
	// The CLI has a provider too, so a media view works the same way there.
	attachVisionToRegistry(registry, resolveModel)
	runner := coordinator.New(coordinator.RunnerConfig{
		SessionID:           "cli",
		SystemPrompt:        prompt.System,
		Model:               modelConfig,
		ResolveModel:        resolveModel,
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
		MemoryContextFunc:   liveMemory,
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
	}, store, registry)
	_, err = runner.Run(ctx, user)
	return err
}

// agentRunID is the run identifier sent to a provider that correlates on it.
const agentRunID = "cli"

func firstNonEmptyStr(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// regionForProvider reads the MiniMax region from a profile.
//
// Defaults to global, which is the deployment most accounts use. A CN account has
// to be told, because the two are separate account systems and a credential does
// not work on the other's host.
func regionForProvider(provider config.LLMProvider) minimaxauth.Region {
	if strings.EqualFold(strings.TrimSpace(provider.URL), llm.MiniMaxCNBaseURL) {
		return minimaxauth.RegionCN
	}
	return minimaxauth.RegionGlobal
}

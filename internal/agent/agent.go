package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/instruction"
	"ceraa/harness/llm"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal"
	"ceraa/internal/claudeauth"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/deepseekauth"
	"ceraa/internal/featuredocs"
	"ceraa/internal/hermesauth"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
	"ceraa/internal/prompt"
	"ceraa/internal/secret"
	"ceraa/internal/subagent"
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
	if err := featuredocs.Install(filepath.Join(paths.DataDir(), "docs")); err != nil {
		return fmt.Errorf("feature docs: %w", err)
	}
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
	compactor.ArchiveDir = cfg.SessionFile
	skillContext, _ := skillStore.CatalogContext(2000)
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
			store := hermesauth.NewStore()
			credential, err := store.Resolve(ctx, provider.HermesProvider)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// All three transports are chosen in one place. The two branches that
			// used to live here and in the bridge each refused anything but chat
			// completions, so the same gap existed twice.
			return hermesauth.ApplyCredential(modelConfig, credential, store)
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
		case "opencode":
			credential, err := opencodeauth.CredentialFor(ctx, provider.APIKey)
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// Zen is OpenAI-compatible, so it needs no transport of its own.
			return opencodeauth.ApplyCredential(modelConfig, credential, agentRunID), nil
		case "minimax":
			credential, err := minimaxauth.Resolve(ctx, minimaxauth.NewStore(regionForProvider(provider)))
			if err != nil {
				return llm.ProviderConfig{}, err
			}
			// Host and header come from the grant, not the stored profile: the
			// agent token is accepted only by the agent backend, and a profile
			// left on the platform URL signs in correctly and then 401s on every
			// request asking for an API secret key this login cannot have.
			modelConfig.BaseURL = minimaxauth.BaseURLFor(credential.Region)
			modelConfig.MessagesAuthStyle = minimaxauth.MessagesAuthFor(credential.Region)
			modelConfig.APIKey = credential.AccessToken
			// Served by the Messages API, so it needs the Anthropic transport
			// rather than chat completions.
			modelConfig.AnthropicNative = true
			modelConfig.AnthropicTokens = minimaxauth.NewTokenSource(credential.Region)
			return modelConfig, nil
		case "deepseek":
			if !deepseekauth.SignedIn() {
				return llm.ProviderConfig{}, fmt.Errorf("DeepSeek account is not signed in: sign in from the provider menu first")
			}
			// Host and header are fixed, not per-region: one account system,
			// one inference origin, one grant. The grant is accepted only at
			// the inference origin.
			modelConfig.BaseURL = llm.DeepSeekBaseURL
			modelConfig.MessagesAuthStyle = llm.DeepSeekMessagesAuth
			// Served by the Messages API, so it needs the Anthropic transport
			// rather than chat completions.
			modelConfig.AnthropicNative = true
			modelConfig.AnthropicTokens = deepseekauth.NewTokenSource()
			return modelConfig, nil
		case "claude":
			if !claudeauth.SignedIn() {
				return llm.ProviderConfig{}, fmt.Errorf("Claude account is not signed in: sign in from the provider menu first")
			}
			modelConfig.BaseURL = llm.ClaudeBaseURL
			modelConfig.MessagesAuthStyle = llm.ClaudeMessagesAuth
			// Served by the Messages API, so it needs the Anthropic transport
			// rather than chat completions.
			modelConfig.AnthropicNative = true
			modelConfig.AnthropicTokens = claudeauth.NewTokenSource()
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
	registry := tool.DefaultRegistryForUser(user, skillStore)
	attachSubagents(registry, cfg, user, provider, resolveModel, skillStore, skillContext, evaluationStore, store)
	// The CLI has a provider too, so a media view works the same way there.
	// Rule scoping is opt-in and off by default. Without credentials the
	// deterministic pass alone runs, which scopes every rule that names a real
	// path - no model, no network. Adding credentials asks a model for the rest
	// and the filesystem still decides what survives.
	instructionProposer, _ := internal.ProposeScopes(modelConfig)

	runner := coordinator.New(agentRunnerConfig(agentRunDeps{
		cfg:                 cfg,
		provider:            provider,
		modelConfig:         modelConfig,
		resolveModel:        resolveModel,
		instructionProposer: instructionProposer,
		skillContext:        skillContext,
		evaluationStore:     evaluationStore,
		compactor:           compactor,
	}), store, registry)
	_, err = runner.Run(ctx, user)
	return err
}

// attachSubagents wires the CLI's task tool to a factory built from this run's
// provider, project and stores. The registry builds the task tool inert, so
// without this every task call fails with "no factory is attached": a tool the
// model can see and use that cannot do anything. It is a helper rather than
// inline so the wiring can be asserted without driving a whole run.
func attachSubagents(registry *tool.Registry, cfg *config.Config, user string, provider config.LLMProvider,
	resolveModel func(context.Context) (llm.ProviderConfig, error),
	skillStore *skills.Store, skillContext string,
	evaluationStore *evaluation.Store, store *sessionstore.Store) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	subagent.Attach(registry, &subagent.Factory{
		ParentSession:   agentRunID,
		ProjectRoot:     cwd,
		Reasoning:       llm.ReasonLevel(strings.TrimSpace(provider.Reasoning)),
		ProviderID:      cfg.EffectiveProviderID(),
		ProviderCfg:     provider,
		ResolveModel:    resolveModel,
		ParentPrompt:    prompt.System,
		SkillStore:      skillStore,
		SkillContext:    skillContext,
		EvaluationStore: evaluationStore,
		Store:           store,
		WallClock:       operation.TaskTimeout,
		NewRegistry:     tool.DefaultRegistryForUser,
		Cfg:             cfg,
		User:            user,
	})
}

// agentRunDeps is what one CLI run's RunnerConfig is built from.
//
// A struct rather than a parameter list because the literal reads nearly all of
// them, and a signature that long invites a transposed argument, which would
// wire the wrong store into the runner with nothing failing to compile.
type agentRunDeps struct {
	cfg                 *config.Config
	provider            config.LLMProvider
	modelConfig         llm.ProviderConfig
	resolveModel        func(context.Context) (llm.ProviderConfig, error)
	instructionProposer instruction.Proposer
	skillContext        string
	evaluationStore     *evaluation.Store
	compactor           *compaction.Compactor
}

// agentRunnerConfig builds the RunnerConfig for one CLI run.
func agentRunnerConfig(d agentRunDeps) coordinator.RunnerConfig {
	return coordinator.RunnerConfig{
		SessionID:           "cli",
		SystemPrompt:        prompt.System,
		Model:               d.modelConfig,
		ResolveModel:        d.resolveModel,
		ContextLimit:        d.provider.ContextLimit,
		MaxOutputTokens:     d.provider.MaxOutputTokens,
		ToolOutputDir:       d.cfg.ToolOutputDir,
		MaxToolResultChars:  d.cfg.MaxToolResultChars,
		MaxConcurrentTools:  d.cfg.MaxConcurrentTools,
		GoalDir:             d.cfg.SessionFile,
		ModelCatalogURL:     d.cfg.ModelCatalogURL,
		ModelCatalogCache:   d.cfg.ModelCatalogCache,
		ModelCatalogRefresh: time.Duration(d.cfg.ModelCatalogRefreshHours) * time.Hour,
		// The CLI runs in the process working directory: this config sets no
		// WorkDir, so the runner resolves relative paths against it, and the
		// instruction store has to resolve against the same place or a project's
		// AGENTS.md is read from somewhere the commands do not run.
		InstructionRules: internal.InstructionRules(".", d.instructionProposer),
		SkillContext:     d.skillContext,
		EvaluationStore:  d.evaluationStore,
		Reasoning:        llm.ReasonLevel(strings.TrimSpace(d.provider.Reasoning)),
		ReasoningEfforts: d.provider.ReasoningEfforts,
		ReasoningDefault: d.provider.ReasoningDefault,
		Compactor:        d.compactor,
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
	}
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

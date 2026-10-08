package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ceraa/harness/compaction"
	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/permission"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/tool"
	"ceraa/internal/claudeauth"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/deepseekauth"
	"ceraa/internal/hermesauth"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
	"ceraa/internal/prompt"
	"ceraa/internal/secret"
	"ceraa/internal/xaiauth"
)

// Session is one interactive conversation on the terminal.
//
// It owns the stores, the tool registry and the runner, and it is built once and
// reused for every turn. That is the whole difference from the one-shot path:
// rebuilding per turn is fine for a single prompt and wrong for a REPL, because a
// session that is rebuilt each turn is a session that forgets.
type Session struct {
	cfg     *config.Config
	store   *sessionstore.Store
	runner  *coordinator.Runner
	render  *Renderer
	perms   *permission.Manager
	session string
	workDir string
	// readLine reads one line, used by the REPL loop and by the permission
	// prompt so both go through one path.
	readLine func(string) (string, error)

	// resolver is kept so a slash command can rebuild the runner against a
	// different provider without reloading the whole config.
	resolver func() (llm.ProviderConfig, config.LLMProvider, error)
	registry func() (*tool.Registry, error)
}

// SessionOptions configures a Session.
type SessionOptions struct {
	// Config is the resolved configuration. Required.
	Config *config.Config
	// Renderer renders the run. Required.
	Renderer *Renderer
	// SessionID is the conversation id. Empty starts a fresh one.
	SessionID string
	// ReadLine reads one line of input, used for the interactive loop and for
	// permission prompts. It is a field so both go through one path and a test can
	// drive either without a terminal.
	ReadLine func(prompt string) (string, error)
}

// NewSession builds a session, opening the credential store and the file-backed
// stores the runner needs.
func NewSession(opts SessionOptions) (*Session, error) {
	cfg := opts.Config
	if cfg == nil {
		return nil, fmt.Errorf("tui: nil config")
	}
	if opts.Renderer == nil {
		return nil, fmt.Errorf("tui: nil renderer")
	}
	// Opened here, before anything can need a credential, for the same reason the
	// Telegram daemon does it: a subscription provider resolves its provider fine
	// and then fails with no credential, which reads as a broken login rather than
	// an uninitialised store.
	if err := secret.Init(filepath.Join(paths.DataDir(), "secret.key"), filepath.Join(paths.DataDir(), "secrets.json")); err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
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
		return nil, fmt.Errorf("session store: %w", err)
	}
	sid := opts.SessionID
	if sid == "" {
		sid = newSessionID()
	}
	s := &Session{
		cfg:     cfg,
		store:   store,
		render:  opts.Renderer,
		perms:   permission.NewManager(),
		session: sid,
		workDir: cfg.WorkspaceRootPath(),
	}
	s.readLine = opts.ReadLine
	if err := s.build(); err != nil {
		return nil, err
	}
	return s, nil
}

// readLineFn returns the input hook, defaulting to stdin.
func (s *Session) readLineFn() func(string) (string, error) {
	if s.readLine != nil {
		return s.readLine
	}
	return defaultReadLine
}

// build assembles the registry and runner for the current provider.
//
// It is called once at construction and again whenever the provider changes, so
// that a model switch takes effect on the next turn rather than the next process.
func (s *Session) build() error {
	provider := s.cfg.ActiveLLM()
	modelConfig := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	modelConfig.PromptCacheMode = provider.PromptCacheMode
	modelConfig.PromptCacheTTL = provider.PromptCacheTTL
	modelConfig.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(provider.Model, provider.URL)

	compactor := compaction.New(modelConfig)
	compactor.ContextLimit = provider.ContextLimit
	compactor.MaxOutputTokens = provider.MaxOutputTokens
	compactor.CacheKey = "ceraa-compaction-cli-" + s.session
	compactor.ArchiveDir = s.cfg.SessionFile

	resolveModel := func(ctx context.Context) (llm.ProviderConfig, error) {
		return resolveProviderModel(ctx, provider)
	}
	compactor.ResolveModel = resolveModel
	s.resolver = func() (llm.ProviderConfig, config.LLMProvider, error) {
		return modelConfig, provider, nil
	}

	memoryStore, err := memory.New(s.cfg.MemoryFile)
	if err != nil {
		return fmt.Errorf("memory store: %w", err)
	}
	skillStore, err := skills.New(s.cfg.SkillFile)
	if err != nil {
		return fmt.Errorf("skill store: %w", err)
	}
	evaluationStore, err := newEvaluationStore(s.cfg)
	if learn, enabled := memory.MakeLearner(modelConfig, memoryStore, "cli"); enabled {
		compactor.Learn = learn
	}
	if err != nil {
		return err
	}
	// A snapshot for the first render and a live reader for every turn after it,
	// so an edit to memory/AGENTS.md reaches the model without a restart.
	memoryContext, _ := memoryStore.RenderContext("cli", s.session, 2000)
	liveMemory := memory.Live(memoryStore, "cli", s.session, filepath.Join(s.workDir, memory.DefaultMemoryFile), 4000)
	skillContext, _ := skillStore.CatalogContext(2000)

	registry := tool.DefaultRegistryForUser("cli", memoryStore, skillStore)
	// The CLI has a provider too, so a media call works the same way it does in
	// Telegram.
	attachVision(registry, resolveModel)
	// Kept so the banner can count what a turn would actually be offered. The
	// field existed and was never assigned, so the count came back empty and the
	// row was left out.
	s.registry = func() (*tool.Registry, error) { return registry, nil }

	s.runner = coordinator.New(coordinator.RunnerConfig{
		SessionID:           s.session,
		SystemPrompt:        prompt.System,
		Model:               modelConfig,
		ResolveModel:        resolveModel,
		ContextLimit:        provider.ContextLimit,
		MaxOutputTokens:     provider.MaxOutputTokens,
		WorkDir:             s.workDir,
		ToolOutputDir:       s.cfg.ToolOutputDir,
		MaxToolResultChars:  s.cfg.MaxToolResultChars,
		MaxConcurrentTools:  s.cfg.MaxConcurrentTools,
		GoalDir:             s.cfg.SessionFile,
		ModelCatalogURL:     s.cfg.ModelCatalogURL,
		ModelCatalogCache:   s.cfg.ModelCatalogCache,
		ModelCatalogRefresh: time.Duration(s.cfg.ModelCatalogRefreshHours) * time.Hour,
		MemoryContext:       memoryContext,
		MemoryContextFunc:   memory.Combine(memory.LiveBond(paths.BondFile(), 2000), liveMemory),
		MemoryDelivery:      s.cfg.MemoryDelivery,
		MemoryEntries:       memoryEntries(memoryStore, s.session),
		SkillContext:        skillContext,
		EvaluationStore:     evaluationStore,
		Compactor:           compactor,
		Callbacks:           s.callbacks(),
		NeedsAuthorization:  s.needsAuthorization,
		AuthorizeTool:       s.authorizeTool,
	}, s.store, registry)
	return nil
}

// callbacks wires the coordinator to the renderer.
//
// Each callback is a one-line forward: the renderer owns presentation, so a
// change to what a run looks like never reaches the coordinator.
func (s *Session) callbacks() coordinator.Callbacks {
	return coordinator.Callbacks{
		OnContent:         s.render.Content,
		OnReasoning:       s.render.Reasoning,
		OnToolStart:       func(_, name string, args map[string]any) { s.render.ToolStart(name, args) },
		OnToolDone:        func(_, name string, res operation.ResultEvent) { s.render.ToolDone(name, res) },
		OnRetry:           s.render.Retry,
		OnCompactionStart: func() { s.render.Compaction(false, nil) },
		OnCompactionDone:  s.render.Compaction,
		OnUsage:           s.render.Usage,
		OnCost:            s.render.Cost,
		// Deliberately not wired to render.Error. The runner fires OnError for
		// every error it returns, and the loop reports the same error once the
		// turn is back, so wiring it here printed the message twice - three
		// wrapped rows, then the same three rows again - and turned an interrupt
		// into `x context canceled` followed by `interrupted`. The loop is the
		// single place a failure reaches the screen.
	}
}

// needsAuthorization mirrors the Telegram gate: writes always, and commands that
// are destructive or reach the network.
//
// The list is a substring test on the rendered command, which is crude and
// deliberate. It is the same rule the bot applies, so a tool that asks the user on
// Telegram asks the user here, and a rule that only existed in one surface would
// be a rule the other surface walks around.
func (s *Session) needsAuthorization(name string, args map[string]any) bool {
	switch name {
	case "write", "edit_line", "batch_edit_file", "delete", "move", "copy":
		return true
	case "bash", "run_command_with_secret":
		lower := strings.ToLower(renderArgs(args))
		for _, risky := range riskyCommands {
			if strings.Contains(lower, risky) {
				return true
			}
		}
	}
	return false
}

var riskyCommands = []string{
	"rm ", "rm\t", "unlink ", "shred ", "mv ", "cp ", "truncate ", "tee ",
	"git clean", "git reset --hard", "git checkout --", "git restore",
	"mkfs", "dd if=", "chmod ", "chown ", "kill ", "pkill ", "shutdown",
	"reboot", "systemctl ", "docker ", "kubectl ", "sudo ", "curl ", "wget ",
	"git push", " -delete", ":(){:|:&};:",
}

// renderArgs flattens args for the substring test without pulling in a formatter
// that would print a whole file.
func renderArgs(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok {
		return cmd
	}
	var b strings.Builder
	for k, v := range args {
		if s, ok := v.(string); ok {
			b.WriteString(k)
			b.WriteString(" ")
			if len(s) > 200 {
				s = s[:200]
			}
			b.WriteString(s)
			b.WriteString(" ")
		}
	}
	return b.String()
}

// authorizeTool asks on the terminal.
//
// The permission manager is used rather than a bare bool so "always allow" is
// remembered for the session, and the prompt is answered inside the manager's
// ready hook so the wait and the read cannot disagree about which call is being
// answered.
func (s *Session) authorizeTool(ctx context.Context, callID, name string, args map[string]any) (bool, error) {
	summary := ToolArgsSummary(name, args)
	decision, err := s.perms.Create(ctx, s.session, callID, name, 5*time.Minute, func() {
		s.askPermission(callID, name, summary)
	})
	if err != nil {
		return false, err
	}
	return decision == "allowOnce" || decision == "allowAlways", nil
}

// askPermission prints the prompt and answers the manager for one call.
//
// The call id arrives as a parameter rather than through a package variable,
// because two tools can ask at once and a shared variable would answer the wrong
// prompt.
func (s *Session) askPermission(callID, name, summary string) {
	r := s.render
	r.write("\n")
	r.line(r.pal.Reverse(" permission ") + " " + r.pal.Bold(ToolLabel(name)) + " " + r.pal.Dim(summary))
	r.write(r.pal.Dim("  allow once [y]  always [a]  deny [n] "))
	answer, err := s.readLineFn()("")
	decision := "deny"
	if err == nil {
		switch a := strings.ToLower(strings.TrimSpace(answer)); {
		case strings.HasPrefix(a, "a"):
			s.perms.AllowAlways(name)
			decision = "allowAlways"
		case strings.HasPrefix(a, "y"):
			decision = "allowOnce"
		}
	}
	s.perms.Answer(s.session, callID, decision)
}

// Run executes one turn.
func (s *Session) Run(ctx context.Context, userText string) error {
	s.render.TurnStart()
	defer func() { s.render.TurnEnd(nil) }()
	_, err := s.runner.Run(ctx, userText)
	return err
}

// RunContext executes one turn and reports its error to the renderer, so a
// failure is visible on the terminal rather than only in the exit status.
func (s *Session) RunContext(ctx context.Context, userText string) error {
	s.render.TurnStart()
	_, err := s.runner.Run(ctx, userText)
	s.render.TurnEnd(err)
	return err
}

// SessionID reports the conversation id.
//
// A nil Session answers empty rather than panicking, because the loop prints the
// id in its prompt and the exit line and must not depend on a session existing to
// be readable.
func (s *Session) SessionID() string {
	if s == nil {
		return ""
	}
	return s.session
}

// Store exposes the session store so slash commands can list conversations.
func (s *Session) Store() *sessionstore.Store {
	if s == nil {
		return nil
	}
	return s.store
}

// WorkDir reports the directory a run's relative paths resolve against.
func (s *Session) WorkDir() string {
	if s == nil {
		return ""
	}
	return s.workDir
}

// Effort reports the configured reasoning level, which is the one setting that
// changes what a turn costs and how long it takes without changing the model.
//
// It is reported from config rather than from what the provider was finally
// sent, because a provider clamps it to what its model accepts and the banner
// that says "high" while the request carried "medium" is the kind of lie that
// costs an afternoon.
func (s *Session) Effort() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return strings.TrimSpace(s.cfg.Reasoning)
}

// Agent reports the selected agent, if one was chosen.
func (s *Session) Agent() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return strings.TrimSpace(s.cfg.Agent)
}

// ToolCount reports how many tools the model may call.
//
// Counted from the registry rather than kept as a number, so it cannot drift
// from what a turn would actually be offered. Zero means the registry could not
// be built, which is not the same as having no tools, so the banner leaves the
// row out rather than claiming an empty set.
func (s *Session) ToolCount() int {
	if s == nil || s.registry == nil {
		return 0
	}
	reg, err := s.registry()
	if err != nil || reg == nil {
		return 0
	}
	return len(reg.All())
}

// providerHost reduces a base URL to its host, so the banner names a provider
// rather than printing a URL at it.
//
// The URL is what the config holds, but a provider row reading
// "https://api.example.com/v1" is the longest string in the block and pushes the
// value column off a phone. The host is the part that identifies it.
func providerHost(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	return parsed.Host
}

// Model reports the active provider and model for the banner and the footer.
func (s *Session) Model() (providerName, modelName string) {
	if s == nil || s.cfg == nil {
		return "", ""
	}
	p := s.cfg.ActiveLLM()
	// EffectiveProviderID answers "where did this come from" and returns default
	// or config when no profile is named. Neither is a provider name, and
	// printing one read like a provider called default, so an install with
	// nothing configured looked configured. Those two also meant the fallbacks
	// below them were unreachable, so nothing named the provider at all.
	switch name := s.cfg.EffectiveProviderID(); name {
	case "default", "config":
		switch {
		case p.Subscription != "":
			providerName = p.Subscription
		case p.URL != "":
			providerName = providerHost(p.URL)
		}
	default:
		providerName = name
	}
	return providerName, p.Model
}

// resolveProviderModel picks the transport for a subscription profile.
//
// Every branch is chosen in one place. A profile is a name and a URL, and the
// transport that serves it is not derivable from either, so the mapping lives
// here rather than being re-guessed at each call site.
func resolveProviderModel(ctx context.Context, provider config.LLMProvider) (llm.ProviderConfig, error) {
	modelConfig := llm.FromProvider(provider.URL, provider.APIKey, provider.Model)
	modelConfig.PromptCacheMode = provider.PromptCacheMode
	modelConfig.PromptCacheTTL = provider.PromptCacheTTL
	modelConfig.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(provider.Model, provider.URL)
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
		return hermesauth.ApplyCredential(modelConfig, credential, store)
	case "cline":
		credential, err := clineauth.Resolve(ctx, nil)
		if err != nil {
			return llm.ProviderConfig{}, err
		}
		modelConfig.BaseURL = firstNonEmpty(provider.URL, llm.ClineGatewayBase)
		modelConfig.APIKey = credential.APIKey()
		if len(modelConfig.ExtraHeaders) == 0 || !clineauth.HasClineHeaders(modelConfig.ExtraHeaders) {
			modelConfig.ExtraHeaders = clineauth.RequestHeaders("cli")
		}
		return modelConfig, nil
	case "opencode":
		credential, err := opencodeauth.CredentialFor(ctx, provider.APIKey)
		if err != nil {
			return llm.ProviderConfig{}, err
		}
		// Zen is OpenAI-compatible, so it needs no transport of its own. The
		// header contract is the only thing a plain config would be missing,
		// which is what answers 401 without it.
		return opencodeauth.ApplyCredential(modelConfig, credential, ""), nil
	case "minimax":
		credential, err := minimaxauth.Resolve(ctx, minimaxauth.NewStore(regionFor(provider)))
		if err != nil {
			return llm.ProviderConfig{}, err
		}
		modelConfig.BaseURL = minimaxauth.BaseURLFor(credential.Region)
		modelConfig.MessagesAuthStyle = minimaxauth.MessagesAuthFor(credential.Region)
		modelConfig.APIKey = credential.AccessToken
		modelConfig.AnthropicNative = true
		modelConfig.AnthropicTokens = minimaxauth.NewTokenSource(credential.Region)
		return modelConfig, nil
	case "deepseek":
		if !deepseekauth.SignedIn() {
			return llm.ProviderConfig{}, fmt.Errorf("DeepSeek account is not signed in: sign in from the provider menu first")
		}
		modelConfig.BaseURL = llm.DeepSeekBaseURL
		modelConfig.MessagesAuthStyle = llm.DeepSeekMessagesAuth
		modelConfig.AnthropicNative = true
		modelConfig.AnthropicTokens = deepseekauth.NewTokenSource()
		return modelConfig, nil
	case "claude":
		if !claudeauth.SignedIn() {
			return llm.ProviderConfig{}, fmt.Errorf("Claude account is not signed in: sign in from the provider menu first")
		}
		modelConfig.BaseURL = llm.ClaudeBaseURL
		modelConfig.MessagesAuthStyle = llm.ClaudeMessagesAuth
		modelConfig.AnthropicNative = true
		modelConfig.AnthropicTokens = claudeauth.NewTokenSource()
		return modelConfig, nil
	case "xai":
		credential, err := xaiauth.Resolve(ctx, nil)
		if err != nil {
			return llm.ProviderConfig{}, err
		}
		modelConfig.BaseURL = firstNonEmpty(credential.BaseURL, llm.XaiBaseURL)
		modelConfig.APIKey = credential.AccessToken
		modelConfig.XaiNative = true
		modelConfig.XaiTokens = xaiauth.NewTokenSource()
		return modelConfig, nil
	default:
		if llm.UsesAnthropicMessages(modelConfig.Model, modelConfig.BaseURL) {
			modelConfig.AnthropicNative = true
		}
		return modelConfig, nil
	}
}

// regionFor reads the MiniMax region from a profile, defaulting to global.
func regionFor(provider config.LLMProvider) minimaxauth.Region {
	if strings.EqualFold(strings.TrimSpace(provider.URL), llm.MiniMaxCNBaseURL) {
		return minimaxauth.RegionCN
	}
	return minimaxauth.RegionGlobal
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

func memoryEntries(store *memory.Store, sessionID string) func() ([]memory.Entry, error) {
	if store == nil {
		return nil
	}
	return func() ([]memory.Entry, error) { return store.List("cli", sessionID) }
}

// newSessionID names a conversation.
//
// A timestamp is enough and is honest about what it is. A random id would look
// like a session key and would be mistaken for one in a directory listing.
func newSessionID() string {
	return fmt.Sprintf("cli-%d", time.Now().Unix())
}

// defaultReadLine reads one line from stdin, used when the caller supplies none.
func defaultReadLine(p string) (string, error) {
	if p != "" {
		fmt.Fprint(os.Stderr, p)
	}
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return "", err
	}
	return line, nil
}

// attachVision gives view_image the run's resolved model outside Telegram too,
// so the CLI sees the same behaviour rather than a stub.
func attachVision(registry *tool.Registry, resolve func(ctx context.Context) (llm.ProviderConfig, error)) {
	vision := &cliVision{resolve: resolve}
	llm.SetMediaSupportHostResolver(func(string) string { return "" })
	if existing, ok := registry.Get("view_image"); ok {
		if t, ok := existing.(*tool.ViewImageTool); ok {
			t.Client = vision
		}
	}
}

type cliVision struct {
	resolve func(ctx context.Context) (llm.ProviderConfig, error)
}

func (c *cliVision) config(ctx context.Context) (llm.ProviderConfig, error) {
	if c.resolve == nil {
		return llm.ProviderConfig{}, errNoProvider
	}
	return c.resolve(ctx)
}

func (c *cliVision) ModelName() string {
	cfg, err := c.config(context.Background())
	if err != nil {
		return ""
	}
	return cfg.Model
}

func (c *cliVision) MediaHost() string { return "" }

func (c *cliVision) DescribeMedia(ctx context.Context, media []*llm.MediaAttachment, prompt string) (string, error) {
	cfg, err := c.config(ctx)
	if err != nil {
		return "", err
	}
	// A media call is its own short request and must not join the session cache.
	cfg.PromptCacheMode = "off"
	return llm.New(cfg).DescribeMedia(ctx, media, prompt)
}

var errNoProvider = errors.New("no provider is configured, so there is no model to look at the image with")

func newEvaluationStore(cfg *config.Config) (*evaluation.Store, error) {
	store, err := evaluation.New(cfg.EvaluationFile)
	if err != nil {
		return nil, fmt.Errorf("evaluation store: %w", err)
	}
	return store, nil
}

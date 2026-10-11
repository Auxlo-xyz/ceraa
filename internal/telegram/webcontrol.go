package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ceraa/harness/coordinator"
	"ceraa/harness/evaluation"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/permission"
	"ceraa/harness/scheduler"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/snapshot"
	"ceraa/harness/todo"
	"ceraa/harness/tool"
	"ceraa/internal"
	"ceraa/internal/build"
	"ceraa/internal/claudeauth"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/deepseekauth"
	"ceraa/internal/envs"
	"ceraa/internal/hermesauth"
	"ceraa/internal/miniapp"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
	"ceraa/internal/prompt"
	"ceraa/internal/secret"
	"ceraa/internal/xaiauth"
)

// Control wiring: the dashboard's backend, injected into the Mini App server.
//
// Each function here reads the stores the bot already owns and returns only
// what the page may show. Credentials never appear: profiles carry a has-key
// flag, secrets carry lengths, and the values stay in the store. The shapes
// are miniapp's structs, so this package fills them and the page package
// stays free of harness imports.

// webControl assigns the dashboard's backend into the Mini App server config.
// One line per concern, each naming the function that owns it, so a missing
// wire is visible here rather than as a 503 discovered from the page.
func webControl(cfg *miniapp.Config, tunnelAddr func() string) {
	cfg.Meta = webMeta
	cfg.Providers = webProviders
	cfg.SetActiveProvider = webSetActiveProvider
	cfg.SaveProvider = webSaveProvider
	cfg.DeleteProvider = webDeleteProvider
	cfg.Subscriptions = webSubscriptions
	cfg.Catalog = webCatalog
	cfg.RefreshCatalog = webRefreshCatalog
	cfg.RunParams = webRunParams
	cfg.SetRunParams = webSetRunParams
	cfg.Tools = webTools
	cfg.SetToolAllowed = webSetToolAllowed
	cfg.Secrets = webSecretLengths
	cfg.Memories = webMemories
	cfg.DeleteMemory = webDeleteMemory
	cfg.TriggerCompaction = webTriggerCompaction
	cfg.Skills = webSkills
	cfg.DecideSkill = webDecideSkill
	cfg.Schedules = webSchedules
	cfg.AddSchedule = webAddSchedule
	cfg.DeleteSchedule = webDeleteSchedule
	cfg.Stats = webStats
	cfg.Files = webFiles
	cfg.Status = webStatus(tunnelAddr)
	cfg.Snapshots = webSnapshots
	cfg.Todos = webTodos
	cfg.SetTodo = webSetTodo
	cfg.RenameSession = webRenameSession
	cfg.DeleteSession = webDeleteSession
	cfg.Projects = webProjects
	cfg.SetProject = webSetProject
	cfg.StopActiveRun = stopActiveRun
}

func webMeta() map[string]any {
	levels := []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	// The levels mirror llm's ReasonLevel constants. A level renamed there
	// must be renamed here too, which is why this list is built beside the
	// validation it has to agree with rather than quoted in the page.
	for _, level := range levels {
		if !llm.ReasonLevel(level).Valid() {
			levels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
			break
		}
	}
	halfLives := memory.TypeHalfLives()
	types := make([]map[string]any, 0, len(halfLives))
	for _, entry := range halfLives {
		types = append(types, map[string]any{
			"type":           string(entry.Type),
			"half_life_days": entry.HalfLife,
		})
	}
	registry := tool.DefaultRegistryWithStores(nil)
	inventory := registry.All()
	toolIDs := make([]string, 0, len(inventory))
	for _, t := range inventory {
		toolIDs = append(toolIDs, t.Name())
	}
	return map[string]any{
		"reasoning_levels":  levels,
		"memory_types":      types,
		"tool_ids":          toolIDs,
		"reserved_prefixes": secret.ReservedPrefixes(),
		"source_tree":       "/workspace/development/ceraa",
	}
}

func webProviders() (string, []miniapp.ProviderEntry, error) {
	active := defaultState.provider()
	profiles := providers.profiles()
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, _ := config.Load()
	var catalog *llm.CatalogClient
	if cfg != nil {
		catalog = llm.NewCatalogClient(cfg.ModelCatalogURL, cfg.ModelCatalogCache, time.Duration(cfg.ModelCatalogRefreshHours)*time.Hour)
	}
	// The default provider is top-level config, not a stored profile, and most
	// installs run on exactly it. A list without it is empty for those installs,
	// and an empty provider picker reads as a broken dashboard rather than an
	// account with one provider. It leads because it is the common case.
	list := []miniapp.ProviderEntry{}
	if cfg != nil {
		def := miniapp.ProviderEntry{
			ID:           "default",
			Endpoint:     cfg.LLM.URL,
			Model:        cfg.LLM.Model,
			Reasoning:    cfg.LLM.Reasoning,
			ContextLimit: cfg.LLM.ContextLimit,
			OutputCap:    cfg.LLM.MaxOutputTokens,
			CacheMode:    cfg.LLM.PromptCacheMode,
			HasKey:       strings.TrimSpace(cfg.LLM.APIKey) != "",
		}
		if catalog != nil && strings.TrimSpace(cfg.LLM.Model) != "" {
			if metadata, err := catalog.Lookup(ctx, cfg.LLM.Model); err == nil {
				def.PricingUsable = metadata.PricingUsable()
			}
		}
		list = append(list, def)
	}
	for _, name := range names {
		profile := profiles[name]
		entry := miniapp.ProviderEntry{
			ID:           name,
			Endpoint:     profile.URL,
			Model:        profile.Model,
			Reasoning:    profile.Reasoning,
			ContextLimit: profile.ContextLimit,
			OutputCap:    profile.MaxOutputTokens,
			CacheMode:    profile.PromptCacheMode,
			HasKey:       strings.TrimSpace(profile.APIKey) != "",
		}
		// Pricing comes from the catalog when it knows the model, and the
		// entry reports unusable otherwise. A lookup that fails for any
		// reason, including the network being gone, is unusable rather than
		// an error, because one unknown model must not hide the rest.
		if catalog != nil && strings.TrimSpace(profile.Model) != "" {
			if metadata, err := catalog.Lookup(ctx, profile.Model); err == nil {
				entry.PricingUsable = metadata.PricingUsable()
			}
		}
		list = append(list, entry)
	}
	return active, list, nil
}

func webSetActiveProvider(id string) error {
	return defaultState.setProvider(id)
}

// webSaveProvider creates or replaces a stored profile. The map arrives from
// JSON, so every field is validated before anything is written: one bad field
// must not store a profile the runner then chokes on.
func webSaveProvider(id string, profile map[string]any) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("provider name is empty")
	}
	// Default is top-level config, not a stored profile. Writing a profile
	// under that name would shadow it in some lookups and not others, which
	// is how a provider gets two definitions that disagree.
	if id == "default" {
		return fmt.Errorf("the default provider is edited through run parameters, not profiles")
	}
	// Start from what is already stored, not from a zero value. The form carries
	// six of the ten profile fields, so building the replacement from the body
	// alone silently reset the four the page cannot see: the cache TTL, the
	// subscription it draws its key from, and the Hermes provider. Editing a
	// model in the dashboard used to unbind those. Only fields the body
	// actually carries are overwritten below.
	stored := config.ProviderProfile{}
	if existing, ok := providers.profiles()[id]; ok {
		stored = existing
	}
	if raw, ok := profile["endpoint"].(string); ok {
		stored.URL = strings.TrimSpace(raw)
	}
	if stored.URL == "" {
		return fmt.Errorf("endpoint is required")
	}
	if raw, ok := profile["model"].(string); ok {
		stored.Model = strings.TrimSpace(raw)
	}
	if stored.Model == "" {
		return fmt.Errorf("model is required")
	}
	if raw, ok := profile["api_key"].(string); ok && strings.TrimSpace(raw) != "" {
		// A key arrives only when the user typed a new one. An empty value
		// keeps the stored key, so editing a profile's model does not blank
		// its credential by accident.
		stored.APIKey = raw
	} else if existing, ok := providers.profiles()[id]; ok {
		stored.APIKey = existing.APIKey
	}
	if raw, ok := profile["reasoning"].(string); ok && strings.TrimSpace(raw) != "" {
		if !llm.ReasonLevel(strings.TrimSpace(raw)).Valid() {
			return fmt.Errorf("unknown reasoning level %q", raw)
		}
		stored.Reasoning = strings.TrimSpace(raw)
	}
	if raw, ok := profile["context_limit"].(float64); ok {
		if raw < 0 {
			return fmt.Errorf("context limit cannot be negative")
		}
		stored.ContextLimit = int(raw)
	}
	if raw, ok := profile["output_cap"].(float64); ok {
		if raw < 0 {
			return fmt.Errorf("output cap cannot be negative")
		}
		stored.MaxOutputTokens = int(raw)
	}
	if raw, ok := profile["cache_mode"].(string); ok {
		stored.PromptCacheMode = strings.TrimSpace(raw)
	}
	return defaultState.setProviderProfile(id, stored)
}

func webDeleteProvider(id string) error {
	removed, err := defaultState.removeProviderProfile(id)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no provider named %q", id)
	}
	return nil
}

func webSubscriptions() ([]miniapp.SubscriptionEntry, error) {
	active := ""
	if selection, err := config.LoadSelection(); err == nil {
		active = selection.ActiveProvider
	}
	entries := []miniapp.SubscriptionEntry{
		{ID: "codex", Label: "Codex", SignedIn: codexauth.Authenticated(), Active: active == llm.CodexSubscription},
		{ID: "hermes", Label: "Hermes", SignedIn: hermesauth.SignedIn(), Active: active == llm.HermesSubscription},
		{ID: "xai", Label: "xAI", SignedIn: xaiauth.SignedIn(), Active: active == llm.XAISubscription},
		{ID: "minimax", Label: "MiniMax", SignedIn: minimaxauth.SignedIn(minimaxauth.RegionGlobal), Active: active == llm.MiniMaxSubscription},
		{ID: "cline", Label: "Cline", SignedIn: clineauth.SignedIn(), Active: active == llm.ClineSubscription},
		{ID: "opencode", Label: "OpenCode Zen", SignedIn: opencodeauth.SignedIn(), Active: active == llm.OpenCodeSubscription},
		{ID: "deepseek", Label: "DeepSeek", SignedIn: deepseekauth.SignedIn(), Active: active == llm.DeepSeekSubscription},
		{ID: "claude", Label: "Claude", SignedIn: claudeauth.SignedIn(), Active: active == llm.ClaudeSubscription},
	}
	return entries, nil
}

func webCatalog() (map[string]any, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return catalogSnapshot(cfg), nil
}

func catalogSnapshot(cfg *config.Config) map[string]any {
	out := map[string]any{
		"source_url":    cfg.ModelCatalogURL,
		"refresh_hours": cfg.ModelCatalogRefreshHours,
		"last_refresh":  "never",
		"models":        -1,
	}
	path := cfg.ModelCatalogCache
	if path == "" {
		path = llm.DefaultModelCatalogCachePath()
	}
	if info, err := os.Stat(path); err == nil {
		out["last_refresh"] = info.ModTime().UTC().Format(time.RFC3339)
	}
	return out
}

func webRefreshCatalog() (map[string]any, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	client := llm.NewCatalogClient(cfg.ModelCatalogURL, cfg.ModelCatalogCache, time.Duration(cfg.ModelCatalogRefreshHours)*time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	models, err := client.List(ctx)
	out := catalogSnapshot(cfg)
	out["models"] = len(models)
	out["refreshed"] = err == nil
	if err != nil {
		// A failed refresh still reports the stale cache rather than nothing:
		// the dashboard shows what the runner will actually use.
		out["refresh_error"] = err.Error()
	}
	return out, nil
}

func webRunParams() map[string]any {
	cfg, _ := config.Load()
	model, agent := defaultState.values()
	auto, configured := defaultState.autoCompaction()
	if !configured {
		auto = true
	}
	out := map[string]any{
		"reasoning":       defaultState.reasoning(),
		"agent":           agent,
		"model":           model,
		"auto_compaction": auto,
	}
	if cfg != nil {
		// context_limit is reported raw so the row stays editable, but it is the
		// *base* block and it is deliberately unset by default - config.DefaultConfig
		// leaves it zero so the runner asks the model catalog. Reading that raw value
		// reported a window the agent never uses, and dividing it by a thousand
		// printed "0k tok".
		//
		// It also read the wrong block entirely when a provider profile is active.
		// mergeProfile folds the profile's limit onto the base, and ActiveLLM is
		// that merged view, so the limit in force is 200k from the profile while
		// cfg.LLM.ContextLimit alone still says zero. Resolving against the base
		// would have reported the fallback for a provider that states 200k, which
		// is a different kind of wrong: a plausible number rather than an obvious
		// one. Reusing llm.ResolveContextWindow keeps the dashboard on the runner's
		// resolution rule rather than a second one that can disagree with it.
		//
		// The model's advertised window is passed so this resolves the same number
		// the runner budgets against, including a downward correction. The catalog
		// can only lower a limit, so a missing or stale catalog leaves the
		// configured value in force rather than changing it. The advertised figure
		// travels alongside so the row can say what the model could take.
		active := cfg.ActiveLLM()
		effective, source, advertised := resolveContextLimitForDisplay(cfg)
		out["context_limit"] = cfg.LLM.ContextLimit
		out["context_limit_effective"] = effective
		out["context_limit_source"] = string(source)
		out["context_limit_advertised"] = advertised
		out["max_output_tokens"] = active.MaxOutputTokens
		out["max_tool_result_chars"] = cfg.MaxToolResultChars
		out["max_concurrent_tools"] = cfg.MaxConcurrentTools
		out["prompt_cache_mode"] = cfg.LLM.PromptCacheMode
		out["prompt_cache_ttl"] = cfg.LLM.PromptCacheTTL
		out["memory_delivery"] = cfg.MemoryDelivery
	}
	return out
}

// webSetRunParams applies run parameter changes. Reasoning, agent and model
// travel the menu's own setters so validation stays in one place; the numeric
// runner tunables persist through the config store with bounds checking here,
// because a negative context limit must be refused before it is saved rather
// than discovered when the next run misbehaves.
func webSetRunParams(update map[string]any) error {
	intKeys := map[string]bool{
		"context_limit": true, "max_output_tokens": true,
		"max_tool_result_chars": true, "max_concurrent_tools": true,
	}
	stringKeys := map[string]bool{
		"prompt_cache_mode": true, "prompt_cache_ttl": true, "memory_delivery": true,
	}
	for key, raw := range update {
		switch {
		case key == "reasoning" || key == "agent" || key == "model":
			if _, ok := raw.(string); !ok {
				return fmt.Errorf("setting %q must be a string", key)
			}
		case intKeys[key]:
			number, ok := raw.(float64)
			if !ok || number < 0 || number != float64(int(number)) {
				return fmt.Errorf("setting %q must be a non-negative integer", key)
			}
		case stringKeys[key]:
			if _, ok := raw.(string); !ok {
				return fmt.Errorf("setting %q must be a string", key)
			}
		default:
			return fmt.Errorf("unknown run parameter %q", key)
		}
	}
	if raw, ok := update["memory_delivery"].(string); ok {
		switch strings.TrimSpace(raw) {
		case "", "cached", "prompt", "sidekick":
		default:
			return fmt.Errorf("unknown memory delivery %q", raw)
		}
	}
	for key, raw := range update {
		var err error
		switch key {
		case "reasoning":
			err = defaultState.setReasoning(raw.(string))
		case "agent":
			err = defaultState.setAgent(raw.(string))
		case "model":
			err = defaultState.setModel(raw.(string))
		}
		if err != nil {
			return err
		}
	}
	numeric := map[string]any{}
	for key := range intKeys {
		if raw, ok := update[key]; ok {
			numeric[key] = int(raw.(float64))
		}
	}
	stringsSet := map[string]string{}
	for key := range stringKeys {
		if raw, ok := update[key].(string); ok {
			stringsSet[key] = strings.TrimSpace(raw)
		}
	}
	if len(numeric) == 0 && len(stringsSet) == 0 {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for key, raw := range numeric {
		switch key {
		case "context_limit":
			cfg.LLM.ContextLimit = raw.(int)
		case "max_output_tokens":
			cfg.LLM.MaxOutputTokens = raw.(int)
		case "max_tool_result_chars":
			cfg.MaxToolResultChars = raw.(int)
		case "max_concurrent_tools":
			if raw.(int) == 0 {
				return fmt.Errorf("max concurrent tools must be at least 1")
			}
			cfg.MaxConcurrentTools = raw.(int)
		}
	}
	for key, raw := range stringsSet {
		switch key {
		case "prompt_cache_mode":
			cfg.LLM.PromptCacheMode = raw
		case "prompt_cache_ttl":
			cfg.LLM.PromptCacheTTL = raw
		case "memory_delivery":
			cfg.MemoryDelivery = raw
		}
	}
	// Load, mutate, save under the store's own lock discipline. The menu
	// setters persist the same way through the provider state; the numeric
	// tunables have no menu setter, so this is their only write path and it
	// must not invent a second one.
	return config.Save(cfg)
}

func webTools() ([]string, []miniapp.ToolEntry, error) {
	registry := tool.DefaultRegistryWithStores(nil)
	known := make(map[string]bool)
	inventory := []miniapp.ToolEntry{}
	for _, t := range registry.All() {
		known[t.Name()] = true
		inventory = append(inventory, miniapp.ToolEntry{Name: t.Name(), Description: t.Description()})
	}
	defaultState.mu.RLock()
	allowed := make([]string, 0, len(defaultState.AlwaysAllow))
	for name := range defaultState.AlwaysAllow {
		// A removed tool must not keep a toggle. The entry is dead state from
		// before the tool was deleted, and the menu cannot even untoggle it
		// because webSetToolAllowed refuses unknown names, so it would sit on
		// the page forever. Filtering here is what stops the next tool removal
		// from leaving the same four rows behind.
		if known[name] {
			allowed = append(allowed, name)
		}
	}
	defaultState.mu.RUnlock()
	sort.Strings(allowed)
	return allowed, inventory, nil
}

func webSetToolAllowed(name string, allowed bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tool name is empty")
	}
	registry := tool.DefaultRegistryWithStores(nil)
	if _, ok := registry.Get(name); !ok {
		return fmt.Errorf("no tool named %q", name)
	}
	// The running manager is seeded from state once, at startup (bot.go). A
	// toggle that only persisted would report success and change nothing until
	// the next restart, which is the difference between a permission and a
	// stored intention. The Telegram toggle updates both; this one has to as well.
	if err := defaultState.setAlwaysAllow(name, allowed); err != nil {
		return err
	}
	if allowed {
		permission.Default.AllowAlways(name)
	} else {
		permission.Default.ClearAlways(name)
	}
	return nil
}

// webSecretLengths lists user secrets with lengths, never values. The lengths
// are read here, server-side, one Get per name: the response carries numbers,
// so a leaked response body is not a leaked key.
func webSecretLengths() ([]miniapp.SecretEntry, error) {
	out := []miniapp.SecretEntry{}
	for _, name := range secret.ListUser() {
		length := "?"
		if value, err := secret.Get(name); err == nil {
			length = fmt.Sprintf("%d chars", len([]rune(value)))
		}
		out = append(out, miniapp.SecretEntry{Name: name, Length: length})
	}
	return out, nil
}

func webMemories() ([]miniapp.MemoryEntry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	store, err := memory.New(cfg.MemoryFile)
	if err != nil {
		return nil, err
	}
	entries, err := store.ListAll()
	if err != nil {
		return nil, err
	}
	out := make([]miniapp.MemoryEntry, 0, len(entries))
	for _, entry := range entries {
		veracity := string(entry.Veracity)
		if veracity == "" {
			// Unset provenance reads as unknown everywhere else, and the
			// dashboard must agree rather than show a blank.
			veracity = string(memory.Unknown)
		}
		out = append(out, miniapp.MemoryEntry{
			ID:        entry.ID,
			Type:      string(entry.Type),
			Content:   entry.Content,
			Veracity:  veracity,
			Seen:      entry.Seen,
			Applies:   entry.Applies,
			Project:   entry.ProjectID,
			CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func webDeleteMemory(id string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := memory.New(cfg.MemoryFile)
	if err != nil {
		return err
	}
	return store.Remove(id)
}

// webLiveRequest returns the request a run would send for this session, so the
// Mini App's compaction button compacts in-band by default rather than cold.
//
// It assembles the same parts the bridge hands the runner: the system prompt,
// the skill catalogue, the memory block and the same tool registry. Whatever
// drifts between the two is a place the button's prefix stops matching the
// runs', which costs cache hits but never correctness. If the session has no
// project any more the button cannot build one and reports it: a session whose
// workspace is gone has nothing live to summarise from, and there is no other
// summariser left.
func webLiveRequest(cfg *config.Config, store *sessionstore.Store, sess *sessionstore.Session) (func(context.Context, *sessionstore.Session) llm.StreamRequest, error) {
	projectRoot := defaultState.projectForSession(sess.ID)
	if projectRoot == "" {
		return nil, nil
	}
	providerID := cfg.EffectiveProviderID()
	providerConfig := activeProviderConfig(cfg, providerID)
	modelConfig := llm.FromProvider(providerConfig.URL, providerConfig.APIKey, providerConfig.Model)
	modelConfig.PromptCacheMode = providerConfig.PromptCacheMode
	modelConfig.PromptCacheTTL = providerConfig.PromptCacheTTL
	modelConfig.GeminiThoughtSignatures = llm.UsesGeminiThoughtSignatures(providerConfig.Model, providerConfig.URL)
	memoryStore, err := memory.New(cfg.MemoryFile)
	if err != nil {
		return nil, err
	}
	skillStore, err := skills.New(cfg.SkillFile)
	if err != nil {
		return nil, err
	}
	evaluationStore, err := evaluation.New(cfg.EvaluationFile)
	if err != nil {
		return nil, err
	}
	// Rule scoping is opt-in and off by default. Without credentials the
	// deterministic pass alone runs, which scopes every rule that names a real
	// path - no model, no network. Adding credentials asks a model for the rest
	// and the filesystem still decides what survives.
	instructionProposer, _ := internal.ProposeScopes(modelConfig)

	memoryContext, _ := memoryStore.RenderContext(projectRoot, sess.ID, 2000)
	liveMemory := memory.Live(memoryStore, projectRoot, sess.ID, filepath.Join(projectRoot, memory.DefaultMemoryFile), 4000)
	// One renderer, two homes. The prompt modes read the bond out of the combined
	// block below; sidekick mode suppresses that whole block and reads it from
	// BondFunc instead, which is why it cannot simply be left inside the combine.
	bondRenderer := memory.LiveBond(paths.BondFile(), 2000)
	skillContext, _ := skillStore.CatalogContext(2000)
	// No user text reaches this path, so the skill-feedback heuristic stays off
	// and the proposal tool stays out of the catalogue, which is where it is on
	// most runs. The tool set only has to be sendable, not bit-identical: where
	// it differs the request costs a cache miss, never a wrong answer.
	registry := telegramRegistry(cfg, "", skillStore)
	builder := coordinator.LiveRequestFor(coordinator.RunnerConfig{
		SessionID:         sess.ID,
		SystemPrompt:      prompt.System,
		Model:             modelConfig,
		ContextLimit:      providerConfig.ContextLimit,
		MaxOutputTokens:   providerConfig.MaxOutputTokens,
		WorkDir:           projectRoot,
		MemoryContext:     memoryContext,
		MemoryContextFunc: memory.Combine(bondRenderer, liveMemory),
		BondFunc:          bondRenderer,
		MemoryDelivery:    cfg.MemoryDelivery,
		MemoryEntries:     memoryEntries(memoryStore, projectRoot, sess.ID),
		InstructionRules:  internal.InstructionRules(projectRoot, instructionProposer),
		SkillContext:      skillContext,
		EvaluationStore:   evaluationStore,
	}, registry, store)
	return builder, nil
}

func webTriggerCompaction(sessionID string) (map[string]any, error) {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = currentSessionID(defaultState.allowedUserID())
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	store, err := configuredStore()
	if err != nil {
		return nil, err
	}
	// Load answers an unknown id with an empty session, so "not found" has to be
	// asked about separately. Without this the button reports "nothing to
	// compact" for a session that never existed, which is a wrong answer rather
	// than an honest one.
	if !store.Exists(sessionID) {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", sessionID, err)
	}
	tokens := 0
	for _, message := range sess.Messages {
		tokens += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
	}
	compactor := newCompactor(cfg, cfg.EffectiveProviderID(), sessionID)
	// In-band is the only way the button summarises: it uses the same request
	// a run would send. Without a project there is no request to build, so the
	// button says so instead of summarising from a thinner copy.
	if builder, err := webLiveRequest(cfg, store, sess); err != nil {
		return nil, err
	} else if builder != nil {
		compactor.LiveRequest = builder
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	compacted, err := compactor.Compact(ctx, sess)
	if err != nil {
		return nil, err
	}
	// Compact rewrites the transcript in memory. Persisting it is what makes the
	// button a compaction rather than a preview: without this write the summary
	// is discarded when the request ends and the next run re-reads the full
	// history the operator just paid to compact. The in-run path saves for the
	// same reason (runner.go), so this mirrors it rather than inventing a second
	// rule.
	if compacted {
		if err := store.Save(sess); err != nil {
			return nil, fmt.Errorf("save compacted session %s: %w", sessionID, err)
		}
	}
	return map[string]any{
		"session":         sessionID,
		"compacted":       compacted,
		"tokens_estimate": tokens,
	}, nil
}

func webSkills() ([]miniapp.SkillProposal, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	store, err := skills.New(cfg.SkillFile)
	if err != nil {
		return nil, err
	}
	proposals, err := store.ListProposals()
	if err != nil {
		return nil, err
	}
	out := make([]miniapp.SkillProposal, 0, len(proposals))
	for _, proposal := range proposals {
		// The proposal carries full content and reviewer feedback; the card
		// shows the first line of the content, which is the one line the
		// proposer wrote to be read.
		summary := strings.TrimSpace(proposal.Content)
		if index := strings.Index(summary, "\n"); index >= 0 {
			summary = strings.TrimSpace(summary[:index])
		}
		if runes := []rune(summary); len(runes) > 200 {
			summary = string(runes[:197]) + "..."
		}
		out = append(out, miniapp.SkillProposal{
			ID:      proposal.ID,
			Name:    proposal.SkillName,
			Summary: summary,
			Status:  string(proposal.Status),
		})
	}
	return out, nil
}

func webDecideSkill(id string, approve bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := skills.New(cfg.SkillFile)
	if err != nil {
		return err
	}
	if approve {
		_, err = store.Approve(id)
		return err
	}
	return store.Reject(id)
}

func webSchedules() ([]miniapp.ScheduleJob, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	store, err := scheduler.New(cfg.SchedulerFile)
	if err != nil {
		return nil, err
	}
	jobs, err := store.List()
	if err != nil {
		return nil, err
	}
	out := make([]miniapp.ScheduleJob, 0, len(jobs))
	for _, job := range jobs {
		entry := miniapp.ScheduleJob{
			ID:       job.ID,
			Cron:     job.Cron,
			Task:     job.Prompt,
			Session:  job.SessionID,
			LastStat: string(job.LastStatus),
		}
		if entry.Task == "" {
			entry.Task = job.Command
		}
		if !job.LastRunAt.IsZero() {
			entry.LastRun = job.LastRunAt.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	return out, nil
}

func webAddSchedule(job map[string]any) (string, error) {
	cron, _ := job["cron"].(string)
	task, _ := job["task"].(string)
	if strings.TrimSpace(cron) == "" {
		return "", fmt.Errorf("cron is required")
	}
	if strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("task is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	store, err := scheduler.New(cfg.SchedulerFile)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("job-%d", time.Now().UnixNano())
	entry := scheduler.Job{
		ID:        id,
		Cron:      strings.TrimSpace(cron),
		Prompt:    strings.TrimSpace(task),
		SessionID: currentSessionID(defaultState.allowedUserID()),
		ChatID:    defaultState.allowedUserID(),
	}
	if err := store.Add(entry); err != nil {
		return "", err
	}
	return id, nil
}

func webDeleteSchedule(id string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := scheduler.New(cfg.SchedulerFile)
	if err != nil {
		return err
	}
	removed, err := store.Remove(id)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no job named %q", id)
	}
	return nil
}

// webStats rolls session runs into dashboard numbers. Everything here is a sum
// over what the store already records: token usage and cost per run, tool
// operations per session, failures, and per-endpoint cache behaviour. Nothing
// is estimated except where the store says its own numbers are untrusted, and
// those runs contribute tokens but never cost.
func webStats() (map[string]any, error) {
	store, err := configuredStore()
	if err != nil {
		return nil, err
	}
	ids, err := store.List()
	if err != nil {
		return nil, err
	}
	sessions := []map[string]any{}
	endpoints := map[string]*endpointAggregate{}
	points := []map[string]any{}
	totals := map[string]any{
		"runs": 0, "tool_calls": 0, "errors": 0,
		"input_tokens": 0, "output_tokens": 0,
		"cache_read_tokens": 0, "cache_write_tokens": 0,
		"spend_usd": 0.0, "has_estimate": false,
	}
	addInt := func(key string, n int) {
		totals[key] = totals[key].(int) + n
	}
	for _, id := range ids {
		sess, err := store.Load(id)
		if err != nil {
			continue
		}
		summary, runPoints := webSessionStats(sess, endpoints)
		points = append(points, runPoints...)
		sessions = append(sessions, summary)
		addInt("runs", summary["runs"].(int))
		addInt("tool_calls", summary["tool_calls"].(int))
		addInt("errors", summary["errors"].(int))
		addInt("input_tokens", summary["input_tokens"].(int))
		addInt("output_tokens", summary["output_tokens"].(int))
		addInt("cache_read_tokens", summary["cache_read_tokens"].(int))
		addInt("cache_write_tokens", summary["cache_write_tokens"].(int))
		totals["spend_usd"] = totals["spend_usd"].(float64) + summary["spend_usd"].(float64)
		if summary["has_estimate"].(bool) {
			totals["has_estimate"] = true
		}
	}
	endpointList := []map[string]any{}
	for host, agg := range endpoints {
		// The shared ratio, not cached/input: split-reporting endpoints count
		// fresh tokens alone in input, so the naive quotient exceeds 1 and the
		// dashboard prints 270% hit rates. The discriminator lives in the
		// function, which is pinned by its own tests.
		hitRate := (llm.UsageInfo{InputTokens: agg.input, CachedInputTokens: agg.cached}).CacheReadRatio()
		models := make([]string, 0, len(agg.models))
		for model := range agg.models {
			models = append(models, model)
		}
		sort.Strings(models)
		endpointList = append(endpointList, map[string]any{
			"endpoint":     host,
			"hit_rate":     hitRate,
			"read_tokens":  agg.cached,
			"write_tokens": agg.written,
			"spend_usd":    agg.spend,
			"runs":         agg.runs,
			"models":       models,
		})
	}
	sort.Slice(endpointList, func(i, j int) bool {
		return endpointList[i]["spend_usd"].(float64) > endpointList[j]["spend_usd"].(float64)
	})
	// Newest activity first, so the dashboard opens on the live session rather
	// than whatever the filesystem listed first. Sessions without runs carry
	// no timestamp and sink to the bottom, which is where dead sessions belong.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i]["updated_at"].(string) > sessions[j]["updated_at"].(string)
	})
	// Per-run points for the token chart, newest last. The chart buckets them
	// client-side by timeframe, so the backend reports instants and never
	// invents a bucket shape the data does not have.
	sort.Slice(points, func(i, j int) bool {
		return points[i]["t"].(string) < points[j]["t"].(string)
	})
	return map[string]any{
		"sessions":  sessions,
		"endpoints": endpointList,
		"totals":    totals,
		"points":    points,
	}, nil
}

type endpointAggregate struct {
	input   int
	cached  int
	written int
	spend   float64
	runs    int
	models  map[string]bool
}

func webSessionStats(sess *sessionstore.Session, endpoints map[string]*endpointAggregate) (map[string]any, []map[string]any) {
	summary := map[string]any{
		"id": sess.ID, "title": sess.Title, "display_title": sess.DisplayTitle(),
		"runs": 0, "tool_calls": len(sess.Operations), "errors": 0,
		"input_tokens": 0, "output_tokens": 0,
		"cache_read_tokens": 0, "cache_write_tokens": 0,
		"spend_usd": 0.0, "has_estimate": false, "updated_at": "",
	}
	add := func(key string, n int) {
		summary[key] = summary[key].(int) + n
	}
	runPoints := []map[string]any{}
	for _, run := range sess.Runs {
		summary["runs"] = summary["runs"].(int) + 1
		if run.Status == sessionstore.RunFailed || strings.TrimSpace(run.Error) != "" {
			summary["errors"] = summary["errors"].(int) + 1
		}
		add("input_tokens", run.Usage.InputTokens)
		add("output_tokens", run.Usage.OutputTokens)
		add("cache_read_tokens", run.Usage.CachedInputTokens)
		add("cache_write_tokens", run.Usage.CacheWriteTokens)
		if run.Cost.Known {
			summary["spend_usd"] = summary["spend_usd"].(float64) + run.Cost.TotalUSD
		} else {
			summary["has_estimate"] = true
		}
		stamp := run.UpdatedAt
		if stamp.IsZero() {
			stamp = run.CreatedAt
		}
		if !stamp.IsZero() {
			runPoints = append(runPoints, map[string]any{
				"t":     stamp.UTC().Format(time.RFC3339),
				"in":    run.Usage.InputTokens,
				"out":   run.Usage.OutputTokens,
				"read":  run.Usage.CachedInputTokens,
				"write": run.Usage.CacheWriteTokens,
				"cost":  run.Cost.TotalUSD,
				"known": run.Cost.Known,
			})
		}
		host := strings.TrimSpace(run.ProviderHost)
		if host == "" {
			continue
		}
		agg := endpoints[host]
		if agg == nil {
			agg = &endpointAggregate{models: map[string]bool{}}
			endpoints[host] = agg
		}
		agg.input += run.Usage.InputTokens
		agg.cached += run.Usage.CachedInputTokens
		agg.written += run.Usage.CacheWriteTokens
		agg.runs++
		if run.Cost.Known {
			agg.spend += run.Cost.TotalUSD
		}
		if strings.TrimSpace(run.PricingModel) != "" {
			agg.models[run.PricingModel] = true
		}
		if stamp := run.UpdatedAt; !stamp.IsZero() {
			summary["updated_at"] = stamp.UTC().Format(time.RFC3339)
		}
	}
	return summary, runPoints
}

// webSnapshots lists recent file snapshots, newest first. They are
// file-level captures, so the dashboard shows them as recent activity rather
// than per-session history.
func webSnapshots() ([]miniapp.SnapshotEntry, error) {
	dir := envs.Get("SESSION_FILE")
	if dir == "" {
		dir = "sessions"
	}
	store, err := snapshot.New(filepath.Join(dir, ".snapshots"))
	if err != nil {
		return nil, err
	}
	entries, err := store.Entries()
	if err != nil {
		return nil, err
	}
	out := make([]miniapp.SnapshotEntry, 0, len(entries))
	for i, entry := range entries {
		if i >= 20 {
			break
		}
		label := entry.Path
		if label == "" {
			label = entry.Hash
		}
		out = append(out, miniapp.SnapshotEntry{
			ID:    entry.Hash,
			Label: label,
			Time:  entry.Timestamp.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func webTodos(sessionID string) ([]miniapp.TodoEntry, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("a session id is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		return nil, err
	}
	todos, err := store.Load(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]miniapp.TodoEntry, 0, len(todos))
	for i, item := range todos {
		out = append(out, miniapp.TodoEntry{
			ID:   fmt.Sprintf("td-%d", i+1),
			Text: item.Content,
			Done: strings.EqualFold(strings.TrimSpace(item.Status), "completed"),
		})
	}
	return out, nil
}

func webRenameSession(id, title string) error {
	store, err := configuredStore()
	if err != nil {
		return err
	}
	return store.Update(id, func(sess *sessionstore.Session) error {
		sess.Title = title
		sess.TitleSource = "dashboard"
		return nil
	})
}

func webDeleteSession(id string) error {
	store, err := configuredStore()
	if err != nil {
		return err
	}
	return store.Delete(id)
}

// webFiles lists one directory for the file browser. Only the uploads dir and
// the workspace root are reachable: anything else, including ".." escapes and
// absolute paths, is refused. The browser shows user files, not a shell.
func webFiles(rel string) ([]miniapp.FileEntry, error) {
	clean := filepath.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	if strings.HasPrefix(clean, "/..") || clean == "/.." {
		return nil, fmt.Errorf("path escapes the file roots")
	}
	var base string
	rest := strings.TrimPrefix(clean, "/")
	switch {
	case rest == "" || rest == "files" || strings.HasPrefix(rest, "files/"):
		base = paths.FilesDir()
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, "files"), "/")
	case rest == "workspace" || strings.HasPrefix(rest, "workspace/"):
		base = configuredWorkspaceRoot()
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, "workspace"), "/")
	default:
		return nil, fmt.Errorf("only files/ and workspace/ are browsable")
	}
	target := filepath.Join(base, filepath.FromSlash(rest))
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("no such directory")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}
	dir, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	entries := make([]miniapp.FileEntry, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, ".") {
			continue
		}
		stat, err := os.Stat(filepath.Join(target, name))
		if err != nil {
			continue
		}
		entries = append(entries, miniapp.FileEntry{
			Name:  name,
			Size:  stat.Size(),
			IsDir: stat.IsDir(),
		})
		if len(entries) >= 500 {
			break
		}
	}
	return entries, nil
}

// webStatus reports what the dashboard header shows. The active chat runs come
// from the bot's own registry; the tunnel address from the surface, which may
// not have one yet. Update state is the startup check's last answer, not a
// fresh probe: a status poll must never make a network call.
func webStatus(tunnelAddr func() string) func() (map[string]any, error) {
	return func() (map[string]any, error) {
		// Active run titles resolve through the session store, because a raw
		// session id is an implementation detail the dashboard must not show.
		// DisplayTitle redacts secrets, so it is safe to render.
		var store *sessionstore.Store
		runs := []map[string]any{}
		activeRuns.Range(func(key, value any) bool {
			chat, _ := key.(int64)
			run, _ := value.(*activeRun)
			if run == nil {
				return true
			}
			entry := map[string]any{
				"chat":    chat,
				"session": run.sessionID,
				"title":   run.sessionID,
			}
			if store == nil {
				store, _ = configuredStore()
			}
			if store != nil {
				if sess, err := store.Load(run.sessionID); err == nil {
					entry["title"] = sess.DisplayTitle()
				}
			}
			runs = append(runs, entry)
			return true
		})
		queued := 0
		promptQueue.mu.Lock()
		for _, pending := range promptQueue.queue {
			queued += len(pending)
		}
		promptQueue.mu.Unlock()
		update, updateChecked := lastUpdateResult()
		return map[string]any{
			"running":          true,
			"version":          build.Version,
			"update_check":     updateCheckActive(),
			"update_check_pin": os.Getenv("CERAA_DISABLE_UPDATE_CHECK") != "",
			"update_available": updateChecked && update.Available,
			"update_latest":    update.Latest,
			"tunnel":           tunnelAddr(),
			"active_runs":      runs,
			"queued_prompts":   queued,
			"workspace":        configuredWorkspaceRoot(),
			"files":            paths.FilesDir(),
			"session":          miniapp.WebSessionID,
		}, nil
	}
}

func webSetTodo(sessionID, content, status string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		return err
	}
	return store.Update(sessionID, content, status)
}

// webProjects lists the workspace's project directories with the current one,
// mirroring the project menu: subdirectories of the workspace root, never
// anything outside it, because a project outside the root is outside the
// session's reach.
func webProjects() (map[string]any, error) {
	root := configuredWorkspaceRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	entries := []string{}
	for _, name := range names {
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.IsDir() {
			continue
		}
		entries = append(entries, name)
		if len(entries) >= 50 {
			break
		}
	}
	sort.Strings(entries)
	// The current root is the one mounted for this surface, which is what
	// webSetProject writes. Reading the Telegram chat's selection instead made
	// the marker name a root the dashboard had not chosen.
	defaultState.mu.RLock()
	current := defaultState.SessionProjects[miniapp.WebSessionID]
	defaultState.mu.RUnlock()
	return map[string]any{
		"workspace": root,
		"current":   current,
		"entries":   entries,
	}, nil
}

// webSetProject mounts a project root for this surface's own session.
//
// It used to write under the authorised user's chat id, which set the Telegram
// chat's project rather than the dashboard's, and then read it back under the
// same key. That made the picker select a project the web session never used and
// the CURRENT marker name a root the surface was not running in. Keying both
// sides on the web session makes the control mean what the page says it does,
// and leaves the Telegram chat's own project selection alone.
func webSetProject(root string) error {
	abs, err := validateWorkspaceProject(root)
	if err != nil {
		return err
	}
	defaultState.mu.Lock()
	defaultState.SessionProjects[miniapp.WebSessionID] = abs
	defaultState.mu.Unlock()
	return defaultState.save()
}

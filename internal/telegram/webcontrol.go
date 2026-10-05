package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/scheduler"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/snapshot"
	"ceraa/harness/todo"
	"ceraa/harness/tool"
	"ceraa/internal/build"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/envs"
	"ceraa/internal/hermesauth"
	"ceraa/internal/miniapp"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
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
	registry := tool.DefaultRegistryWithStores(nil, nil)
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
	list := make([]miniapp.ProviderEntry, 0, len(names))
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
	stored := config.ProviderProfile{}
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
		out["context_limit"] = cfg.LLM.ContextLimit
		out["max_output_tokens"] = cfg.LLM.MaxOutputTokens
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
	defaultState.mu.RLock()
	allowed := make([]string, 0, len(defaultState.AlwaysAllow))
	for name := range defaultState.AlwaysAllow {
		allowed = append(allowed, name)
	}
	defaultState.mu.RUnlock()
	sort.Strings(allowed)
	registry := tool.DefaultRegistryWithStores(nil, nil)
	inventory := []miniapp.ToolEntry{}
	for _, t := range registry.All() {
		inventory = append(inventory, miniapp.ToolEntry{Name: t.Name(), Description: t.Description()})
	}
	return allowed, inventory, nil
}

func webSetToolAllowed(name string, allowed bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tool name is empty")
	}
	registry := tool.DefaultRegistryWithStores(nil, nil)
	if _, ok := registry.Get(name); !ok {
		return fmt.Errorf("no tool named %q", name)
	}
	return defaultState.setAlwaysAllow(name, allowed)
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
	sess, err := store.Load(sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	tokens := 0
	for _, message := range sess.Messages {
		tokens += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
	}
	compactor := newCompactor(cfg, cfg.EffectiveProviderID(), sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	compacted, err := compactor.Compact(ctx, sess)
	if err != nil {
		return nil, err
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
		hitRate := 0.0
		if agg.input > 0 {
			hitRate = float64(agg.cached) / float64(agg.input)
		}
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
		runs := []map[string]any{}
		activeRuns.Range(func(key, value any) bool {
			chat, _ := key.(int64)
			run, _ := value.(*activeRun)
			if run == nil {
				return true
			}
			runs = append(runs, map[string]any{
				"chat":    chat,
				"session": run.sessionID,
			})
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
			"update_check":     os.Getenv("CERAA_DISABLE_UPDATE_CHECK") == "",
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
	return map[string]any{
		"workspace": root,
		"current":   defaultState.project(defaultState.allowedUserID()),
		"entries":   entries,
	}, nil
}

func webSetProject(root string) error {
	return defaultState.setProject(defaultState.allowedUserID(), root)
}

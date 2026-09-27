package menus

import (
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/scheduler"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/todo"
	"ceraa/harness/tool"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/paths"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var All = []string{
	"status", "session", "provider", "model", "agent", "variant", "thinking", "project", "tools", "todos",
	"context", "compaction_level", "file_browser", "settings", "permission", "search",
	"index", "snapshots", "message_history", "skills_catalog", "command_catalog", "mcp_catalog", "worktree",
	"memory", "skill", "eval", "schedule", "codex",
}

type runtimeState struct {
	Model            string                            `json:"model,omitempty"`
	Provider         string                            `json:"provider,omitempty"`
	ProviderProfiles map[string]config.ProviderProfile `json:"provider_profiles,omitempty"`
	Agent            string                            `json:"agent,omitempty"`
	Variant          string                            `json:"variant,omitempty"`
	Reasoning        string                            `json:"reasoning,omitempty"`
	AutoCompaction   bool                              `json:"auto_compaction,omitempty"`
	AutoSet          bool                              `json:"auto_compaction_set,omitempty"`
	Projects         map[string]string                 `json:"projects,omitempty"`
}

func runtimeValues() runtimeState {
	data, _ := os.ReadFile(filepath.Join(paths.DataDir(), "bridge-state.json"))
	var state runtimeState
	_ = json.Unmarshal(data, &state)
	return state
}

func projectForChat(chatID int64) string {
	state := runtimeValues()
	root := state.Projects[strconv.FormatInt(chatID, 10)]
	workspace := workspaceRootPath()
	if root == "" {
		return workspace
	}
	rel, err := filepath.Rel(workspace, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return workspace
	}
	return root
}

func workspaceRootPath() string {
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return config.DefaultConfig().WorkspaceRootPath()
	}
	return cfg.WorkspaceRootPath()
}

func configuredStore() (*sessionstore.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return sessionstore.New(cfg.SessionFile)
}

func InlinePath(path string) map[string]any {
	return fileBrowserMenu(path)
}

func Inline(name string, page int) map[string]any {
	return InlineForSession(name, page, "")
}

func InlineForChat(name string, page int, chatID int64, sessionID string) map[string]any {
	root := projectForChat(chatID)
	switch name {
	case "project":
		return projectMenu(workspaceRootPath())
	case "file_browser":
		if root == "" {
			return simpleMenu("Files", "Choose a project before browsing files")
		}
		return fileBrowserMenu(root)
	case "provider":
		return ProviderMenu()
	case "model":
		return ModelMenu(0)
	case "model_catalog":
		return ModelCatalogForChat(chatID, "")
	case "memory":
		return memoryMenu(chatID, sessionID)
	case "title_settings":
		return titleSettingsMenu()
	case "skill":
		return skillMenu()
	case "eval":
		return evalMenu()
	case "schedule":
		return scheduleMenu()
	default:
		return InlineForSession(name, page, sessionID)
	}
}

func InlineForSession(name string, page int, sessionID string) map[string]any {
	_ = page
	switch name {
	case "status", "context":
		return statusMenu(sessionID)
	case "provider":
		return ProviderMenu()
	case "model":
		return ModelMenu(0)
	case "agent":
		return agentMenu()
	case "variant":
		return variantMenu()
	case "thinking":
		return thinkingMenu()
	case "session":
		return sessionMenu(sessionID)
	case "file_browser":
		return fileBrowserMenu(".")
	case "project":
		return projectMenu()
	case "tools", "skills_catalog":
		return toolsMenu()
	case "todos":
		return todosMenu(sessionID)
	case "codex":
		return CodexMenu(codexauth.Authenticated(), runtimeValues().Provider)
	case "settings":
		return settingsMenu()
	case "title_settings":
		return titleSettingsMenu()
	case "permission":
		return permissionMenu()
	case "compaction_level":
		return compactionMenu()
	case "mcp_catalog":
		return simpleMenu("MCP", "No MCP servers configured")
	case "command_catalog":
		return commandMenu()
	case "index":
		return indexMenu()
	case "snapshots":
		return snapshotMenu()
	case "search":
		return searchMenu()
	case "message_history":
		return historyMenu()
	case "worktree":
		return worktreeMenu()
	case "memory":
		return memoryMenu(0, sessionID)
	case "skill":
		return skillMenu()
	case "eval":
		return evalMenu()
	case "schedule":
		return scheduleMenu()
	default:
		return closeMenu()
	}
}

func closeMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "✕ Close", "callback_data": "close"}}}}
}

// ProviderEntry is one provider as the menus see it.
//
// The stores disagree about where a provider lives, and that disagreement is why
// removal appeared not to work: the select menu merged config.json with
// bridge-state.json, while the remove menu listed only bridge-state.json and
// removal deleted only from bridge-state.json. A provider in both was listed once,
// removed from one store, and then still on screen; a provider in config.json alone
// was selectable but had nothing to tap to remove it.
type ProviderEntry struct {
	ID      string
	Profile config.ProviderProfile
	// InConfig and InState record which stores hold this id, so a caller removing it
	// knows where to look.
	InConfig bool
	InState  bool
	// Removable is false for the base configuration, which is not a stored profile.
	Removable bool
}

// ProviderEntries returns every provider the menus can show, sorted by id.
//
// One definition, used by both the select menu and the remove menu, so the two
// cannot drift apart again.
func ProviderEntries() ([]ProviderEntry, string) {
	cfg, _ := config.Load()
	state := runtimeValues()
	merged := map[string]*ProviderEntry{}
	add := func(id string, profile config.ProviderProfile, inConfig, inState bool) {
		entry, ok := merged[id]
		if !ok {
			entry = &ProviderEntry{ID: id, Removable: id != "default"}
			merged[id] = entry
		}
		entry.Profile = profile
		entry.InConfig = entry.InConfig || inConfig
		entry.InState = entry.InState || inState
	}
	if cfg != nil {
		for id, profile := range cfg.ProviderProfiles {
			add(id, profile, true, false)
		}
		add("default", config.ProviderProfile{URL: cfg.LLM.URL, Model: cfg.LLM.Model}, true, false)
	}
	for id, profile := range state.ProviderProfiles {
		add(id, profile, false, true)
	}
	entries := make([]ProviderEntry, 0, len(merged))
	for _, entry := range merged {
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	active := state.Provider
	if active == "" {
		active = "default"
	}
	return entries, active
}

// ModelMenuForTest exposes ModelMenu for tests in another package.
func ModelMenuForTest() map[string]any { return ModelMenu(0) }

// ProviderMenu lists providers, and only providers.
//
// It used to be one menu called "Provider / Model" that listed a provider per row
// with the model appended to the label, which made both halves harder to read: the
// model was repeated on every provider row while being separately choosable, and
// "remove provider" sat in the middle of it. Provider identity and model choice
// are separate decisions and now have separate screens.
func ProviderMenu() map[string]any {
	entries, active := ProviderEntries()
	rows := make([][]map[string]string, 0, len(entries)+4)
	for _, entry := range entries {
		label := entry.ID
		if entry.ID == active {
			label = "✅ " + label
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "select_provider:" + entry.ID}})
	}
	// Codex is a row rather than only a profile, because a profile row cannot exist
	// until the profile does, and leaving it solely in onboarding meant a configured
	// user had no way to reach it from here.
	codexLabel := "🧠 Codex (ChatGPT)"
	switch {
	case active == llm.CodexSubscription:
		codexLabel = "✅ " + codexLabel
	case codexauth.Authenticated():
		codexLabel += " · switch to it"
	default:
		codexLabel += " · sign in"
	}
	rows = append(rows, []map[string]string{{"text": codexLabel, "callback_data": "provider:codex"}})
	rows = append(rows, []map[string]string{{"text": "➕ Custom provider", "callback_data": "provider:custom"}})
	rows = append(rows, []map[string]string{{"text": "🗑 Remove provider", "callback_data": "provider:remove_menu"}})
	rows = append(rows, []map[string]string{{"text": "◀️ Back", "callback_data": "menu:hub"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

// ModelMenu shows the model that is answering and the settings that shape how it is
// called, so the two can be judged together instead of inferred.
func ModelMenu(chatID int64) map[string]any {
	entries, active := ProviderEntries()
	var profile config.ProviderProfile
	found := false
	for _, entry := range entries {
		if entry.ID == active {
			profile, found = entry.Profile, true
			break
		}
	}
	rows := make([][]map[string]string, 0, 4)
	if found && strings.TrimSpace(profile.Model) != "" {
		rows = append(rows, []map[string]string{{"text": "🤖 " + profile.Model, "callback_data": "noop"}})
	}
	rows = append(rows, []map[string]string{{"text": detailsText(active, found, profile), "callback_data": "noop"}})
	rows = append(rows, []map[string]string{{"text": "🔎 Choose model", "callback_data": "menu:model_catalog"}})
	rows = append(rows, []map[string]string{{"text": "◀️ Back", "callback_data": "menu:hub"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

// detailsText renders the settings attached to the active model.
//
// Telegram truncates button text, so the detail is split across several buttons
// rather than crammed into one. Every line is a fact read from the active profile
// rather than a restatement of the model name.
func detailsText(active string, found bool, profile config.ProviderProfile) string {
	if !found || strings.TrimSpace(profile.Model) == "" {
		return "No model is active yet.\nPick one below."
	}
	lines := []string{"Provider: " + active}
	if host := endpointHost(profile.URL); host != "" {
		lines = append(lines, "Endpoint: "+host)
	}
	if profile.ContextLimit > 0 {
		lines = append(lines, "Context: "+humanTokens(profile.ContextLimit))
	} else {
		lines = append(lines, "Context: from catalog")
	}
	if profile.MaxOutputTokens > 0 {
		lines = append(lines, "Max output: "+humanTokens(profile.MaxOutputTokens))
	}
	if llm.IsSubscription(profile.Subscription) {
		lines = append(lines, "Auth: "+profile.Subscription+" subscription")
	} else if profile.APIKey != "" {
		lines = append(lines, "Auth: API key")
	}
	cache := strings.TrimSpace(profile.PromptCacheMode)
	if cache == "" {
		cache = "auto"
	}
	lines = append(lines, "Cache: "+cache)
	if ttl := strings.TrimSpace(profile.PromptCacheTTL); ttl != "" {
		lines = append(lines, "Cache TTL: "+ttl)
	}
	return strings.Join(lines, "\n")
}

// endpointHost reduces a base URL to a host for display.
func endpointHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return trimmed
	}
	return parsed.Host
}

// humanTokens renders a token count compactly.
func humanTokens(n int) string {
	if n >= 1000 {
		return strconv.Itoa(n/1000) + "k"
	}
	return strconv.Itoa(n)
}

var modelChoices []llm.ModelMetadata

func ModelCatalogForChat(chatID int64, query string) map[string]any {
	cfg, _ := config.Load()
	state := runtimeValues()
	active := state.Provider
	if active == "" {
		active = "default"
	}
	profileURL := ""
	for id, profile := range cfg.ProviderProfiles {
		if id == active {
			profileURL = profile.URL
		}
	}
	if profile, ok := state.ProviderProfiles[active]; ok {
		profileURL = profile.URL
	}
	if profileURL == "" {
		profileURL = cfg.LLM.URL
	}
	catalog := llm.NewCatalogClient(cfg.ModelCatalogURL, cfg.ModelCatalogCache, time.Duration(cfg.ModelCatalogRefreshHours)*time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, _ := catalog.List(ctx)
	prefix := ""
	if strings.Contains(profileURL, "poolside") {
		prefix = "poolside/"
	}
	filtered := make([]llm.ModelMetadata, 0, len(models))
	for _, model := range models {
		if prefix != "" && !strings.HasPrefix(strings.ToLower(model.ID), prefix) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(model.ID+" "+model.Name), strings.ToLower(query)) {
			continue
		}
		filtered = append(filtered, model)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })
	modelChoices = filtered
	rows := make([][]map[string]string, 0, 12)
	for i, model := range filtered {
		if i >= 10 {
			break
		}
		label := model.ID
		if len(label) > 44 {
			label = label[:44] + "..."
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": fmt.Sprintf("model:%d", i)}})
	}
	if len(rows) == 0 {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "🔎 Search models", "callback_data": "model_catalog_search"}, {"text": "✏️ Enter model ID", "callback_data": "provider:model_manual"}},
			{{"text": "⬅️ Provider / Model", "callback_data": "menu:provider"}, {"text": "✕ Close", "callback_data": "close"}},
		}}
	}
	rows = append(rows, []map[string]string{{"text": "🔎 Search models", "callback_data": "model_catalog_search"}, {"text": "✏️ Enter model ID", "callback_data": "provider:model_manual"}})
	rows = append(rows, []map[string]string{{"text": "⬅️ Provider / Model", "callback_data": "menu:provider"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func ModelChoice(index int) (llm.ModelMetadata, bool) {
	if index < 0 || index >= len(modelChoices) {
		return llm.ModelMetadata{}, false
	}
	return modelChoices[index], true
}

func agentMenu() map[string]any {
	state := runtimeValues()
	agent := state.Agent
	if agent == "" {
		agent = "build"
	}
	rows := [][]map[string]string{}
	for _, value := range []string{"build", "plan"} {
		label := value
		if value == agent {
			label = "✅ " + value
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "select_agent:" + value}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func variantMenu() map[string]any {
	variant := runtimeValues().Variant
	if variant == "" {
		variant = "default"
	}
	rows := [][]map[string]string{}
	for _, value := range []string{"default", "compact"} {
		label := value
		if value == variant {
			label = "✅ " + value
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "select_variant:" + value}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

// thinkingMenu exposes the reasoning level. It is separate from the variant menu
// because a variant changes how an answer reads, while this changes how much the
// model thinks before producing one.
func thinkingMenu() map[string]any {
	current := runtimeValues().Reasoning
	rows := [][]map[string]string{}
	row := []map[string]string{}
	for _, value := range []string{"off", "minimal", "low", "medium", "high", "max"} {
		label := value
		if value == current {
			label = "✅ " + value
		}
		row = append(row, map[string]string{"text": label, "callback_data": "select_reasoning:" + value})
		if len(row) == 3 {
			rows = append(rows, row)
			row = []map[string]string{}
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	// Default clears the runtime override so the provider's own default applies.
	defaultLabel := "🌐 default"
	if current == "" {
		defaultLabel = "✅ 🌐 default"
	}
	rows = append(rows, []map[string]string{{"text": defaultLabel, "callback_data": "select_reasoning:default"}})
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func sessionMenu(active string) map[string]any {
	store, err := configuredStore()
	if err != nil {
		return simpleMenu("Sessions", "Session store unavailable")
	}
	ids, _ := store.List()
	var rows [][]map[string]string
	shown := 0
	for _, id := range ids {
		sess, err := store.Load(id)
		if err != nil || sess.ID != id || id == "inbox" {
			continue
		}
		if shown >= 10 {
			break
		}
		label := fmt.Sprintf("%s · %d msgs", sess.DisplayTitle(), len(sess.Messages))
		if id == active {
			label = "✅ " + label
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "attach_session:" + id}})
		rows = append(rows, []map[string]string{
			{"text": "✏️ Rename", "callback_data": "session_rename:" + id},
			{"text": "🔁 Regenerate title", "callback_data": "session_regenerate:" + id},
			{"text": "🗑 Delete", "callback_data": "session_delete:" + id},
		})
		shown++
	}
	if len(rows) == 0 {
		return simpleMenu("Sessions", "No sessions yet")
	}
	rows = append(rows, []map[string]string{{"text": "➕ New session", "callback_data": "new_session"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func fileBrowserMenu(path string) map[string]any {
	if strings.TrimSpace(path) == "" {
		return simpleMenu("Files", "Choose a project before browsing files")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return simpleMenu("Files", "Cannot read "+path)
	}
	rows := [][]map[string]string{}
	for i, entry := range entries {
		if i >= 10 {
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		rows = append(rows, []map[string]string{{"text": name, "callback_data": "browse:" + filepath.Join(path, entry.Name())}})
	}
	if path != "." {
		rows = append(rows, []map[string]string{{"text": "⬆️ Up", "callback_data": "browse:" + filepath.Dir(path)}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func projectMenu(roots ...string) map[string]any {
	root := workspaceRootPath()
	if len(roots) > 0 && roots[0] != "" {
		root = roots[0]
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return simpleMenu("Projects", "Workspace root unavailable: "+err.Error())
	}
	rows := [][]map[string]string{}
	entries, _ := os.ReadDir(root)
	shown := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || shown >= 8 {
			continue
		}
		rows = append(rows, []map[string]string{{"text": "📁 " + entry.Name(), "callback_data": "project:" + filepath.Join(root, entry.Name())}})
		shown++
	}
	if len(rows) == 0 {
		return simpleMenu("Projects", "Workspace root: "+root)
	}
	rows = append(rows, []map[string]string{{"text": "📍 Workspace root", "callback_data": "project:" + root}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func settingsMenu() map[string]any {
	cfg, _ := config.Load()
	model, url, limit := "unknown", "", 0
	if cfg != nil {
		model, url, limit = cfg.LLM.Model, cfg.LLM.URL, cfg.LLM.ContextLimit
	}
	rows := [][]map[string]string{
		{{"text": "🧠 " + model, "callback_data": "menu:provider"}},
		{{"text": "🏷️ Session title settings", "callback_data": "menu:title_settings"}},
		{{"text": "🧮 Context: " + strconv.Itoa(limit), "callback_data": "menu:context"}},
		{{"text": "🌐 " + url, "callback_data": "noop"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func titleSettingsMenu() map[string]any {
	cfg, _ := config.Load()
	model := "gemini-3.5-flash-lite"
	if cfg != nil && cfg.TitleModel != "" {
		model = cfg.TitleModel
	}
	defaultLabel := "Gemini 3.5 Flash Lite"
	if model == "gemini-3.5-flash-lite" {
		defaultLabel = "✅ " + defaultLabel
	}
	customLabel := "✏️ Custom model: " + model
	if model == "gemini-3.5-flash-lite" {
		customLabel = "✏️ Enter custom model ID"
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🏷️ Current: " + model, "callback_data": "noop"}},
		{{"text": defaultLabel, "callback_data": "title_model:default"}},
		{{"text": customLabel, "callback_data": "title_model:custom"}},
		{{"text": "🔑 Set Gemini API key", "callback_data": "title_key:set"}, {"text": "🧪 Test title model", "callback_data": "title_test"}},
		{{"text": "⬅️ Settings", "callback_data": "menu:settings"}, {"text": "✕ Close", "callback_data": "close"}},
	}}
}

func statusMenu(sessionID string) map[string]any {
	_ = sessionID
	rows := [][]map[string]string{
		{{"text": "🗜️ Compact now", "callback_data": "compact:now"}},
		{{"text": "💬 Recent messages", "callback_data": "menu:message_history"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}
	return map[string]any{"inline_keyboard": rows}
}

func toolsMenu() map[string]any {
	rows := [][]map[string]string{}
	for _, registered := range tool.DefaultRegistry().All() {
		rows = append(rows, []map[string]string{{"text": "🔧 " + registered.Name(), "callback_data": "tool:" + registered.Name()}})
		if len(rows) >= 12 {
			break
		}
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func todosMenu(sessionID string) map[string]any {
	if sessionID == "" {
		return simpleMenu("Todos", "No active session")
	}
	cfg, _ := config.Load()
	if cfg == nil {
		return simpleMenu("Todos", "Config unavailable")
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		return simpleMenu("Todos", err.Error())
	}
	items, _ := store.Load(sessionID)
	rows := [][]map[string]string{}
	for i, item := range items {
		if i >= 10 {
			break
		}
		label := item.Status + " · " + item.Content
		rows = append(rows, []map[string]string{{"text": label, "callback_data": fmt.Sprintf("todo:%d:%s", i, item.Status)}})
	}
	if len(rows) == 0 {
		return simpleMenu("Todos", "No todos in this session")
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func compactionMenu() map[string]any {
	state := runtimeValues()
	auto := state.AutoCompaction
	if !state.AutoSet {
		auto = true
	}
	autoLabel := "⚙️ Auto: on"
	autoData := "compaction:auto_off"
	if !auto {
		autoLabel = "⚙️ Auto: off"
		autoData = "compaction:auto_on"
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🗜️ Compact now", "callback_data": "compact:now"}},
		{{"text": autoLabel, "callback_data": autoData}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func permissionMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔒 Risky tool approvals", "callback_data": "permission:policy"}},
		{{"text": "✅ Always allow read/search", "callback_data": "permission:allow_read"}},
		{{"text": "↩️ Reset policy", "callback_data": "permission:reset"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func snapshotMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "↩️ Revert latest file", "callback_data": "revert:prompt"}},
		{{"text": "📋 List snapshots", "callback_data": "snapshots:list"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func searchMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔎 Search index", "callback_data": "search:prompt"}},
		{{"text": "🗃️ Rebuild index", "callback_data": "index:rebuild"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func indexMenu() map[string]any {
	return searchMenu()
}

func historyMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "Last 5", "callback_data": "show_messages:5"}},
		{{"text": "Last 20", "callback_data": "show_messages:20"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func commandMenu() map[string]any {
	rows := [][]map[string]string{}
	for _, command := range Commands() {
		name := "/" + strings.ReplaceAll(command.Command, "-", "_")
		rows = append(rows, []map[string]string{{"text": name, "callback_data": "run_command:" + name}})
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func worktreeMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🌿 Git worktrees", "callback_data": "worktree:list"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func memoryMenu(chatID int64, sessionID string) map[string]any {
	cfg, _ := config.Load()
	project := projectForChat(chatID)
	store, err := memory.New(cfg.MemoryFile)
	if err != nil {
		return simpleMenu("Memory", "Memory store unavailable")
	}
	entries, _ := store.List(project, sessionID)
	rows := make([][]map[string]string, 0, len(entries)+2)
	for i, entry := range entries {
		if i >= 8 {
			break
		}
		label := entry.Content
		if len(label) > 36 {
			label = label[:36] + "..."
		}
		rows = append(rows, []map[string]string{{"text": "🗑 " + label, "callback_data": "memory:remove:" + entry.ID}})
	}
	rows = append(rows, []map[string]string{{"text": "➕ Add memory", "callback_data": "memory:add"}, {"text": "🔄 Refresh", "callback_data": "memory:view"}})
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func skillMenu() map[string]any {
	cfg, _ := config.Load()
	store, err := skills.New(cfg.SkillFile)
	if err != nil {
		return simpleMenu("Skills", "Skill store unavailable")
	}
	proposals, _ := store.ListProposals()
	rows := make([][]map[string]string, 0, len(proposals)+2)
	for _, proposal := range proposals {
		if proposal.Status != skills.Pending {
			continue
		}
		label := proposal.SkillName
		if len(label) > 28 {
			label = label[:28] + "..."
		}
		rows = append(rows, []map[string]string{
			{"text": "✅ " + label, "callback_data": "skill:approve:" + proposal.ID},
			{"text": "✗", "callback_data": "skill:reject:" + proposal.ID},
		})
	}
	rows = append(rows, []map[string]string{{"text": "🔄 Refresh", "callback_data": "skill:view"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

func evalMenu() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🔄 Refresh baseline", "callback_data": "eval:view"}},
		{{"text": "✕ Close", "callback_data": "close"}},
	}}
}

func scheduleMenu() map[string]any {
	cfg, _ := config.Load()
	store, err := scheduler.New(cfg.SchedulerFile)
	if err != nil {
		return simpleMenu("Schedule", "Scheduler store unavailable")
	}
	jobs, _ := store.List()
	rows := make([][]map[string]string, 0, len(jobs)+5)
	for _, job := range jobs {
		label := job.Name
		if label == "" {
			label = job.Prompt
		}
		if len(label) > 32 {
			label = label[:32] + "..."
		}
		rows = append(rows, []map[string]string{{"text": "🗑 " + label, "callback_data": "schedule:remove:" + job.ID}})
	}
	rows = append(rows,
		[]map[string]string{{"text": "➕ Every 30m", "callback_data": "schedule:preset:30m"}, {"text": "➕ Every 1h", "callback_data": "schedule:preset:1h"}},
		[]map[string]string{{"text": "➕ Every 24h", "callback_data": "schedule:preset:24h"}},
		[]map[string]string{{"text": "🔄 Refresh", "callback_data": "schedule:view"}, {"text": "✕ Close", "callback_data": "close"}},
	)
	return map[string]any{"inline_keyboard": rows}
}

func simpleMenu(name, msg string) map[string]any {
	if msg == "" {
		msg = name
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "ℹ️ " + msg, "callback_data": "noop"}, {"text": "✕ Close", "callback_data": "close"}},
	}}
}

func Onboarding() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🟣 OpenRouter", "callback_data": "onboarding:provider:openrouter"}, {"text": "🟠 Poolside", "callback_data": "onboarding:provider:poolside"}},
		{{"text": "🔐 Hermes subscription", "callback_data": "onboarding:provider:hermes"}, {"text": "🧠 Codex (ChatGPT)", "callback_data": "onboarding:provider:codex"}},
		{{"text": "➕ Custom provider", "callback_data": "onboarding:provider:custom"}},
		{{"text": "🔒 Lock bot to this account", "callback_data": "onboarding:claim"}, {"text": "✕ Close", "callback_data": "close"}},
	}}
}

func Root() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "📊 Status", "callback_data": "menu:status"}, {"text": "💬 Sessions", "callback_data": "menu:session"}},
		{{"text": "🔌 Provider", "callback_data": "menu:provider"}, {"text": "🤖 Model", "callback_data": "menu:model"}},
		{{"text": "🤖 Mode", "callback_data": "menu:agent"}, {"text": "💭 Variant", "callback_data": "menu:variant"}},
		{{"text": "🧠 Thinking", "callback_data": "menu:thinking"}},
		{{"text": "📁 Project", "callback_data": "menu:project"}},
		{{"text": "🔧 Tools", "callback_data": "menu:tools"}, {"text": "✅ Todos", "callback_data": "menu:todos"}, {"text": "✕ Close", "callback_data": "close"}},
	}}
}

func Hub() map[string]any {
	labels := map[string]string{
		"status": "📊 Status", "session": "💬 Sessions", "provider": "🔌 Provider", "model": "🤖 Model", "agent": "🤖 Mode",
		"variant": "💭 Variant", "thinking": "🧠 Thinking", "project": "📁 Project", "tools": "🔧 Tools", "todos": "✅ Todos",
		"context": "🧮 Context", "compaction_level": "🗜️ Compact", "file_browser": "📂 Files",
		"settings": "⚙️ Settings", "permission": "🔒 Permissions", "search": "🔎 Search", "index": "🗃️ Index",
		"message_history": "🕘 History", "snapshots": "↩️ Snapshots", "skills_catalog": "🧩 Tools", "command_catalog": "⌘ Commands",
		"mcp_catalog": "🔌 MCP", "worktree": "🌿 Worktree", "memory": "🧠 Memory", "skill": "🧩 Skills", "eval": "📈 Eval", "schedule": "⏰ Schedule",
		"codex": codexHubLabel(),
	}
	rows := [][]map[string]string{}
	for i, name := range All {
		if i%2 == 0 {
			rows = append(rows, []map[string]string{{"text": labels[name], "callback_data": "menu:" + name}})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], map[string]string{"text": labels[name], "callback_data": "menu:" + name})
		}
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

// codexHubLabel names the Codex entry by state, so the hub answers "is Codex
// ready" without being opened.
func codexHubLabel() string {
	switch {
	case codexauth.Authenticated() && runtimeValues().Provider == llm.CodexSubscription:
		return "🧠 Codex · active"
	case codexauth.Authenticated():
		return "🧠 Codex · signed in"
	default:
		return "🧠 Codex · sign in"
	}
}

// CodexMenu is the Codex management screen: the button that does whatever the
// current state calls for.
//
// The authenticated flag is a parameter rather than read here, so the state to
// button mapping can be tested without arranging a real credential.
//
// Codex status used to be reachable only by typing /codex-status, which made the
// whole auth flow unusable from the menus, since a phone user has no reason to
// know a command exists.
func CodexMenu(authenticated bool, active string) map[string]any {
	isActive := active == llm.CodexSubscription
	var rows [][]map[string]string
	switch {
	case !authenticated:
		rows = append(rows, []map[string]string{{"text": "🔑 Sign in to ChatGPT", "callback_data": "codex:login"}})
	case !isActive:
		rows = append(rows, []map[string]string{
			{"text": "✅ Switch to Codex", "callback_data": "codex:activate"},
			{"text": "🔁 Sign in again", "callback_data": "codex:login"},
		})
	default:
		rows = append(rows, []map[string]string{{"text": "🔁 Sign in again", "callback_data": "codex:login"}})
	}
	rows = append(rows, []map[string]string{{"text": "◀️ Back", "callback_data": "menu:hub"}})
	return map[string]any{"inline_keyboard": rows}
}

// RemoveProviderMenu lists the providers that can actually be deleted.
//
// "default" is excluded because it is the base configuration rather than a
// stored profile, and sessions may still be running against the active provider,
// so the active one is marked rather than hidden.
func RemoveProviderMenu(active string) map[string]any {
	// The same merged view the select menu uses. It listed only the runtime store
	// before, so a provider held in config.json was selectable but had no row here
	// to tap, which is the other half of removal looking broken.
	entries, current := ProviderEntries()
	rows := make([][]map[string]string, 0, len(entries)+1)
	for _, entry := range entries {
		if !entry.Removable {
			continue
		}
		label := "🗑 " + entry.ID
		if entry.ID == current || (active != "" && entry.ID == active) {
			label = "⚠️ " + label + " (active)"
		}
		rows = append(rows, []map[string]string{{"text": label, "callback_data": "provider:remove:" + entry.ID}})
	}
	if len(rows) == 0 {
		rows = append(rows, []map[string]string{{"text": "Nothing to remove", "callback_data": "noop"}})
	}
	rows = append(rows, []map[string]string{{"text": "◀️ Provider", "callback_data": "menu:provider"}, {"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

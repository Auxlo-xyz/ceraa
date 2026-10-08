package menus

import (
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/scheduler"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/todo"
	"ceraa/harness/tool"
	"ceraa/internal/clineauth"
	"ceraa/internal/codexauth"
	"ceraa/internal/config"
	"ceraa/internal/deepseekauth"
	"ceraa/internal/hermesauth"
	"ceraa/internal/logger"
	"ceraa/internal/minimaxauth"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/paths"
	"ceraa/internal/secret"
	"ceraa/internal/xaiauth"
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

// All is every menu the hub can offer, in the order it walks them.
//
// "eval", "command_catalog" and "mcp_catalog" were removed rather than hidden.
// None was wanted: the evaluation screen was a dashboard nobody opened, the
// command catalog restated /help with buttons, and the MCP catalog had no servers
// to list and so only ever rendered its own empty state. Each is gone from All,
// from both resolvers, from the advertised command list, and from every dispatch
// site, so a stale keyboard carrying one of these names resolves through the
// default to a bare Close rather than opening something that no longer exists.
var All = []string{
	"status", "session", "provider", "model", "agent", "thinking", "project", "tools", "todos",
	"context", "compaction_level", "file_browser", "settings", "permission", "search",
	"index", "snapshots", "message_history", "skills_catalog", "worktree",
	"memory", "skill", "schedule", "secrets",
	"codex", "hermes", "xai", "minimax", "cline", "opencode", "deepseek",
}

// runtimeState is the bot's own state, read from bridge-state.json.
//
// It holds no provider or model. Those live in config.json and come from
// config.LoadSelection, so a menu cannot describe a provider the run path is not
// using. Only the chat-keyed state the menus need — currently the project map —
// is read here.
type runtimeState struct {
	Projects map[string]string `json:"projects,omitempty"`
}

// runtimeStatePath is the file the hub reads its labels from.
//
// Indirected from the data dir so a test can point it at a temporary file without
// setting CERAA_HOME, which paths.DataDir caches and would then leak between tests.
var runtimeStatePath = ""

func runtimeStateFile() string {
	if runtimeStatePath != "" {
		return runtimeStatePath
	}
	return filepath.Join(paths.DataDir(), "bridge-state.json")
}

func runtimeValues() runtimeState {
	data, _ := os.ReadFile(runtimeStateFile())
	var state runtimeState
	_ = json.Unmarshal(data, &state)
	return state
}

// selection reads the model-facing choice from config.json.
//
// An unreadable config yields the zero value rather than a guess, so a screen that
// needs a model says it has none instead of naming a provider it cannot confirm.
func selection() config.Selection {
	state, err := config.LoadSelection()
	if err != nil {
		return config.Selection{}
	}
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
	case "schedule":
		return scheduleMenu()
	default:
		return InlineForSession(name, page, sessionID)
	}
}

func InlineForSession(name string, page int, sessionID string) map[string]any {
	_ = page
	switch name {
	// The hub is deliberately absent from All: it is the screen the hub links to,
	// not one of the entries it lists, so listing it there would make it open
	// itself. It still needs a case, because four menus emit "menu:hub" as their
	// Back row - ProviderMenu, ModelMenu, subscriptionMenu and CodexMenu - and
	// with no case here that data resolved to the default closeMenu(). Back from
	// any provider or model screen therefore answered with a message reading
	// "📋 hub" carrying nothing but a Close button.
	//
	// The regression guard walks All, which is why it never caught this: the name
	// the buttons emit is not a name All contains.
	case "hub":
		return Hub()
	case "status", "context":
		return statusMenu(sessionID)
	case "provider":
		return ProviderMenu()
	case "model":
		return ModelMenu(0)
	case "agent":
		return agentMenu()
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
	case "secrets":
		return SecretsMenu(storedUserSecrets())
	case "todos":
		return todosMenu(sessionID)
	case "codex":
		return CodexMenu(codexauth.Authenticated(), selection().ActiveProvider)
	case "hermes":
		return subscriptionMenu("Hermes", "🔐", hermesauth.SignedIn(), selection().ActiveProvider == llm.HermesSubscription, "provider:hermes")
	case "xai":
		return subscriptionMenu("xAI", "✳️", xaiauth.SignedIn(), selection().ActiveProvider == llm.XAISubscription, "provider:xai")
	case "minimax":
		return subscriptionMenu("MiniMax", "🎬", minimaxauth.SignedIn(minimaxauth.RegionGlobal), selection().ActiveProvider == llm.MiniMaxSubscription, "provider:minimax")
	case "cline":
		return subscriptionMenu("Cline", "🦞", clineauth.SignedIn(), selection().ActiveProvider == llm.ClineSubscription, "provider:cline")
	case "opencode":
		return subscriptionAuthMenu("OpenCode Zen", opencodeauth.SignedIn(), selection().ActiveProvider == llm.OpenCodeSubscription, "provider:opencode", true)
	case "deepseek":
		return subscriptionMenu("DeepSeek", "🐋", deepseekauth.SignedIn(), selection().ActiveProvider == llm.DeepSeekSubscription, "provider:deepseek")
	case "hermes:models", "xai:models", "minimax:models", "cline:models", "codex:models",
		"opencode:models", "deepseek:models",
		"provider:hermes:models", "provider:xai:models", "provider:minimax:models",
		"provider:cline:models", "provider:codex:models", "provider:opencode:models",
		"provider:deepseek:models":
		// Both spellings. The bare "<id>:models" is the menu name, and the prefixed
		// form is the callback data subscriptionMenu emits. Only the menu name was
		// here, which is the same gap the callback had: the button's data resolved
		// to nothing and tapping Model did nothing.
		return SubscriptionModelMenu(strings.TrimSuffix(strings.TrimPrefix(name, "provider:"), ":models"))
	case "settings":
		return settingsMenu()
	case "title_settings":
		return titleSettingsMenu()
	case "permission":
		return permissionMenu()
	case "compaction_level":
		return compactionMenu()
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
	case "schedule":
		return scheduleMenu()
	default:
		// No fallback here on purpose. One was added while chasing this bug and a
		// test showed nothing reaches it, so it was untestable code wearing a
		// comment about dead ends. The regression guard is the test that walks All
		// and fails if any entry resolves to a bare Close, which is what would
		// catch the next provider added without a menu.
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
	merged := map[string]*ProviderEntry{}
	// One store. This used to merge config.json with bridge-state.json and flag
	// each entry with which file held it, because a provider recorded in one was
	// invisible to the menu reading the other — the split that let a screen and the
	// run path disagree. Both flags are now set together for the same reason: a
	// caller that asks "is this removable" must get the same answer whichever
	// question it is asking.
	add := func(id string, profile config.ProviderProfile) {
		entry, ok := merged[id]
		if !ok {
			entry = &ProviderEntry{ID: id, Removable: id != "default"}
			merged[id] = entry
		}
		entry.Profile = profile
		entry.InConfig = true
		entry.InState = true
	}
	if cfg != nil {
		for id, profile := range cfg.ProviderProfiles {
			add(id, profile)
		}
		add("default", config.ProviderProfile{URL: cfg.LLM.URL, Model: cfg.LLM.Model})
	}
	entries := make([]ProviderEntry, 0, len(merged))
	for _, entry := range merged {
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	active := selection().ActiveProvider
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
	// Hermes is a row for the same reason as Codex. It existed only in onboarding,
	// so a user who had already signed in had no way to reach it from here.
	hermesLabel := "🔐 Hermes (Nous Portal)"
	switch {
	case active == llm.HermesSubscription:
		hermesLabel = "✅ " + hermesLabel
	case hermesauth.SignedIn():
		hermesLabel += " · switch to it"
	default:
		hermesLabel += " · sign in"
	}
	rows = append(rows, []map[string]string{{"text": hermesLabel, "callback_data": "provider:hermes"}})
	// xAI is a row for the same reason: a subscription profile cannot appear as a
	// profile row until it exists, so without this a signed-in user could not
	// reach it from here.
	xaiLabel := "✳️ xAI (Grok)"
	switch {
	case active == llm.XAISubscription:
		xaiLabel = "✅ " + xaiLabel
	case xaiauth.SignedIn():
		xaiLabel += " · switch to it"
	default:
		xaiLabel += " · sign in"
	}
	rows = append(rows, []map[string]string{{"text": xaiLabel, "callback_data": "provider:xai"}})
	// MiniMax is a row for the same reason as the other subscription providers.
	minimaxLabel := "🎬 MiniMax"
	switch {
	case active == llm.MiniMaxSubscription:
		minimaxLabel = "✅ " + minimaxLabel
	case minimaxauth.SignedIn(minimaxauth.RegionGlobal):
		minimaxLabel += " · switch to it"
	default:
		minimaxLabel += " · sign in"
	}
	rows = append(rows, []map[string]string{{"text": minimaxLabel, "callback_data": "provider:minimax"}})
	// Cline is a row for the same reason as the other subscription providers.
	clineLabel := "🦞 Cline Account"
	switch {
	case active == llm.ClineSubscription:
		clineLabel = "✅ " + clineLabel
	case clineauth.SignedIn():
		clineLabel += " · switch to it"
	default:
		clineLabel += " · sign in"
	}
	rows = append(rows, []map[string]string{{"text": clineLabel, "callback_data": "provider:cline"}})
	// OpenCode Zen is a row for the same reason as the others: it is an API key
	// rather than an account login, but it is still a first-class provider.
	openCodeLabel := "🌌 OpenCode Zen"
	switch {
	case active == llm.OpenCodeSubscription:
		openCodeLabel = "✅ " + openCodeLabel
	case opencodeauth.SignedIn():
		openCodeLabel += " · switch to it"
	default:
		openCodeLabel += " · add key"
	}
	rows = append(rows, []map[string]string{{"text": openCodeLabel, "callback_data": "provider:opencode"}})
	// DeepSeek is a row for the same reason as the others: a subscription
	// profile cannot appear as a profile row until it exists, so without this
	// a signed-in user could not reach it from here.
	deepseekLabel := "🐋 DeepSeek Account"
	switch {
	case active == llm.DeepSeekSubscription:
		deepseekLabel = "✅ " + deepseekLabel
	case deepseekauth.SignedIn():
		deepseekLabel += " · switch to it"
	default:
		deepseekLabel += " · sign in"
	}
	rows = append(rows, []map[string]string{{"text": deepseekLabel, "callback_data": "provider:deepseek"}})
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
	state := selection()
	active := state.ActiveProvider
	if active == "" {
		active = "default"
	}
	profileURL := ""
	if profile, ok := state.Profiles[active]; ok {
		profileURL = profile.URL
	}
	if profileURL == "" && cfg != nil {
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
	// A subscription provider is served its own gateway, and the remote catalog is
	// a list of namespaced ids for OpenAI-compatible hosts that it does not
	// serve. Asking it for one of those models produces a request the endpoint
	// cannot answer, so the active provider's own list is used instead.
	// No IsSubscription guard: a provider with no subscription has no models here
	// anyway, so the empty check is the one that decides. An earlier version had
	// both, and a mutation flipping the guard changed nothing, which is what made
	// it untestable code rather than a condition.
	if own := subscriptionCatalogEntries(active); len(own) > 0 {
		modelChoices = own
		return subscriptionModelCatalog(own, active)
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
	agent := selection().Agent
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

// thinkingMenu exposes the reasoning level. It is separate from Mode because Mode
// changes what the agent is allowed to do, while this changes how much the
// model thinks before producing one.
func thinkingMenu() map[string]any {
	current := selection().Reasoning
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
	// Unset means the user never chose, which the menu shows as on.
	auto := true
	if configured := selection().AutoCompaction; configured != nil {
		auto = *configured
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
		{{"text": "🤖 Mode", "callback_data": "menu:agent"}},
		{{"text": "🧠 Thinking", "callback_data": "menu:thinking"}},
		{{"text": "📁 Project", "callback_data": "menu:project"}},
		{{"text": "🔧 Tools", "callback_data": "menu:tools"}, {"text": "✅ Todos", "callback_data": "menu:todos"}, {"text": "✕ Close", "callback_data": "close"}},
	}}
}

// hubHidden lists entries the hub does not show.
//
// "tools" and "skills_catalog" are two buttons for one menu — both resolve to
// toolsMenu() — and the thing behind them is the agent's tool registry, not
// something a person administers. Two identical buttons labelled "Tools" in the
// same grid is worse than one that is absent: it reads as a mistake.
//
// They stay in All and keep dispatching, so a hub keyboard sent before this
// change still opens a real menu instead of a dead button.
var hubHidden = map[string]bool{
	"tools":          true,
	"skills_catalog": true,
}

// SecretsMenu lists stored secrets and offers to add one.
//
// Names and lengths only. The value is never rendered, because a menu is the one
// surface most likely to be screenshotted.
func SecretsMenu(stored []StoredSecret) map[string]any {
	rows := [][]map[string]string{}
	for _, entry := range stored {
		rows = append(rows, []map[string]string{{
			"text":          "🗑️ " + entry.Name + " · " + entry.Length,
			"callback_data": "delsecret:" + entry.Name,
		}})
	}
	if len(rows) == 0 {
		rows = append(rows, []map[string]string{{"text": "— none stored —", "callback_data": "noop"}})
	}
	rows = append(rows, []map[string]string{{"text": "➕ Add Secret", "callback_data": "setsecret:"}})
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

// StoredSecret is one row in the secrets menu: a name and how long its value is.
type StoredSecret struct {
	Name   string
	Length string
}

// StoredUserSecrets is the exported view the bot renders from.
func StoredUserSecrets() []StoredSecret { return storedUserSecrets() }

// storedUserSecrets describes what is stored without decrypting anything into a
// view: the name, and the length, which is enough to tell a truncated paste from a
// complete one and useless for reconstructing the value.
func storedUserSecrets() []StoredSecret {
	names := secret.ListUser()
	out := make([]StoredSecret, 0, len(names))
	for _, name := range names {
		length := "?"
		if value, err := secret.Get(name); err == nil {
			length = fmt.Sprintf("%d chars", len([]rune(value)))
		}
		out = append(out, StoredSecret{Name: name, Length: length})
	}
	return out
}

func Hub() map[string]any {
	labels := map[string]string{
		"status": "📊 Status", "session": "💬 Sessions", "provider": "🔌 Provider", "model": "🤖 Model", "agent": "🤖 Mode",
		"thinking": "🧠 Thinking", "project": "📁 Project", "tools": "🔧 Tools", "todos": "✅ Todos",
		"context": "🧮 Context", "compaction_level": "🗜️ Compact", "file_browser": "📂 Files",
		"settings": "⚙️ Settings", "permission": "🔒 Permissions", "search": "🔎 Search", "index": "🗃️ Index",
		"message_history": "🕘 History", "snapshots": "↩️ Snapshots", "skills_catalog": "🧩 Tools",
		"worktree": "🌿 Worktree", "memory": "🧠 Memory", "skill": "🧩 Skills", "schedule": "⏰ Schedule",
		"secrets": "🔑 Secrets",
		"codex":   codexHubLabel(), "hermes": hermesHubLabel(), "xai": xaiHubLabel(), "minimax": minimaxHubLabel(), "cline": clineHubLabel(),
		"opencode": openCodeHubLabel(), "deepseek": deepseekHubLabel(),
	}
	rows := [][]map[string]string{}
	placed := 0
	for _, name := range All {
		if hubHidden[name] {
			continue
		}
		if placed%2 == 0 {
			rows = append(rows, []map[string]string{{"text": labels[name], "callback_data": "menu:" + name}})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], map[string]string{"text": labels[name], "callback_data": "menu:" + name})
		}
		placed++
	}
	rows = append(rows, []map[string]string{{"text": "✕ Close", "callback_data": "close"}})
	return map[string]any{"inline_keyboard": rows}
}

// codexHubLabel names the Codex entry by state, so the hub answers "is Codex
// ready" without being opened.
// hermesHubLabel names the Hermes entry by state, so the hub answers "is Hermes
// signed in" without the user having to open the provider menu.
func hermesHubLabel() string {
	switch {
	case hermesauth.SignedIn() && selection().ActiveProvider == llm.HermesSubscription:
		return "🔐 Hermes · active"
	case hermesauth.SignedIn():
		return "🔐 Hermes · signed in"
	default:
		return "🔐 Hermes · sign in"
	}
}

// xaiHubLabel names the xAI entry by state.
func xaiHubLabel() string {
	switch {
	case xaiauth.SignedIn() && selection().ActiveProvider == llm.XAISubscription:
		return "✳️ xAI · active"
	case xaiauth.SignedIn():
		return "✳️ xAI · signed in"
	default:
		return "✳️ xAI · sign in"
	}
}

// minimaxHubLabel names the MiniMax entry by state.
//
// The active/signed-in split is decided by selection(), which re-reads
// config.json on every call. A hub that rendered the wrong provider as active
// while the file said otherwise would mean the read is stale, and nothing else in
// the process could say so, so the value that decided it is logged. A hub render
// that cannot explain itself is how this looked like a caching bug for a while.
func minimaxHubLabel() string {
	logger.Info(fmt.Sprintf("hub label minimax: active_provider=%q minimax_subscription=%q",
		selection().ActiveProvider, llm.MiniMaxSubscription))
	switch {
	case minimaxauth.SignedIn(minimaxauth.RegionGlobal) && selection().ActiveProvider == llm.MiniMaxSubscription:
		return "🎬 MiniMax · active"
	case minimaxauth.SignedIn(minimaxauth.RegionGlobal):
		return "🎬 MiniMax · signed in"
	default:
		return "🎬 MiniMax · sign in"
	}
}

// clineHubLabel names the Cline entry by state.
func clineHubLabel() string {
	switch {
	case clineauth.SignedIn() && selection().ActiveProvider == llm.ClineSubscription:
		return "🦞 Cline · active"
	case clineauth.SignedIn():
		return "🦞 Cline · signed in"
	default:
		return "🦞 Cline · sign in"
	}
}

// deepseekHubLabel names the DeepSeek entry by state.
func deepseekHubLabel() string {
	switch {
	case deepseekauth.SignedIn() && selection().ActiveProvider == llm.DeepSeekSubscription:
		return "🐋 DeepSeek · active"
	case deepseekauth.SignedIn():
		return "🐋 DeepSeek · signed in"
	default:
		return "🐋 DeepSeek · sign in"
	}
}

// openCodeHubLabel names the OpenCode Zen entry by state.
//
// The third state says "add key" rather than "sign in", because there is no
// login flow: the credential is an API key the user pastes. Calling it a
// sign-in would promise a flow that does not exist.
func openCodeHubLabel() string {
	switch {
	case opencodeauth.SignedIn() && selection().ActiveProvider == llm.OpenCodeSubscription:
		return "🌌 OpenCode Zen · active"
	case opencodeauth.SignedIn():
		return "🌌 OpenCode Zen · key added"
	default:
		return "🌌 OpenCode Zen · add key"
	}
}

func codexHubLabel() string {
	switch {
	case codexauth.Authenticated() && selection().ActiveProvider == llm.CodexSubscription:
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
// subscriptionMenu is the menu for a subscription provider.
//
// These three were listed in the hub and had no case in the menu resolver, so both
// the hub row and the typed command fell through to a bare Close button. Codex was
// the only one that ever worked, and the same shape is what makes the others
// consistent with it.
func subscriptionMenu(label, icon string, signedIn, active bool, entry string) map[string]any {
	return subscriptionAuthMenu(label, signedIn, active, entry, false)
}

// subscriptionAuthMenu builds the subscription screen, wording the credential
// buttons for how that credential is actually obtained.
//
// keyBased separates a pasted API key from an OAuth login. Both are "you are not
// signed in yet", but only one of them can be told to go sign in: offering
// "Sign in" for a provider whose whole flow is paste a key promises a login
// screen that does not exist, and offering "Add key" for a device login would
// hide the browser step behind the phrase it needs.
func subscriptionAuthMenu(label string, signedIn, active bool, entry string, keyBased bool) map[string]any {
	row := func(text, data string) map[string]string {
		return map[string]string{"text": text, "callback_data": data}
	}
	addKey, rekey := "🔑 Sign in to "+label, "🔁 Sign in again"
	if keyBased {
		addKey, rekey = "🔑 Add "+label+" key", "🔁 Replace key"
	}
	// Re-authentication gets its own callback. It used to be the same entry as
	// "Switch to", and the entry point activates when a credential exists, so the
	// button re-activated the provider instead of signing in again — and removing
	// the provider did not help, because removal clears the profile while the
	// credential stays in the encrypted store. The two intents need two routes.
	relogin := strings.TrimSuffix(entry, ":models") + ":relogin"
	var rows [][]map[string]string
	switch {
	case active:
		rows = append(rows, []map[string]string{
			row("✅ "+label+" is active", "noop"),
			row("🧠 Model", entry+":models"),
			row(rekey, relogin),
		})
	case signedIn:
		rows = append(rows, []map[string]string{
			row("✅ Switch to "+label, entry),
			row("🧠 Model", entry+":models"),
			row(rekey, relogin),
		})
	default:
		rows = append(rows, []map[string]string{row(addKey, entry)})
	}
	rows = append(rows, []map[string]string{row("◀️ Back", "menu:hub")})
	return map[string]any{"inline_keyboard": rows}
}

// subscriptionCatalogEntries renders a subscription provider's models as catalog
// entries, labelled provider/model so it reads as belonging to the signed-in
// account rather than as a global list.
//
// The stored id stays exactly what the gateway expects. Only the label is
// qualified, because the provider rejects an id it does not serve and a display
// name is never sent.
func subscriptionCatalogEntries(providerID string) []llm.ModelMetadata {
	subscription := subscriptionForProvider(providerID)
	label := subscriptionLabel(providerID)
	models := llm.SubscriptionModels(subscription)
	if len(models) == 0 {
		return nil
	}
	current := currentModel()
	entries := make([]llm.ModelMetadata, 0, len(models))
	for _, model := range models {
		name := label + "/" + model
		if active := selection().ActiveProvider; active == providerID && current == model {
			name = "✅ " + name
		}
		entries = append(entries, llm.ModelMetadata{ID: model, Name: name})
	}
	return entries
}

// subscriptionModelCatalog renders the provider/model catalog for an active
// subscription provider, routed through the same model:N callbacks as the generic
// catalog so the choice is applied the same way.
func subscriptionModelCatalog(entries []llm.ModelMetadata, providerID string) map[string]any {
	rows := make([][]map[string]string, 0, len(entries))
	for i, model := range entries {
		rows = append(rows, []map[string]string{{"text": model.Name, "callback_data": "model:" + strconv.Itoa(i)}})
	}
	rows = append(rows, []map[string]string{
		{"text": "◀️ Back", "callback_data": "menu:model"},
		{"text": "✕ Close", "callback_data": "close"},
	})
	return map[string]any{"inline_keyboard": rows}
}

// subscriptionModelMenu lists the models a subscription provider is known to
// serve, so the model can be chosen rather than left at a hardcoded default.
// SubscriptionLabel is the display name for a subscription provider id.
func SubscriptionLabel(providerID string) string { return subscriptionLabel(providerID) }

// SubscriptionModelMenu lists one provider's models, scoped to that provider.
//
// Exported because the callback that opens it has to be answered here, and a menu
// reached from a provider's own screen has to show that provider's models rather
// than the active provider's. Those come apart the moment someone reads the model
// list of a provider they are not currently on, which is the normal way to decide
// before switching.
func SubscriptionModelMenu(providerID string) map[string]any {
	return subscriptionModelMenu(providerID)
}

func subscriptionModelMenu(providerID string) map[string]any {
	subscription := subscriptionForProvider(providerID)
	models := llm.SubscriptionModels(subscription)
	if len(models) == 0 {
		return map[string]any{"inline_keyboard": [][]map[string]string{
			{{"text": "◀️ Back", "callback_data": "menu:" + providerID}},
		}}
	}
	label := subscriptionLabel(providerID)
	active := selection().ActiveProvider
	rows := make([][]map[string]string, 0, len(models)/2+2)
	for i, model := range models {
		text := model
		if active == subscription && currentModel() == model {
			text = "✅ " + model
		}
		_ = i
		rows = append(rows, []map[string]string{{"text": text, "callback_data": "provider:" + providerID + ":model:" + model}})
	}
	rows = append(rows, []map[string]string{{"text": "◀️ Back", "callback_data": "menu:" + providerID}})
	_ = label
	return map[string]any{"inline_keyboard": rows}
}

// subscriptionForProvider maps a profile id to its subscription marker.
func subscriptionForProvider(providerID string) string {
	switch providerID {
	case "codex":
		return llm.CodexSubscription
	case "hermes":
		return llm.HermesSubscription
	case "xai":
		return llm.XAISubscription
	case "minimax":
		return llm.MiniMaxSubscription
	case "cline":
		return llm.ClineSubscription
	case "opencode":
		return llm.OpenCodeSubscription
	case "deepseek":
		return llm.DeepSeekSubscription
	default:
		return ""
	}
}

func subscriptionLabel(providerID string) string {
	switch providerID {
	case "codex":
		return "Codex"
	case "hermes":
		return "Hermes"
	case "xai":
		return "xAI"
	case "minimax":
		return "MiniMax"
	case "cline":
		return "Cline"
	case "opencode":
		return "OpenCode Zen"
	default:
		return providerID
	}
}

func currentModel() string { return selection().Model }

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

// CodexMenuForTest exposes a provider screen for routing tests.
func CodexMenuForTest(authenticated bool) map[string]any { return CodexMenu(authenticated, "") }

// SubscriptionMenuForTest exposes subscriptionMenu to tests in another package.
//
// signedIn and active are passed rather than probed, because the interesting
// branches are the ones a signed-out environment never renders and a test that
// probed the real stores could only ever see the sign-in prompt.
func SubscriptionMenuForTest(label, providerID string, signedIn, active bool) map[string]any {
	return subscriptionMenu(label, "", signedIn, active, "provider:"+providerID)
}

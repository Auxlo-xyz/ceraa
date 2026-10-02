package telegram

import (
	"ceraa/harness/llm"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/harness/coordinator"
	"ceraa/harness/memory"
	"ceraa/harness/scheduler"
	"ceraa/harness/search"
	"ceraa/harness/sessionstore"
	"ceraa/harness/skills"
	"ceraa/harness/snapshot"
	"ceraa/harness/todo"
	"ceraa/harness/tool"
	"ceraa/internal/config"
	"ceraa/internal/logger"
	"ceraa/internal/miniapp"
	"ceraa/internal/opencodeauth"
	"ceraa/internal/secret"
	"ceraa/internal/telegram/menus"
)

func commandCatalog() []menus.CommandSpec {
	return menus.Commands()
}

type providerDraft struct {
	URL   string
	Model string
	ID    string
	Stage string
}

var pendingMemory sync.Map
var pendingModelSearch sync.Map
var pendingProvider sync.Map
var pendingSchedule sync.Map

func commandNames() []map[string]string {
	catalog := commandCatalog()
	out := make([]map[string]string, 0, len(catalog))
	for _, command := range catalog {
		name := strings.ReplaceAll(command.Command, "-", "_")
		out = append(out, map[string]string{"command": name, "description": command.Description})
	}
	return out
}
func configuredStore() (*sessionstore.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return sessionstore.New(cfg.SessionFile)
}

func currentSessionID(chatID int64) string {
	return defaultState.sessionID(chatID)
}

func startNewSession(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed to create session: "+err.Error())
		return
	}
	id := fmt.Sprintf("tg-%d", time.Now().UnixNano())
	if err := store.Save(&sessionstore.Session{ID: id, Version: 2}); err != nil {
		send(token, chatID, "Failed to create session: "+err.Error())
		return
	}
	if err := defaultState.setSession(chatID, id); err != nil {
		send(token, chatID, "Session created, but state save failed: "+err.Error())
		return
	}
	send(token, chatID, "🆕 New session started. Send a task to name it automatically.")
}

// menuRoot is the screen a submenu should return to.
//
// name is the menu to re-render, not just the markup that was drawn. Back used to
// replay the stored text and keyboard verbatim, which froze the screen at the state
// it had when it was first opened: a hub opened while one provider was active went
// on showing that provider after switching to another, and re-rendering was
// impossible because the stored copy had no idea which menu it was. A restart
// cleared the map, which is why it looked like it needed one.
type menuRoot struct {
	name   string
	text   string
	markup map[string]any
}

var menuTriggerPending sync.Map
var menuTriggerByMessage sync.Map
var menuRootByMessage sync.Map

func menuTriggerKey(chatID int64, messageID int) string {
	return fmt.Sprintf("%d:%d", chatID, messageID)
}

func setMenuTriggerPending(chatID int64, messageID int) {
	if messageID > 0 {
		menuTriggerPending.Store(chatID, messageID)
	}
}

func menuTriggerForMessage(chatID int64, messageID int) int {
	value, ok := menuTriggerByMessage.Load(menuTriggerKey(chatID, messageID))
	if !ok {
		return 0
	}
	triggerID, _ := value.(int)
	return triggerID
}

func menuRootForMessage(chatID int64, messageID int) (menuRoot, bool) {
	value, ok := menuRootByMessage.Load(menuTriggerKey(chatID, messageID))
	if !ok {
		return menuRoot{}, false
	}
	root, _ := value.(menuRoot)
	return root, true
}

func sendMenuMarkup(token string, chatID int64, text string, markup map[string]any) {
	sendNamedMenuMarkup(token, chatID, "", text, markup)
}

// sendNamedMenuMarkup sends a menu and remembers which menu it was, so returning to
// it re-renders from current state. An empty name keeps the stored copy, which is
// the right fallback for a one-off keyboard that is not a menu at all.
func sendNamedMenuMarkup(token string, chatID int64, name, text string, markup map[string]any) {
	messageID := sendMessageWithMarkup(token, chatID, text, markup)
	if messageID == 0 {
		return
	}
	menuRootByMessage.Store(menuTriggerKey(chatID, messageID), menuRoot{name: name, text: text, markup: markup})
	if trigger, ok := menuTriggerPending.LoadAndDelete(chatID); ok {
		menuTriggerByMessage.Store(menuTriggerKey(chatID, messageID), trigger)
	}
}

func handleModelCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		sendMenuMarkup(token, chatID, "Select a model", menusInline("model"))
		return
	}
	if err := defaultState.setModel(parts[1]); err != nil {
		send(token, chatID, "Failed to set model: "+err.Error())
		return
	}
	send(token, chatID, "✅ Model set to "+parts[1])
}

func handleAgentCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		sendMenuMarkup(token, chatID, "Select an agent", menusInline("agent"))
		return
	}
	agent := parts[1]
	if agent != "build" && agent != "plan" {
		send(token, chatID, "Agent must be build or plan")
		return
	}
	if err := defaultState.setAgent(agent); err != nil {
		send(token, chatID, "Failed to set agent: "+err.Error())
		return
	}
	send(token, chatID, "✅ Agent set to "+agent)
}

func handleProjectCommand(token string, chatID int64, text string) {
	path := strings.TrimSpace(strings.TrimPrefix(text, "/project"))
	if path == "" {
		sendMenuMarkup(token, chatID, "Select a project", menus.InlineForChat("project", 0, chatID, currentSessionID(chatID)))
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(configuredWorkspaceRoot(), path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		send(token, chatID, "Project must be an existing directory")
		return
	}
	if err := defaultState.setProject(chatID, path); err != nil {
		send(token, chatID, "Failed to set project: "+err.Error())
		return
	}
	send(token, chatID, "✅ Project set to "+defaultState.project(chatID))
}

func handleToolsCommand(token string, chatID int64) {
	var lines []string
	for _, registered := range tool.DefaultRegistry().All() {
		description := registered.Description()
		if len(description) > 140 {
			description = description[:140] + "..."
		}
		lines = append(lines, fmt.Sprintf("🔧 `%s` — %s", registered.Name(), description))
	}
	if len(lines) == 0 {
		send(token, chatID, "No tools registered")
		return
	}
	text := "Available tools:\n" + strings.Join(lines, "\n")
	sendMenuMarkup(token, chatID, text, menus.Inline("tools", 0))
}

func handleTodosCommand(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := todo.New(cfg.SessionFile)
	if err != nil {
		send(token, chatID, "Todo store error: "+err.Error())
		return
	}
	items, err := store.Load(currentSessionID(chatID))
	if err != nil {
		send(token, chatID, "No todos available")
		return
	}
	if len(items) == 0 {
		send(token, chatID, "No todos in this session")
		return
	}
	var lines []string
	for i, item := range items {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, item.Status, item.Content))
	}
	text := "Todos:\n" + strings.Join(lines, "\n")
	sendMenuMarkup(token, chatID, text, menus.InlineForChat("todos", 0, chatID, currentSessionID(chatID)))
}

// statusReport builds the single report behind /status, /context and the hub's
// Status button.
//
// These were three separate paths producing three different bodies for one menu:
// /status printed session and config facts, /context printed token and cache
// facts, and the Status button printed nothing at all, just the menu's name. All
// three attached the same keyboard, so the menu looked identical while the content
// underneath it changed. One report, three entry points.
func statusReport(chatID int64) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("config error: %w", err)
	}
	store, err := configuredStore()
	if err != nil {
		return "", fmt.Errorf("session store error: %w", err)
	}
	tokens, messages := 0, 0
	// cacheRead is a rendered string, not a number. The provider did not always
	// report counts that describe the request, and 0% is a claim while "unknown"
	// is the truth, so the two must not share a field.
	cacheRead := "unavailable (provider token counts do not describe this request)"
	sessionTitle := "Untitled session"
	// Cumulative usage across the whole session: billing, not a single request.
	// Input comes from the runs and output from the messages, because input is the
	// figure a provider has been observed misreporting and the runs are where we
	// checked it. sessionInputTokens holds that rule.
	var usageInput, usageOutput, usageReasoning int
	// inputNote is empty unless part of the input total had to be measured rather
	// than taken from the provider, in which case the line says so instead of
	// quietly presenting our own count as the provider's.
	inputNote := ""
	measuredRuns := 0
	if sess, err := store.Load(currentSessionID(chatID)); err == nil {
		messages = len(sess.Messages)
		sessionTitle = sess.DisplayTitle()
		for _, message := range sess.Messages {
			tokens += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
			usageOutput += message.Usage.OutputTokens
			usageReasoning += message.Usage.ReasoningTokens
		}
		// Input is summed from runs, not messages, because the runs are where the
		// provider's claim was reconciled against our own count. Output and
		// reasoning stay on messages: neither has been observed misreporting, and
		// the run total for them is the same number.
		var measured int
		usageInput, measured = sessionInputTokens(sess)
		measuredRuns = measured
		if measuredRuns > 0 {
			inputNote = fmt.Sprintf("\nInput is Ceraa's own measurement: %d of %d runs reported counts that do not describe the request",
				measuredRuns, len(sess.Runs))
		}
		if len(sess.Runs) > 0 {
			// Only report a cache percentage when the provider's token counts
			// describe this request. Some endpoints report a running total, and
			// dividing that by itself produces a ratio that drifts toward 1 and
			// looks like excellent caching while describing nothing.
			last := sess.Runs[len(sess.Runs)-1]
			if last.UsageTrust.Trusted {
				cacheRead = fmt.Sprintf("%d%%", int(last.Usage.CacheReadRatioFor(last.UsageTrust)*100))
			}
		}
	}
	// The base config is not what is serving this session: a provider activated
	// from the menu lives in a profile, so reading cfg.LLM here reported an empty
	// model and the wrong context limit, which is worse than showing nothing.
	model := cfg.LLM.Model
	contextLimit := cfg.LLM.ContextLimit
	if _, profile, ok := defaultState.activeProviderProfile(); ok {
		if strings.TrimSpace(profile.Model) != "" {
			model = profile.Model
		}
		if profile.ContextLimit > 0 {
			contextLimit = profile.ContextLimit
		}
	}
	activeProvider := defaultState.provider()
	if contextLimit <= 0 {
		contextLimit = displayContextLimit(cfg)
	}
	percent := 0
	if contextLimit > 0 {
		percent = tokens * 100 / contextLimit
	}
	if strings.TrimSpace(model) == "" {
		model = "unknown"
	}
	// The session total is the honest headline, and the estimate beside it is
	// explicitly labelled: the estimate is a character-count heuristic over stored
	// text, while the total is what the provider actually reported.
	//
	// Agent is included because the old /status showed it and the old /context did
	// not, and picking either report meant losing facts the other had. Endpoint is
	// reduced to a host: the full URL carries a path that is noise here and a key in
	// the case of some providers.
	_, agent := defaultState.values()
	if agent == "" {
		agent = "build"
	}
	endpoint := providerHost(activeProviderURL(cfg))
	// "input" is the uncached count: Run.Usage reports the prompt total, and the
	// cached part of it is billed at roughly a sixth of the price. Labelling it
	// saved a misreading, because the same number next to a 92% cache rate looks
	// like a cache that is not working. The count itself is the run-based one, so
	// a rejected provider figure is replaced by our own measurement rather than
	// printed.
	report := fmt.Sprintf("📊 Status\n~%s / %s tokens (%d%%) estimated\n%s uncached input / %s output / %s reasoning reported\nCache read: %s%s\nSession: %s\nProject: `%s`\nMessages: %d\nProvider: %s\nModel: `%s`\nEndpoint: %s\nAgent: %s",
		groupDigits(tokens), groupDigits(contextLimit), percent,
		groupDigits(usageInput), groupDigits(usageOutput), groupDigits(usageReasoning),
		cacheRead, inputNote, sessionTitle, defaultState.project(chatID), messages, activeProvider, model,
		endpoint, agent)
	// The tool-output cap is the one cost control that can be tuned, so it is
	// reported with the figure that chooses it: what a lower cap would have saved.
	return report + "\n\n" + coordinator.ToolOutputStatsLine(cfg.MaxToolResultChars), nil
}

// handleContextCommand shows the status report. Kept as a distinct command because
// it is what people reach for when they mean "how full is the context", but it is
// the same report, so the two cannot drift apart again.
// handleCacheCommand reports cache behaviour per endpoint, which is the only way
// to tell a route that caches from one that does not: a session mixes providers
// and an aggregate hides which one is at fault.
func handleCacheCommand(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed to open sessions: "+err.Error())
		return
	}
	sess, err := store.Load(currentSessionID(chatID))
	if err != nil {
		send(token, chatID, "Failed to load session: "+err.Error())
		return
	}
	send(token, chatID, cacheReport(sess))
}

func handleContextCommand(token string, chatID int64) {
	text, err := statusReport(chatID)
	if err != nil {
		send(token, chatID, err.Error())
		return
	}
	sendMenuMarkup(token, chatID, text, menus.InlineForSession("status", 0, currentSessionID(chatID)))
}

// sessionInputTokens totals the session's input tokens and reports how many runs
// had to be measured rather than believed.
//
// Summing message usage adds up every figure the session ever stored, including
// the ones reconciliation already rejected. One live session had a provider
// reporting a running total, and the naive sum turned 66,107 tokens actually sent
// into 4,111,306 reported, which is the number this line presents as what the
// session cost.
//
// An untrusted run contributes its own measurement instead, so the total stays a
// real count rather than falling to zero. A run stored before trust existed has
// no measurement and keeps whatever it reported, because a missing verdict on old
// data is not evidence that the data was wrong.
func sessionInputTokens(sess *sessionstore.Session) (total, measured int) {
	if sess == nil {
		return 0, 0
	}
	if len(sess.Runs) == 0 {
		for _, message := range sess.Messages {
			total += message.Usage.InputTokens
		}
		return total, 0
	}
	for _, run := range sess.Runs {
		if !run.UsageTrust.Trusted && run.UsageTrust.LocalInput > 0 {
			total += run.UsageTrust.LocalInput
			measured++
			continue
		}
		total += run.Usage.InputTokens
	}
	return total, measured
}

// groupDigits renders an integer with thousands separators, so a seven-figure
// token count is readable at a glance in a chat bubble.
func groupDigits(n int) string {
	if n < 0 {
		return "-" + groupDigits(-n)
	}
	digits := strconv.Itoa(n)
	if len(digits) <= 3 {
		return digits
	}
	var out strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		out.WriteString(digits[:lead])
	}
	for index := lead; index < len(digits); index += 3 {
		if out.Len() > 0 {
			out.WriteByte(',')
		}
		out.WriteString(digits[index : index+3])
	}
	return out.String()
}

func handleSearchCommand(token string, chatID int64, text string) {
	query := strings.TrimSpace(strings.TrimPrefix(text, "/search"))
	if query == "" {
		sendMenuMarkup(token, chatID, "Search project", menusInline("search"))
		return
	}
	project := defaultState.project(chatID)
	if project == "" {
		send(token, chatID, "Choose a project before searching")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	idx, err := search.Open(search.ProjectIndexPath(cfg.WorkspaceRootPath(), project))
	if err != nil {
		send(token, chatID, "Search index error: "+err.Error())
		return
	}
	defer idx.Close()
	if _, err := idx.IndexDir(project); err != nil {
		send(token, chatID, "Index error: "+err.Error())
		return
	}
	results, err := idx.Search(query, 10)
	if err != nil {
		send(token, chatID, "Search error: "+err.Error())
		return
	}
	if len(results) == 0 {
		send(token, chatID, "No matches for "+query)
		return
	}
	var lines []string
	for i, result := range results {
		lines = append(lines, fmt.Sprintf("%d. `%s` (rank %.2f)", i+1, result.Path, result.Rank))
	}
	send(token, chatID, "Search results:\n"+strings.Join(lines, "\n"))
}

func handleIndexCommand(token string, chatID int64, text string) {
	project := defaultState.project(chatID)
	if project == "" {
		send(token, chatID, "Choose a project before indexing")
		return
	}
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	idx, err := search.Open(search.ProjectIndexPath(cfg.WorkspaceRootPath(), project))
	if err != nil {
		send(token, chatID, "Index error: "+err.Error())
		return
	}
	defer idx.Close()
	count, err := idx.IndexDir(project)
	if err != nil {
		send(token, chatID, "Index error: "+err.Error())
		return
	}
	send(token, chatID, fmt.Sprintf("🗃️ Indexed %d files from %s", count, project))
}

func handleCatalogCommand(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}
	switch strings.TrimPrefix(parts[0], "/") {
	case "skills_catalog":
		handleToolsCommand(token, chatID)
	default:
		send(token, chatID, "Unknown catalog")
	}
}

func handleSnapshotsCommand(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := snapshot.New(filepath.Join(cfg.SessionFile, ".snapshots"))
	if err != nil {
		send(token, chatID, "Snapshot store error: "+err.Error())
		return
	}
	entries, err := store.Entries()
	if err != nil {
		send(token, chatID, "Snapshot error: "+err.Error())
		return
	}
	if len(entries) == 0 {
		send(token, chatID, "No snapshots yet. File writes create snapshots automatically.")
		return
	}
	var lines []string
	for i, entry := range entries {
		if i >= 10 {
			break
		}
		state := "existing"
		if !entry.Exists {
			state = "new file"
		}
		lines = append(lines, fmt.Sprintf("%d. `%s` (%s, %s)", i+1, entry.Path, state, entry.Timestamp.Format("2006-01-02 15:04")))
	}
	send(token, chatID, "Snapshots:\n"+strings.Join(lines, "\n")+"\n\nUse /revert <path> to restore the latest snapshot.")
}

func handleRevertCommand(token string, chatID int64, text string) {
	path := strings.TrimSpace(strings.TrimPrefix(text, "/revert"))
	if path == "" {
		send(token, chatID, "Usage: /revert <path>")
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(defaultState.project(chatID), path)
	}
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := snapshot.New(filepath.Join(cfg.SessionFile, ".snapshots"))
	if err != nil {
		send(token, chatID, "Snapshot store error: "+err.Error())
		return
	}
	entry, err := store.RevertLatest(path)
	if err != nil {
		send(token, chatID, "Revert failed: "+err.Error())
		return
	}
	send(token, chatID, "↩️ Restored "+entry.Path)
}

func handlePermissionCommand(token string, chatID int64) {
	sendMenuMarkup(token, chatID, "Permission controls", menusInline("permission"))
}

func menusInline(name string) map[string]any {
	return menusInlineForSession(name, "")
}

func menusInlineForSession(name, sessionID string) map[string]any {
	return menus.InlineForSession(name, 0, sessionID)
}

func handleSessions(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed to open sessions: "+err.Error())
		return
	}
	ids, err := store.List()
	if err != nil {
		send(token, chatID, "Failed to list sessions: "+err.Error())
		return
	}
	if len(ids) == 0 {
		send(token, chatID, "No sessions yet. Use /new to start.")
		return
	}
	var lines []string
	for i, id := range ids {
		sess, err := store.Load(id)
		if err != nil || sess.ID != id || id == "inbox" {
			continue
		}
		if len(sess.Messages) > 0 && (sess.Title == "" || (sess.TitleSource == "fallback" && sess.TitleAttempts < 3)) {
			scheduleSessionTitle(id)
		}
		if i >= 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(ids)-10))
			break
		}
		lines = append(lines, fmt.Sprintf("• %s (%d messages)", sess.DisplayTitle(), len(sess.Messages)))
	}
	text := "Sessions:\n" + strings.Join(lines, "\n")
	sendMenuMarkup(token, chatID, text, menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
}

// handleOpenSession loads a session by ID -> maps to soraacode session.ts get
func handleOpenSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /open <session_id>\nList with /sessions")
		return
	}
	id := parts[1]
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	sess, err := store.Load(id)
	if err != nil {
		send(token, chatID, "Session not found: "+id)
		return
	}
	if err := defaultState.setSession(chatID, sess.ID); err != nil {
		send(token, chatID, "Session opened, but state save failed: "+err.Error())
		return
	}
	send(token, chatID, fmt.Sprintf("Opened session %s (%d messages)", sess.DisplayTitle(), len(sess.Messages)))
}

// handleMessages shows recent messages -> maps to soraacode session/history.ts
func handleMessages(token string, chatID int64) {
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	sess, err := store.Load(currentSessionID(chatID))
	if err != nil {
		send(token, chatID, "No session yet.")
		return
	}
	if len(sess.Messages) == 0 {
		send(token, chatID, "No messages in current session.")
		return
	}
	start := len(sess.Messages) - 5
	if start < 0 {
		start = 0
	}
	var lines []string
	for _, m := range sess.Messages[start:] {
		role := string(m.Role)
		if m.ToolCall != nil {
			role = "tool:" + m.ToolCall.Name
		} else if m.ToolCallID != "" {
			role = "tool_result"
		}
		preview := m.Content
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		if preview == "" && m.ToolResult != "" {
			preview = m.ToolResult
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
		}
		lines = append(lines, fmt.Sprintf("**%s**: %s", role, preview))
	}
	send(token, chatID, "Recent messages:\n"+strings.Join(lines, "\n"))
}

// handleListFiles glob/list directory -> maps to soraacode filesystem.ts + glob.ts
func handleListFiles(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	path := defaultState.project(chatID)
	if len(parts) >= 2 {
		path = parts[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(defaultState.project(chatID), path)
		}
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			send(token, chatID, "Read failed: "+err.Error())
			return
		}
		content := string(data)
		if len(content) > 3500 {
			content = content[:3500] + "...[truncated]"
		}
		send(token, chatID, "```\n"+content+"\n```")
		return
	}
	// List directory
	entries, err := os.ReadDir(path)
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	if len(entries) == 0 {
		send(token, chatID, "Empty: "+path)
		return
	}
	var lines []string
	for i, e := range entries {
		if i >= 30 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(entries)-30))
			break
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		lines = append(lines, e.Name()+suffix)
	}
	send(token, chatID, "Files in `"+path+"`:\n"+strings.Join(lines, "\n"))
}

// handleStatus shows harness status -> maps to soraacode session/status.ts + llm/route
func handleStatus(token string, chatID int64) {
	text, err := statusReport(chatID)
	if err != nil {
		send(token, chatID, err.Error())
		return
	}
	sendMenuMarkup(token, chatID, text, menus.InlineForSession("status", 0, currentSessionID(chatID)))
}

// handleSettings shows config -> maps to soraacode config.ts
func handleSettings(token string, chatID int64) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	providers := make([]string, 0, len(cfg.Subscriptions.Providers))
	for name, provider := range cfg.Subscriptions.Providers {
		kind := provider.Kind
		if kind == "" {
			kind = "configured"
		}
		providers = append(providers, name+":"+kind)
	}
	provider := activeProviderConfig(cfg, defaultState.provider())
	_ = provider
	text := fmt.Sprintf("⚙️ Safe settings\n• Provider: %s\n• Model: %s\n• Endpoint: %s\n• Context limit: %d\n• Max output: %d\n• Session store: %s\n• Telegram user: configured\n• Subscriptions: %s", defaultState.provider(), provider.Model, provider.URL, provider.ContextLimit, provider.MaxOutputTokens, cfg.SessionFile, strings.Join(providers, ", "))
	sendMessageWithMarkup(token, chatID, text, map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "🧠 Model", "callback_data": "menu:model"}},
		{{"text": "🧮 Context", "callback_data": "menu:context"}, {"text": "✕ Close", "callback_data": "close"}},
	}})
}

// handleDeleteSession deletes a session -> maps to soraacode session.ts delete
func handleDeleteSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		send(token, chatID, "Usage: /delete <session_id>")
		return
	}
	id := parts[1]
	store, err := configuredStore()
	if err != nil {
		send(token, chatID, "Failed: "+err.Error())
		return
	}
	// Clear session
	sess, err := store.Load(id)
	if err != nil {
		send(token, chatID, "Session not found: "+id)
		return
	}
	if err := sess.Clear(); err != nil {
		send(token, chatID, "Delete failed: "+err.Error())
		return
	}
	cfg, err := config.Load()
	if err == nil {
		_ = os.Remove(filepath.Join(cfg.SessionFile, id+".json"))
	}
	send(token, chatID, "Deleted session "+id)
}

// handleRenameSession renames session file -> maps to soraacode rename-manager.js
func handleRenameSession(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		send(token, chatID, "Usage: /rename <old_id> <new_id>")
		return
	}
	oldID := parts[1]
	newID := parts[2]
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	oldPath := filepath.Join(cfg.SessionFile, oldID+".json")
	newPath := filepath.Join(cfg.SessionFile, newID+".json")
	if _, err := os.Stat(oldPath); err != nil {
		send(token, chatID, "Session not found: "+oldID)
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		send(token, chatID, "Rename failed: "+err.Error())
		return
	}
	if currentSessionID(chatID) == oldID {
		_ = defaultState.setSession(chatID, newID)
	}
	send(token, chatID, fmt.Sprintf("Renamed %s -> %s", oldID, newID))
}

// handleAbort cancels running operations -> maps to soraacode session/interrupt.ts + run-control
func handleAbort(token string, chatID int64) {
	if stopped, sessionID := stopActiveRun(chatID); stopped {
		note := ""
		if sessionID != "" {
			note = " (" + sessionID + ")"
		}
		send(token, chatID, "🛑 Current run cancelled"+note)
		return
	}
	send(token, chatID, "No active run to cancel")
}

// handleCompaction shows compaction level -> maps to soraacode session/compaction.ts + overflow.ts + context-epoch.ts
func handleCompaction(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) == 1 {
		cfg, _ := config.Load()
		store, _ := configuredStore()
		tokens, compactions := 0, 0
		if store != nil {
			if sess, err := store.Load(currentSessionID(chatID)); err == nil {
				for _, message := range sess.Messages {
					tokens += (len(message.Content) + len(message.ToolResult) + len(message.Reasoning)) / 4
					if message.Compaction != nil {
						compactions++
					}
				}
			}
		}
		auto, configured := defaultState.autoCompaction()
		if !configured {
			auto = true
		}
		limit := displayContextLimit(cfg)
		text := fmt.Sprintf("🗜️ Compaction status\n• Tokens: ~%d / %d\n• Auto: %t\n• Compactions: %d\n• Keep: 8k recent tokens", tokens, limit, auto, compactions)
		sendMenuMarkup(token, chatID, text, menus.Inline("compaction_level", 0))
		return
	}
	level := parts[1]
	if level == "auto" && len(parts) >= 3 {
		enabled := parts[2] == "on" || parts[2] == "true"
		if parts[2] != "on" && parts[2] != "off" && parts[2] != "true" && parts[2] != "false" {
			send(token, chatID, "Usage: /compaction auto on|off")
			return
		}
		if err := defaultState.setAutoCompaction(enabled); err != nil {
			send(token, chatID, "Failed to save compaction setting: "+err.Error())
			return
		}
		send(token, chatID, fmt.Sprintf("✅ Automatic compaction %s", map[bool]string{true: "enabled", false: "disabled"}[enabled]))
		return
	}
	if level == "compact" || level == "now" {
		requestForcedCompaction(currentSessionID(chatID))
		send(token, chatID, "🗜️ Compaction queued for the next prompt in this session")
		return
	}
	send(token, chatID, "Usage: /compaction [now|auto on|auto off]")
}

// handleWorktree handles git worktree -> maps to soraacode worktree-service.js
func handleWorktree(token string, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) == 1 || parts[1] == "list" {
		output, err := exec.Command("git", "worktree", "list").CombinedOutput()
		if err != nil {
			send(token, chatID, "Worktree lookup failed: "+err.Error())
			return
		}
		send(token, chatID, strings.TrimSpace(string(output)))
		return
	}
	send(token, chatID, "Usage: /worktree [list]")
}

func handlePendingInput(token string, chatID int64, text string) bool {
	if value, ok := pendingTitleSetting.LoadAndDelete(chatID); ok {
		if value.(string) == "model" {
			if err := saveTitleModel(text); err != nil {
				send(token, chatID, "Model save failed: "+err.Error())
				return true
			}
			sendMenuMarkup(token, chatID, "✅ Title model updated", menus.InlineForChat("title_settings", 0, chatID, currentSessionID(chatID)))
		} else {
			if err := saveTitleKey(text); err != nil {
				send(token, chatID, "API key save failed: "+err.Error())
				return true
			}
			sendMenuMarkup(token, chatID, "🔑 Gemini API key saved securely", menus.InlineForChat("title_settings", 0, chatID, currentSessionID(chatID)))
		}
		return true
	}
	if value, ok := pendingSessionAction.LoadAndDelete(chatID); ok {
		action := value.(sessionAction)
		if action.Action == "rename" {
			if err := renameSession(action.ID, text); err != nil {
				send(token, chatID, "Rename failed: "+err.Error())
				return true
			}
			sendMenuMarkup(token, chatID, "✏️ Session renamed", menus.InlineForChat("session", 0, chatID, currentSessionID(chatID)))
		}
		return true
	}
	if _, ok := pendingModelSearch.LoadAndDelete(chatID); ok {
		sendMenuMarkup(token, chatID, "🔎 Model search", menus.ModelCatalogForChat(chatID, text))
		return true
	}
	if value, ok := pendingProvider.Load(chatID); ok {
		draft := value.(providerDraft)
		switch draft.Stage {
		case "url":
			if !strings.HasPrefix(text, "http://") && !strings.HasPrefix(text, "https://") {
				logger.Warn("provider onboarding rejected a URL that is not http(s)")
				send(token, chatID, "Provider URL must start with http:// or https://")
				return true
			}
			logger.Info("provider onboarding: url accepted")
			draft.URL = text
			draft.Stage = "model"
			pendingProvider.Store(chatID, draft)
			logger.Info("provider onboarding: awaiting model id")
			send(token, chatID, "Send the model ID for the custom provider.")
			return true
		case "model":
			if strings.TrimSpace(text) == "" {
				send(token, chatID, "Model ID cannot be empty.")
				return true
			}
			draft.Model = text
			if draft.ID == llm.HermesSubscription {
				pendingProvider.Delete(chatID)
				profile := config.ProviderProfile{Model: draft.Model, Subscription: llm.HermesSubscription}
				if err := defaultState.setProviderProfile(draft.ID, profile); err != nil {
					send(token, chatID, "Provider profile save failed: "+err.Error())
					return true
				}
				if err := defaultState.setProvider(draft.ID); err != nil {
					send(token, chatID, "Provider activation failed: "+err.Error())
					return true
				}
				send(token, chatID, "🔐 Hermes subscription selected. Make sure Hermes is logged in, then send a task.")
				return true
			}
			draft.Stage = "key"
			pendingProvider.Store(chatID, draft)
			logger.Info("provider onboarding: model accepted, awaiting api key")
			send(token, chatID, "Send the API key for "+draft.ID+".")
			return true
		case "key":
			if strings.TrimSpace(text) == "" {
				send(token, chatID, "API key cannot be empty.")
				return true
			}
			pendingProvider.Delete(chatID)
			// A provider onboarded straight to the key stage never collected a
			// model, so it has none to verify or to save. Zen serves a fixed
			// catalog and the default is the one the picker shows first, which
			// is the same choice the user would have made from the list.
			if draft.Model == "" {
				draft.Model = llm.DefaultOpenCodeModel
			}
			// Prove the base URL, model, and key work together before storing
			// them, so a bad combination is reported here rather than on the
			// user's first real task.
			verifyErr := verifyProvider(context.Background(), draft.URL, text, draft.Model)
			if draft.ID == llm.OpenCodeSubscription {
				// Zen expects a header contract on every request, so the check
				// sends it too. Verifying with a bare Authorization header would
				// be a different request from the one the transport makes.
				verifyErr = verifyProviderWithHeaders(context.Background(), draft.URL, text, draft.Model,
					opencodeauth.RequestHeaders("verify", ""))
			}
			if verifyErr != nil {
				logger.Warn("provider onboarding verification failed for " + draft.ID + ": " + verifyErr.Error())
				send(token, chatID, "❌ That provider did not answer: "+verifyErr.Error()+"\n\nNothing was saved. Fix what is described above and start over.")
				return true
			}
			logger.Info("provider onboarding: verified " + draft.ID)
			profile := config.ProviderProfile{URL: draft.URL, Model: draft.Model}
			if draft.ID == llm.OpenCodeSubscription {
				// The key goes into the encrypted store beside the OAuth tokens
				// rather than into provider_api_key_, because that is where
				// opencodeauth reads it and because the store is encrypted while
				// the profile field is not.
				if err := opencodeauth.Save(opencodeauth.Credential{APIKey: text}); err != nil {
					send(token, chatID, "OpenCode credential save failed: "+err.Error())
					return true
				}
				profile.Subscription = llm.OpenCodeSubscription
			} else {
				if err := secret.Set("provider_api_key_"+draft.ID, text); err != nil {
					send(token, chatID, "Provider key save failed: "+err.Error())
					return true
				}
			}
			if err := defaultState.setProviderProfile(draft.ID, profile); err != nil {
				send(token, chatID, "Provider profile save failed: "+err.Error())
				return true
			}
			if err := defaultState.setProvider(draft.ID); err != nil {
				send(token, chatID, "Provider activation failed: "+err.Error())
				return true
			}
			send(token, chatID, "✅ "+draft.ID+" is configured. Choose another control or send a task.")
			return true
		}
	}
	if _, ok := pendingMemory.LoadAndDelete(chatID); ok {
		cfg, err := config.Load()
		if err != nil {
			send(token, chatID, "Config error: "+err.Error())
			return true
		}
		store, err := memory.New(cfg.MemoryFile)
		if err != nil {
			send(token, chatID, "Memory store error: "+err.Error())
			return true
		}
		entry, err := store.Add(defaultState.project(chatID), currentSessionID(chatID), text)
		if err != nil {
			send(token, chatID, "Memory save failed: "+err.Error())
			return true
		}
		send(token, chatID, "Memory saved: "+entry.ID)
		return true
	}
	if value, ok := pendingSchedule.LoadAndDelete(chatID); ok {
		interval, valid := value.(time.Duration)
		if !valid || interval < time.Minute {
			return true
		}
		cfg, err := config.Load()
		if err != nil {
			send(token, chatID, "Config error: "+err.Error())
			return true
		}
		store, err := scheduler.New(cfg.SchedulerFile)
		if err != nil {
			send(token, chatID, "Scheduler store error: "+err.Error())
			return true
		}
		job := scheduler.Job{ID: fmt.Sprintf("job_%d", time.Now().UnixNano()), Name: "scheduled task", Prompt: text, SessionID: currentSessionID(chatID), ChatID: chatID, Project: defaultState.project(chatID), Interval: interval}
		if err := store.Add(job); err != nil {
			send(token, chatID, "Schedule add failed: "+err.Error())
			return true
		}
		send(token, chatID, "Scheduled job added: "+job.ID)
		return true
	}
	return false
}

func handleMemoryCommand(token string, chatID int64, text string) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := memory.New(cfg.MemoryFile)
	if err != nil {
		send(token, chatID, "Memory store error: "+err.Error())
		return
	}
	project := defaultState.project(chatID)
	sessionID := currentSessionID(chatID)
	parts := strings.Fields(text)
	if len(parts) == 1 {
		sendMenuMarkup(token, chatID, "🧠 Compact memory", menus.InlineForChat("memory", 0, chatID, sessionID))
		return
	}
	if len(parts) >= 2 && parts[1] == "add" {
		content := strings.TrimSpace(strings.TrimPrefix(text, "/memory add"))
		if content == "" {
			send(token, chatID, "Usage: /memory add <compact fact>")
			return
		}
		entry, err := store.Add(project, sessionID, content)
		if err != nil {
			send(token, chatID, "Memory save failed: "+err.Error())
			return
		}
		send(token, chatID, "Memory saved: "+entry.ID)
		return
	}
	if len(parts) >= 2 && parts[1] == "remove" {
		if err := store.Remove(parts[2]); err != nil {
			send(token, chatID, "Memory removal failed: "+err.Error())
			return
		}
		send(token, chatID, "Memory removed")
		return
	}
	entries, err := store.List(project, sessionID)
	if err != nil {
		send(token, chatID, "Memory load failed: "+err.Error())
		return
	}
	if len(entries) == 0 {
		send(token, chatID, "No compact memory saved for this project/session")
		return
	}
	var lines []string
	for i, entry := range entries {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, entry.Scope, entry.Content))
	}
	send(token, chatID, "Memory:\n"+strings.Join(lines, "\n"))
}

func handleSkillCommand(token string, chatID int64, text string) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := skills.New(cfg.SkillFile)
	if err != nil {
		send(token, chatID, "Skill store error: "+err.Error())
		return
	}
	parts := strings.Fields(text)
	if len(parts) == 1 {
		sendMenuMarkup(token, chatID, "🧩 Skill proposals", menus.InlineForChat("skill", 0, chatID, currentSessionID(chatID)))
		return
	}
	if len(parts) >= 4 && parts[1] == "rollback" {
		version, err := strconv.Atoi(parts[3])
		if err != nil {
			send(token, chatID, "Version must be a number")
			return
		}
		skill, err := store.Rollback(parts[2], version)
		if err != nil {
			send(token, chatID, "Skill rollback failed: "+err.Error())
			return
		}
		send(token, chatID, fmt.Sprintf("Skill %s restored to version %d as version %d", skill.Name, version, len(skill.Versions)))
		return
	}
	if len(parts) >= 2 && (parts[1] == "approve" || parts[1] == "reject") {
		if len(parts) < 3 {
			send(token, chatID, "Usage: /skill approve|reject <proposal_id>")
			return
		}
		if parts[1] == "approve" {
			skill, err := store.Approve(parts[2])
			if err != nil {
				send(token, chatID, "Skill approval failed: "+err.Error())
				return
			}
			send(token, chatID, fmt.Sprintf("Skill %s approved at version %d", skill.Name, len(skill.Versions)))
		} else {
			if err := store.Reject(parts[2]); err != nil {
				send(token, chatID, "Skill rejection failed: "+err.Error())
				return
			}
			send(token, chatID, "Skill proposal rejected")
		}
		return
	}
	proposals, err := store.ListProposals()
	if err != nil {
		send(token, chatID, "Skill store error: "+err.Error())
		return
	}
	var lines []string
	for i, proposal := range proposals {
		lines = append(lines, fmt.Sprintf("%d. %s [%s] %s — %s", i+1, proposal.ID, proposal.Status, proposal.SkillName, proposal.Feedback))
	}
	if len(lines) == 0 {
		send(token, chatID, "No skill proposals. The agent can create one with skill_propose after feedback.")
		return
	}
	send(token, chatID, "Skill proposals:\n"+strings.Join(lines, "\n")+"\n\nApprove with /skill approve <proposal_id>")
}

func handleScheduleCommand(token string, chatID int64, text string) {
	cfg, err := config.Load()
	if err != nil {
		send(token, chatID, "Config error: "+err.Error())
		return
	}
	store, err := scheduler.New(cfg.SchedulerFile)
	if err != nil {
		send(token, chatID, "Scheduler store error: "+err.Error())
		return
	}
	parts := strings.Fields(text)
	if len(parts) == 1 || parts[1] == "list" {
		sendMenuMarkup(token, chatID, "⏰ Scheduled jobs", menus.InlineForChat("schedule", 0, chatID, currentSessionID(chatID)))
		return
	}
	switch parts[1] {
	case "add":
		if len(parts) < 4 {
			send(token, chatID, "Usage: /schedule add <interval like 30m> <prompt>")
			return
		}
		interval, err := time.ParseDuration(parts[2])
		if err != nil || interval < time.Minute {
			send(token, chatID, "Interval must be a duration of at least 1m, for example 30m")
			return
		}
		job := scheduler.Job{
			ID:        fmt.Sprintf("job_%d", time.Now().UnixNano()),
			Name:      "scheduled task",
			Prompt:    strings.Join(parts[3:], " "),
			SessionID: currentSessionID(chatID),
			ChatID:    chatID,
			Project:   defaultState.project(chatID),
			Interval:  interval,
		}
		if err := store.Add(job); err != nil {
			send(token, chatID, "Schedule add failed: "+err.Error())
			return
		}
		send(token, chatID, "Scheduled job added: "+job.ID)
	case "remove":
		if len(parts) < 3 {
			send(token, chatID, "Usage: /schedule remove <job_id>")
			return
		}
		removed, err := store.Remove(parts[2])
		if err != nil {
			send(token, chatID, "Schedule remove failed: "+err.Error())
			return
		}
		if removed {
			send(token, chatID, "Scheduled job removed")
		} else {
			send(token, chatID, "Scheduled job not found")
		}
	default:
		send(token, chatID, "Usage: /schedule [list|add <interval> <prompt>|remove <job_id>]")
	}
}

// handleHelp shows full command list -> maps to soraacode bot/commands/definitions.js
func handleFullHelp(token string, chatID int64) {
	catalog := commandCatalog()
	lines := make([]string, 0, len(catalog)+2)
	lines = append(lines, "Ceraa commands:")
	for _, command := range catalog {
		lines = append(lines, "/"+command.Command+" - "+command.Description)
	}
	lines = append(lines, "", "Send any other text to run the agent.")
	send(token, chatID, strings.Join(lines, "\n"))
}

// handleWebShare gives the admitted user the public address of the Mini App
// surface.
//
// The handshake with the edge takes seconds of wall clock, and the update loop
// is sequential, so only the already-open case is answered inline. Anything
// slower answers twice: one line now so the message is not left hanging, and
// the address when the client has announced one.
func handleWebShare(token string, chatID int64, server *miniapp.Server) {
	if server == nil {
		send(token, chatID, "The Mini App surface is off. Set CERAA_MINIAPP=1 and restart to turn it on.")
		return
	}
	if address := server.TunnelAddress(); address != "" {
		send(token, chatID, "Mini App:\n"+address)
		return
	}
	send(token, chatID, "Opening a tunnel...")
	go func() {
		address, err := server.StartTunnel()
		if err != nil {
			send(token, chatID, "No public address: "+err.Error())
			return
		}
		send(token, chatID, "Mini App:\n"+address)
	}()
}

// displayContextLimit reports the budget in force for a model whose profile does
// not state one.
//
// An unset profile limit means "the catalog decides", so rendering the raw zero
// would tell the user the model has no context window at all, which is both wrong
// and alarming. The runner may have recalibrated from live catalog data by now;
// this mirrors the resolver's fallback for the cases where it has not.
func displayContextLimit(cfg *config.Config) int {
	if cfg != nil {
		if _, profile, ok := defaultState.activeProviderProfile(); ok && profile.ContextLimit > 0 {
			return profile.ContextLimit
		}
		if cfg.LLM.ContextLimit > 0 {
			return cfg.LLM.ContextLimit
		}
	}
	return llm.FallbackContextWindow
}

// activeProviderURL is the endpoint of the provider that is actually serving.
//
// The base config is not what serves a session once a provider has been activated
// from the menu, so reading cfg.LLM reported an empty URL for a working install.
func activeProviderURL(cfg *config.Config) string {
	provider := cfg.ActiveLLM()
	if _, profile, ok := defaultState.activeProviderProfile(); ok && strings.TrimSpace(profile.URL) != "" {
		return profile.URL
	}
	return provider.URL
}

// providerHost reduces a base URL to a host for display.
func providerHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "unknown"
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return trimmed
	}
	return parsed.Host
}

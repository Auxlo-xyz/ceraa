package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/permission"
	"ceraa/harness/sessionstore"
	"ceraa/internal/config"
	"ceraa/internal/miniapp"
)

// A tool toggle that only persisted is a stored intention, not a permission.
// The running manager is seeded from state once at startup, so without this the
// dashboard reported success and the next run still asked.
func TestDashboardToolToggleReachesTheRunningManager(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	permission.Default = permission.NewManager()
	defer func() { permission.Default = permission.NewManager() }()

	if err := webSetToolAllowed("bash", true); err != nil {
		t.Fatalf("allow bash: %v", err)
	}
	// A fresh request for the same tool must short-circuit to allowAlways
	// without anyone answering it. The assertion is the wait: a request that asks
	// for a decision runs out the clock. The timeout is a second rather than the
	// manager's default five minutes so a regression fails in a second instead of
	// hanging the suite for five.
	decision, err := permission.Default.Create(t.Context(), "web", "req-1", "bash", time.Second, nil)
	if err != nil {
		t.Fatalf("an allowed tool still asked for permission: %v", err)
	}
	if decision != "allowAlways" {
		t.Fatalf("decision = %q, want allowAlways", decision)
	}

	if err := webSetToolAllowed("bash", false); err != nil {
		t.Fatalf("deny bash: %v", err)
	}
	if _, err := permission.Default.Create(t.Context(), "web", "req-2", "bash", time.Second, nil); err == nil {
		t.Fatal("clearing the toggle left the tool allowed in the running manager")
	}
	if defaultState.alwaysAllowed("bash") {
		t.Fatal("clearing the toggle left it persisted")
	}
}

// Saving a profile the page can only partly describe must not blank the fields
// the page cannot show. The form carries six of the ten; the cache TTL, the
// subscription and the Hermes provider were being zeroed on every edit.
func TestSavingAProfileKeepsFieldsTheFormCannotShow(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ProviderProfiles = map[string]config.ProviderProfile{
			"house": {
				URL:             "https://old.example/v1",
				APIKey:          "secret",
				Model:           "old-model",
				PromptCacheMode: "auto-prefix",
				PromptCacheTTL:  "1h",
				Subscription:    "chatgpt",
				HermesProvider:  "openai",
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	previous := providers
	providers = newProviderState(store)
	defer func() { providers = previous }()

	if err := webSaveProvider("house", map[string]any{
		"endpoint": "https://new.example/v1",
		"model":    "new-model",
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}

	saved, ok := providers.profiles()["house"]
	if !ok {
		t.Fatal("the profile was not stored")
	}
	if saved.Model != "new-model" || saved.URL != "https://new.example/v1" {
		t.Fatalf("the fields the form carries did not land: %+v", saved)
	}
	if saved.PromptCacheTTL != "1h" {
		t.Errorf("PromptCacheTTL = %q, want 1h: a field the form never sends must survive an edit", saved.PromptCacheTTL)
	}
	if saved.Subscription != "chatgpt" {
		t.Errorf("Subscription = %q, want chatgpt", saved.Subscription)
	}
	if saved.HermesProvider != "openai" {
		t.Errorf("HermesProvider = %q, want openai", saved.HermesProvider)
	}
	if saved.APIKey != "secret" {
		t.Errorf("APIKey was blanked by an edit that did not include one")
	}
	if saved.PromptCacheMode != "auto-prefix" {
		t.Errorf("PromptCacheMode = %q, want it kept when the body omits it", saved.PromptCacheMode)
	}
}

// A field the body does carry still overrides. The merge above must not turn
// into "never change anything".
func TestSavingAProfileStillAppliesEveryFieldTheFormSends(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ProviderProfiles = map[string]config.ProviderProfile{
			"house": {URL: "https://old.example/v1", Model: "old-model", PromptCacheTTL: "1h"},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	previous := providers
	providers = newProviderState(store)
	defer func() { providers = previous }()

	if err := webSaveProvider("house", map[string]any{
		"endpoint":      "https://new.example/v1",
		"model":         "new-model",
		"reasoning":     "high",
		"context_limit": 200000.0,
		"output_cap":    32000.0,
		"cache_mode":    "off",
		"api_key":       "rotated",
	}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	saved := providers.profiles()["house"]
	if saved.Reasoning != "high" || saved.ContextLimit != 200000 || saved.MaxOutputTokens != 32000 {
		t.Fatalf("the form's own fields did not land: %+v", saved)
	}
	if saved.PromptCacheMode != "off" {
		t.Fatalf("PromptCacheMode = %q, want the body's off", saved.PromptCacheMode)
	}
	if saved.APIKey != "rotated" {
		t.Fatalf("APIKey = %q, want the new key", saved.APIKey)
	}
	if saved.PromptCacheTTL != "1h" {
		t.Fatalf("PromptCacheTTL = %q, want the unmentioned field kept", saved.PromptCacheTTL)
	}
}

// The dashboard's Stop button is wired to the bot's canceller, so the two are
// one control rather than a button that posts to a route nobody serves. This
// asserts the seam webControl installs, because the button being enabled is
// worthless if the function behind it is absent.
func TestTheStopControlIsWiredToTheRunningBot(t *testing.T) {
	var cfg miniapp.Config
	webControl(&cfg, func() string { return "" })
	if cfg.StopActiveRun == nil {
		t.Fatal("the surface exposes a Stop button with nothing behind it")
	}
	var cancelled bool
	activeRuns.Store(int64(777), &activeRun{
		cancel:    func() { cancelled = true },
		stop:      func() { cancelled = true },
		sessionID: "ses-dash",
	})
	t.Cleanup(func() { activeRuns.Delete(int64(777)) })

	stopped, sessionID := cfg.StopActiveRun(777)
	if !stopped || !cancelled {
		t.Fatalf("stop through the control surface did not reach the run: stopped=%v cancelled=%v", stopped, cancelled)
	}
	if sessionID != "ses-dash" {
		t.Fatalf("session = %q, want ses-dash", sessionID)
	}
}

// Mounting a project root is a control on this surface, so it has to land on
// this surface's session. It wrote under the Telegram chat id instead, which
// selected a project the dashboard never ran in and left its own marker naming
// whatever that chat had.
func TestProjectRootIsMountedForTheWebSessionNotTheTelegramChat(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	root := configuredWorkspaceRoot()
	if err := os.MkdirAll(filepath.Join(root, "widget"), 0o755); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := webSetProject(filepath.Join(root, "widget")); err != nil {
		t.Fatalf("mount project: %v", err)
	}
	if got := defaultState.projectForSession(miniapp.WebSessionID); got != filepath.Join(root, "widget") {
		t.Fatalf("the web session project = %q, want the mounted root", got)
	}
	// The Telegram chat's own selection must be untouched: the dashboard is not
	// where a chat's working directory is chosen. project() falls back to the
	// workspace root when nothing is set, so the map is what has to be checked.
	chatKey := strconv.FormatInt(defaultState.allowedUserID(), 10)
	defaultState.mu.RLock()
	moved, ok := defaultState.Projects[chatKey]
	defaultState.mu.RUnlock()
	if ok && moved != "" {
		t.Fatalf("mounting from the dashboard set the Telegram chat project to %q", moved)
	}
}

// And the marker the page draws has to be the one it just mounted, which means
// reading it back under the same key it was written.
func TestProjectListingReportsTheRootThisSurfaceMounted(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	root := configuredWorkspaceRoot()
	if err := os.MkdirAll(filepath.Join(root, "gadget"), 0o755); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// A Telegram chat project set the old way, to prove the listing ignores it.
	if err := defaultState.setProject(defaultState.allowedUserID(), root); err != nil {
		t.Fatalf("set chat project: %v", err)
	}
	if err := webSetProject(filepath.Join(root, "gadget")); err != nil {
		t.Fatalf("mount project: %v", err)
	}

	listed, err := webProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if got := listed["current"]; got != filepath.Join(root, "gadget") {
		t.Fatalf("current = %v, want the root the surface mounted", got)
	}
	entries, _ := listed["entries"].([]string)
	found := false
	for _, entry := range entries {
		if entry == "gadget" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a directory inside the workspace root is not listed: %v", entries)
	}
}

// The manual compaction button pays for a summary. Not saving it means the
// summary is discarded when the request ends and the next run re-reads the full
// history, so the transcript on disk is asserted, not just the reply.
func TestManualCompactionPersistsTheSummaryItPaidFor(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "CHECKPOINT SUMMARY"}}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()

	// Written at the path config.Load reads, which is CERAA_HOME/config.json and
	// is the same file the stubbed provider state points at. Seeding a store
	// somewhere else would leave the code under test reading a different config.
	dir := t.TempDir()
	cfgStore := config.NewStoreAt(filepath.Join(os.Getenv("CERAA_HOME"), "config.json"))
	if err := cfgStore.Update(func(cfg *config.Config) error {
		cfg.LLM = config.LLMProvider{
			URL:             provider.URL,
			APIKey:          "test",
			Model:           "test",
			ContextLimit:    200000,
			MaxOutputTokens: 4096,
		}
		cfg.SessionFile = dir
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	store, err := sessionstore.New(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// Long enough that selectMessageUnits leaves a head to summarise. Keep is
	// 8000 tokens, so the filler below is sized well past it.
	filler := strings.Repeat("archive this stretch of the conversation. ", 400)
	messages := []sessionstore.Message{{Role: llm.RoleUser, Content: filler}}
	for i := 0; i < 4; i++ {
		messages = append(messages,
			sessionstore.Message{Role: llm.RoleUser, Content: filler},
			sessionstore.Message{Role: llm.RoleAssistant, Content: "working on it"},
		)
	}
	if err := store.Save(&sessionstore.Session{ID: "ses-compact", Version: 1, Messages: messages}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	result, err := webTriggerCompaction("ses-compact")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result["compacted"] != true {
		t.Fatalf("compacted = %v, want true", result["compacted"])
	}
	if requests.Load() == 0 {
		t.Fatal("no provider request was made, so nothing was summarised")
	}

	// Re-read through a fresh store: this is what the next run sees.
	reloaded, err := sessionstore.New(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	sess, err := reloaded.Load("ses-compact")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	summaries := 0
	for _, message := range sess.Messages {
		if message.Compaction != nil && strings.Contains(message.Compaction.Summary, "CHECKPOINT SUMMARY") {
			summaries++
		}
	}
	if summaries == 0 {
		t.Fatalf("the summary was not written to disk; the session still holds %d messages and no checkpoint", len(sess.Messages))
	}
	if len(sess.Messages) >= len(messages) {
		t.Fatalf("the transcript was not shortened: %d messages, was %d", len(sess.Messages), len(messages))
	}
}

// A session id that does not exist is an error, not "nothing to compact". Load
// answers an unknown id with an empty session, so the button used to report a
// successful no-op for a conversation that was never there.
func TestCompactingASessionThatDoesNotExistIsAnError(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	dir := t.TempDir()
	cfgStore := config.NewStoreAt(filepath.Join(os.Getenv("CERAA_HOME"), "config.json"))
	if err := cfgStore.Update(func(cfg *config.Config) error {
		cfg.SessionFile = dir
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, err := webTriggerCompaction("ses-never-existed"); err == nil {
		t.Fatal("compacting a session that does not exist reported success")
	}
}

// The dashboard rendered the context window straight from cfg.LLM.ContextLimit,
// and that is the wrong block as well as the wrong value. An active provider
// profile states its own limit and mergeProfile folds it onto the base, so the
// limit in force is the profile's while the base block still reads zero. Reading
// the base reported the fallback for a provider that says 200k: a plausible
// wrong number, which is worse than the obvious "0k tok" this replaced.
//
// The configured base is still reported raw, because that is the row the operator
// edits. What may be displayed is the effective one.
func TestTheDashboardReportsAContextLimitTheRunnerWouldActuallyUse(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	dir := t.TempDir()
	t.Setenv("CERAA_HOME", dir)

	// The reported case: a provider profile that states a window, selected, with
	// the base block left unset.
	store := config.NewStoreAt(filepath.Join(dir, "config.json"))
	err := store.Update(func(cfg *config.Config) error {
		cfg.ActiveProvider = "codex"
		cfg.ProviderProfiles = map[string]config.ProviderProfile{
			"codex": {ContextLimit: 200000, Model: "gpt-6-luna"},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed an active provider profile: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.LLM.ContextLimit != 0 {
		t.Fatalf("the fixture needs the base block unset, got %d", cfg.LLM.ContextLimit)
	}
	if got := cfg.ActiveLLM().ContextLimit; got != 200000 {
		t.Fatalf("the merged view should carry the profile's limit, got %d", got)
	}

	params := webRunParams()
	configured, _ := params["context_limit"].(int)
	effective, _ := params["context_limit_effective"].(int)
	source, _ := params["context_limit_source"].(string)

	if configured != 0 {
		t.Errorf("configured context_limit = %d, want the raw base 0 so the row stays editable", configured)
	}
	if effective != 200000 {
		t.Errorf("effective context limit = %d, want the profile's 200000; reporting the floor here is a plausible wrong number", effective)
	}
	if source != string(llm.ContextFromProfile) {
		t.Errorf("context limit source = %q, want %q", source, llm.ContextFromProfile)
	}
}

// With no profile and no configured limit the runner falls back rather than
// running on zero, and the dashboard has to say the floor was used rather than
// leave a bare number that looks measured.
func TestTheDashboardNamesTheContextLimitFloorWhenNothingStates(t *testing.T) {
	restore := stubBothState(t)
	defer restore()
	t.Setenv("CERAA_HOME", t.TempDir())

	if got, _ := config.Load(); got.LLM.ContextLimit != 0 {
		t.Fatalf("the fixture assumes context_limit is unset by default, got %d", got.LLM.ContextLimit)
	}

	params := webRunParams()
	effective, _ := params["context_limit_effective"].(int)
	source, _ := params["context_limit_source"].(string)
	if effective != llm.FallbackContextWindow {
		t.Errorf("effective context limit = %d, want the fallback %d; a zero here is what put 0k on the screen",
			effective, llm.FallbackContextWindow)
	}
	if source != string(llm.ContextFromDefault) {
		t.Errorf("context limit source = %q, want %q", source, llm.ContextFromDefault)
	}

	// A limit configured on the base is a decision somebody made, so it must win
	// outright rather than be second-guessed by the resolver the display shares.
	if err := webSetRunParams(map[string]any{"context_limit": float64(200000)}); err != nil {
		t.Fatalf("set context_limit: %v", err)
	}
	params = webRunParams()
	if effective, _ := params["context_limit_effective"].(int); effective != 200000 {
		t.Errorf("effective context limit after configuring one = %d, want 200000", effective)
	}
	if source, _ := params["context_limit_source"].(string); source != string(llm.ContextFromProfile) {
		t.Errorf("source after configuring a limit = %q, want %q", source, llm.ContextFromProfile)
	}
}

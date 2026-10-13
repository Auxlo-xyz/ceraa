package tool

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ceraa/harness/netguard"
	"ceraa/harness/operation"
	"ceraa/internal/paths"
)

// BrowserTool drives a real Chromium through go-rod.
//
// The driver sits behind an interface so the tool layer can be tested without a
// browser, and so a browser crash surfaces as an ordinary tool error instead of
// taking the process down.
type BrowserTool struct {
	manager *BrowserManager
}

const (
	// browserIdleTimeout closes a browser that has been unused for this long, so
	// a forgotten page does not keep a Chromium process alive forever.
	browserIdleTimeout = 5 * time.Minute
	// browserLaunchTimeout bounds the first launch, which includes downloading
	// Chromium the very first time.
	browserLaunchTimeout = 10 * time.Minute
	// pageActionTimeout bounds a single page interaction, so a hung navigation
	// fails the call instead of stalling the run.
	pageActionTimeout = 60 * time.Second
	browserMaxOutput  = 20_000
)

// NewBrowserTool returns a tool bound to a fresh manager. One manager owns one
// browser, which is what makes a sequence of actions meaningful.
func NewBrowserTool() *BrowserTool {
	return &BrowserTool{manager: NewBrowserManager(browserIdleTimeout)}
}

func (b *BrowserTool) Name() string { return "browser" }

func (b *BrowserTool) Description() string {
	// The action list, and what each argument belongs to, are carried by the
	// `action` enum and the per-argument descriptions. Repeating them here made
	// this the most expensive schema in the registry, and it did not help: a
	// model still paired arguments with the wrong action 14 times in a row
	// against the longer text. What is left is only what a schema cannot say,
	// namely that the page is shared state across calls.
	return "Drive a real Chromium browser. Each argument belongs to the action named in " +
		"its own description; passing one to a different action is rejected."
}

func (b *BrowserTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"open", "screenshot", "text", "html", "click", "type", "eval", "status", "close"},
				"description": "The single browser action to perform. Exactly one action per call: to open a page and then act on it, make a separate call for each step. Sending arguments that belong to a different action is rejected.",
			},
			"url": map[string]any{"type": "string", "description": "Web address to open, http or https. Use with action=open."},
			"path": map[string]any{
				"type":        "string",
				"description": "For action=open, a local file to render. For action=screenshot, where to write the image, defaulting to the Ceraa data directory.",
			},
			"selector": map[string]any{"type": "string", "description": "CSS selector for screenshot, text, html, click, or type."},
			"text":     map[string]any{"type": "string", "description": "Text to type for action=type."},
			"script":   map[string]any{"type": "string", "description": "JavaScript source for action=eval. The page's result is returned as JSON."},
			"full_page": map[string]any{
				"type":        "boolean",
				"description": "Capture the whole scrollable page rather than the viewport. action=screenshot only.",
			},
			"submit": map[string]any{
				"type":        "boolean",
				"description": "Press Enter after typing. action=type only.",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "Milliseconds to wait after the action before returning, for pages that render late.",
			},
		},
		"required": []string{"action"},
	}
}

// browserArgRules is one action's accepted arguments.
//
// Named rather than anonymous so a per-action tool can declare its own set. The
// combined tool needs either url or path, which cannot be written down as a list,
// so browser_open takes a required url and browser_open_file takes a required
// path. Two tools, two required arguments, and no schema that a call with
// nothing in it can satisfy.
type browserArgRules struct {
	required []string
	optional []string
}

// browserArgSpec documents the arguments each action accepts, so a call with a
// meaningless argument is reported as a mistake rather than silently ignored.
var browserArgSpec = map[string]browserArgRules{
	"open":       {required: []string{}, optional: []string{"url", "path", "wait_ms"}},
	"screenshot": {optional: []string{"path", "selector", "full_page", "wait_ms"}},
	"text":       {optional: []string{"selector", "wait_ms"}},
	"html":       {optional: []string{"selector", "wait_ms"}},
	"click":      {required: []string{"selector"}, optional: []string{"wait_ms"}},
	"type":       {required: []string{"selector", "text"}, optional: []string{"submit", "wait_ms"}},
	"eval":       {required: []string{"script"}, optional: []string{"wait_ms"}},
	"status":     {},
	"close":      {},
}

// browserActionAlias maps an action name a model is likely to reach for onto the
// real one. Only collisions with an argument name belong here; every other
// misspelling should still be rejected, because a rejected name tells the model it
// guessed wrong, while a silently accepted one hides it.
var browserActionAlias = map[string]string{
	// The argument for the JavaScript action is `script`, and the action is `eval`.
	"script": "eval",
	// Likewise `js`, which is the other name a model reaches for.
	"js": "eval",
}

// browserArgOwners is the reverse of browserArgSpec: for each argument, the
// actions that accept it.
//
// Built rather than written out, so adding an action cannot leave this stale
// and pointing the model at a call shape that no longer validates.
var browserArgOwners = func() map[string][]string {
	owners := make(map[string][]string)
	for action, spec := range browserArgSpec {
		for _, key := range append(append([]string{}, spec.required...), spec.optional...) {
			owners[key] = append(owners[key], action)
		}
	}
	// Sorted, because map iteration order would otherwise leak into the message
	// and make the same mistake produce different advice on different runs.
	for key := range owners {
		sort.Strings(owners[key])
	}
	return owners
}()

// browserArgOwnerHint names the actions that do accept an argument the caller
// put on the wrong one.
//
// The rejection already says what the chosen action accepts, and that was not
// enough: the model retried the same call shape, concluded the tool was
// unavailable, and told the user the browser was missing. It had nowhere to go,
// because nothing said where `submit` goes. Naming the owner turns a dead end
// into the next call, and stating that action's required arguments hands over a
// shape that validates instead of one to guess at.
func browserArgOwnerHint(arg string) string {
	owners := browserArgOwners[arg]
	if len(owners) == 0 {
		return ""
	}
	parts := make([]string, 0, len(owners))
	for _, action := range owners {
		// The offending argument is dropped from the list of what the owner
		// needs. Repeating it back reads as though it were still the problem,
		// and "action=click with selector" as advice about a misplaced selector
		// is worse than no advice.
		var needed []string
		for _, key := range browserArgSpec[action].required {
			if key != arg {
				needed = append(needed, key)
			}
		}
		if len(needed) > 0 {
			parts = append(parts, fmt.Sprintf("action=%s with %s", action, strings.Join(needed, " and ")))
			continue
		}
		parts = append(parts, "action="+action)
	}
	return fmt.Sprintf(" %q belongs to %s, as its own call.", arg, strings.Join(parts, " or "))
}

// describeBrowserArgs renders what an action accepts, for the rejection message.
//
// Naming only the offending argument left the model with no way to recover: it
// retried the same call shape until it concluded the tool was unavailable, and
// then told the user the browser tools were missing. Naming what is accepted,
// and stating the one-action-per-call rule, is what lets it try again correctly.
func describeBrowserArgs(spec struct {
	required []string
	optional []string
}) string {
	switch {
	case len(spec.required) > 0 && len(spec.optional) > 0:
		return "it requires " + strings.Join(spec.required, " and ") +
			", and also accepts " + strings.Join(spec.optional, ", ")
	case len(spec.required) > 0:
		return "it requires " + strings.Join(spec.required, " and ")
	case len(spec.optional) > 0:
		return "it accepts " + strings.Join(spec.optional, ", ")
	default:
		return "it takes no arguments"
	}
}

func (b *BrowserTool) Translate(args map[string]any) (operation.Spec, error) {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		return operation.Spec{}, errors.New("action is required")
	}
	// "eval" is the only action whose name differs from the argument it takes, and
	// that mismatch was the single most common way this tool was called wrong: a
	// model asked for action=script because the argument is called script, and got
	// told the action did not exist. Accepting the argument's own name is free and
	// removes the collision at its source, rather than describing it again — the
	// descriptions already spell out which arguments belong to which action, and a
	// model paired them wrongly 14 times in a row against that longer text.
	if alias, ok := browserActionAlias[action]; ok {
		action = alias
	}
	spec, known := browserArgSpec[action]
	if !known {
		return operation.Spec{}, fmt.Errorf("unknown browser action %q; use one of open, screenshot, text, html, click, type, eval, status, close", action)
	}
	// The action selector itself is always allowed; everything else must be one
	// this action can actually use.
	allowed := map[string]bool{"action": true}
	for _, key := range append(append([]string{}, spec.required...), spec.optional...) {
		allowed[key] = true
	}
	for key := range args {
		if !allowed[key] {
			return operation.Spec{}, fmt.Errorf(
				"browser action %q does not accept %q; %s.%s Each call performs exactly one action, so a sequence needs separate calls.",
				action, key, describeBrowserArgs(spec), browserArgOwnerHint(key))
		}
	}
	translated := map[string]any{"action": action}
	for _, key := range append(append([]string{}, spec.required...), spec.optional...) {
		value, present := args[key]
		if !present || value == nil {
			continue
		}
		if err := validateBrowserArg(action, key, value); err != nil {
			return operation.Spec{}, err
		}
		translated[key] = value
	}
	for _, key := range spec.required {
		if _, ok := translated[key]; !ok {
			return operation.Spec{}, fmt.Errorf("browser action %q requires %q", action, key)
		}
	}
	// open needs a target, and accepts either a web address or a local file. A
	// separate path argument rather than a file:// URL keeps the URL check
	// honest: web addresses are still restricted to http and https.
	if action == "open" {
		_, hasURL := translated["url"]
		_, hasPath := translated["path"]
		if hasURL == hasPath {
			return operation.Spec{}, errors.New("browser action \"open\" requires exactly one of \"url\" or \"path\"")
		}
	}
	return operation.Spec{Type: "browser", Args: translated}, nil
}

// validateBrowserArg rejects arguments of the wrong shape. Validating here means
// a malformed call is refused before a browser is ever launched.
func validateBrowserArg(action, key string, value any) error {
	switch key {
	case "full_page", "submit":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be true or false", key)
		}
	case "wait_ms":
		number, ok := value.(float64)
		if !ok || number < 0 || number > 60_000 {
			return fmt.Errorf("wait_ms must be between 0 and 60000")
		}
	case "url", "path", "selector", "text", "script":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", key)
		}
		if strings.ContainsRune(text, 0) {
			return fmt.Errorf("%s must not contain a null byte", key)
		}
		if key == "url" {
			if err := validateBrowserURL(strings.TrimSpace(text)); err != nil {
				return err
			}
		}
		if key == "path" && action == "open" {
			// Checked here so an unusable target is refused before the call is
			// dispatched, rather than failing after a browser has been launched.
			if _, err := localFileURL(strings.TrimSpace(text)); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateBrowserURL keeps the browser to the web, and off this machine.
//
// Two separate restrictions, and the second one used to be missing. A file:// or
// javascript: URL would let a page read this host's filesystem, which is well
// beyond browsing. A public URL is the other half: nothing stopped the browser
// being pointed at 169.254.169.254, where cloud instance credentials live, or
// at 127.0.0.1, where the Mini App, the model catalog cache and the bridge all
// listen. An agent that reads a hostile page gets to follow whatever address it
// finds.
//
// Checking only the scheme is therefore not enough on its own, and the browser
// is the easiest way in because needsPermission deliberately exempts it. The
// refusal names the address so the model can say something useful about it.
//
// What this check does NOT cover, stated plainly rather than implied: navigation
// happens inside Chromium over CDP, so netguard's dialer guard is not in the
// path here the way it is for webfetch. That leaves two gaps a reader should know
// about rather than assume away. A page that navigates itself — location.href,
// a redirect chain, a form post — is not re-checked, because this function sees
// the URL the model asked for and not the ones the page reaches afterwards. And
// browser_eval runs in the page, so script there can fetch any origin it likes.
// Closing both means intercepting inside Chromium, which is a larger change
// than this guard and is not attempted here.
//
// Localhost is genuinely useful work, so CERAA_ALLOW_PRIVATE_NET reopens it
// deliberately and says so in the log.
func validateBrowserURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	lowered := strings.ToLower(raw)
	if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return errors.New(nonWebRefusal(raw))
	}
	return netguard.CheckURL(context.Background(), raw)
}

// nonWebRefusal explains the block instead of only stating it: the model is the
// only reader, and it needs the scheme it used and where a local file belongs.
func nonWebRefusal(raw string) string {
	scheme := ""
	if parsed, err := url.Parse(raw); err == nil {
		scheme = strings.ToLower(parsed.Scheme)
	}
	if scheme == "" {
		return fmt.Sprintf("%q does not look like a URL: a scheme such as https:// is required. "+
			"For a local file, use browser_open_file with the path argument.", raw)
	}
	return fmt.Sprintf("the %s: scheme is refused by design; only http:// and https:// URLs may be opened. "+
		"For a local file, use browser_open_file with the path argument.", scheme)
}

func (b *BrowserTool) TranslateResult(op operation.Operation) string {
	if op.Error != "" && op.Output == "" {
		return "Error: " + op.Error
	}
	return op.Output
}

// Run satisfies tool.Runner. A browser is stateful, so the action runs in the
// process and not through a shell.
func (b *BrowserTool) Run(spec operation.Spec) (string, error) {
	return b.RunContext(context.Background(), spec)
}

// RunContext performs one browser action.
func (b *BrowserTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	action, _ := spec.Args["action"].(string)
	switch action {
	case "close":
		return b.manager.Close()
	case "status":
		// Status must not launch a browser just to report that none exists.
		return b.manager.Status()
	}
	driver, err := b.manager.Driver(ctx)
	if err != nil {
		return "", err
	}
	output, err := performBrowserAction(ctx, driver, spec.Args)
	if err != nil {
		return "", err
	}
	if output != "" && len(output) > browserMaxOutput {
		output = output[:browserMaxOutput] + "\n… truncated"
	}
	return output, nil
}

// performBrowserAction dispatches one validated action against the driver.
func performBrowserAction(ctx context.Context, driver browserDriver, args map[string]any) (string, error) {
	stringArg := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	waitMS := 0
	if value, ok := args["wait_ms"].(float64); ok {
		waitMS = int(value)
	}
	settle := func() {
		if waitMS > 0 {
			select {
			case <-time.After(time.Duration(waitMS) * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}

	action, _ := args["action"].(string)
	switch action {
	case "open":
		target, err := browserOpenTarget(args)
		if err != nil {
			return "", err
		}
		info, err := driver.Navigate(ctx, target)
		if err != nil {
			return "", err
		}
		settle()
		return formatPageInfo(info), nil
	case "screenshot":
		path := stringArg("path")
		if path == "" {
			path = defaultScreenshotPath()
		}
		if err := ensureParentDir(path); err != nil {
			return "", err
		}
		fullPage, _ := args["full_page"].(bool)
		if err := driver.Screenshot(ctx, path, fullPage, stringArg("selector")); err != nil {
			return "", err
		}
		settle()
		return fmt.Sprintf("screenshot saved to %s", path), nil
	case "text":
		settle()
		text, err := driver.Text(ctx, stringArg("selector"))
		if err != nil {
			return "", err
		}
		return text, nil
	case "html":
		settle()
		return driver.HTML(ctx, stringArg("selector"))
	case "click":
		settle()
		if err := driver.Click(ctx, stringArg("selector")); err != nil {
			return "", err
		}
		settle()
		info, err := driver.Info(ctx)
		if err == nil {
			return "clicked " + stringArg("selector") + "\n" + formatPageInfo(info), nil
		}
		return "clicked " + stringArg("selector"), nil
	case "type":
		submit, _ := args["submit"].(bool)
		settle()
		if err := driver.Type(ctx, stringArg("selector"), args["text"].(string), submit); err != nil {
			return "", err
		}
		settle()
		return "typed into " + stringArg("selector"), nil
	case "eval":
		settle()
		result, err := driver.Eval(ctx, stringArg("script"))
		if err != nil {
			return "", err
		}
		return result, nil
	}
	return "", fmt.Errorf("unsupported browser action %q", action)
}

func formatPageInfo(info pageInfo) string {
	var builder strings.Builder
	if info.URL != "" {
		fmt.Fprintf(&builder, "url: %s\n", info.URL)
	}
	if info.Title != "" {
		fmt.Fprintf(&builder, "title: %s\n", info.Title)
	}
	if info.Status != 0 {
		fmt.Fprintf(&builder, "status: %d\n", info.Status)
	}
	return strings.TrimRight(builder.String(), "\n")
}

// defaultScreenshotPath writes into the data directory so a screenshot never
// lands in the user's project as an untracked file.
func defaultScreenshotPath() string {
	name := fmt.Sprintf("shot-%s.png", time.Now().UTC().Format("20060102-150405"))
	return filepath.Join(paths.DataDir(), "browser-shots", name)
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create screenshot directory: %w", err)
	}
	return nil
}

// BrowserManager owns at most one browser and hands it to sequential callers.
//
// Ceraa dispatches independent tools concurrently, and a browser is shared
// mutable state, so access is serialised here. The browser is closed after an
// idle period, and a crashed browser is discarded rather than reused.
type BrowserManager struct {
	mu        sync.Mutex
	driver    browserDriver
	lastUsed  time.Time
	idle      time.Duration
	newDriver func(context.Context) (browserDriver, error)
}

// liveManagers tracks every manager that has been handed out, so process
// shutdown can close the browsers they launched.
//
// Without this a Chromium outlives the daemon: it is a child process, so killing
// Ceraa reparents the browser tree to init rather than terminating it, and it
// then sits there indefinitely. Observed as 23 orphaned Chromium processes
// surviving 21 restarts, one of them 14 hours old. The idle timeout does not
// help, because a process that is no longer running never re-checks it.
//
// Managers are created per tool registry rather than once per process, so a
// single global would not do; they are tracked and removed on Close instead.
var (
	liveManagersMu sync.Mutex
	liveManagers   = map[*BrowserManager]struct{}{}
)

func trackManager(m *BrowserManager) {
	liveManagersMu.Lock()
	defer liveManagersMu.Unlock()
	liveManagers[m] = struct{}{}
}

func untrackManager(m *BrowserManager) {
	liveManagersMu.Lock()
	defer liveManagersMu.Unlock()
	delete(liveManagers, m)
}

// CloseLiveBrowsers closes every browser this process launched and reports how
// many were still open. Safe to call with none.
func CloseLiveBrowsers() int {
	liveManagersMu.Lock()
	managers := make([]*BrowserManager, 0, len(liveManagers))
	for m := range liveManagers {
		managers = append(managers, m)
	}
	liveManagersMu.Unlock()
	closed := 0
	for _, m := range managers {
		if _, err := m.Close(); err == nil {
			closed++
		}
	}
	return closed
}

// NewBrowserManager builds a manager that launches a real Chromium on demand.
func NewBrowserManager(idle time.Duration) *BrowserManager {
	m := &BrowserManager{
		idle:      idle,
		newDriver: func(ctx context.Context) (browserDriver, error) { return newRodDriver(ctx) },
	}
	trackManager(m)
	return m
}

// Driver returns the live browser, launching or replacing it as needed.
func (m *BrowserManager) Driver(ctx context.Context) (browserDriver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeIfIdleLocked()
	if m.driver != nil {
		m.lastUsed = time.Now()
		return m.driver, nil
	}
	driver, err := m.newDriver(ctx)
	if err != nil {
		return nil, err
	}
	m.driver = driver
	m.lastUsed = time.Now()
	return driver, nil
}

// Close shuts the browser down. The manager can launch a fresh one afterwards.
func (m *BrowserManager) Close() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driver == nil {
		return "no browser is open", nil
	}
	err := m.driver.Close()
	m.driver = nil
	untrackManager(m)
	if err != nil {
		return "", fmt.Errorf("close browser: %w", err)
	}
	return "browser closed", nil
}

// Status reports browser state without launching anything.
func (m *BrowserManager) Status() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driver == nil {
		return "no browser is open; use action=open to start one", nil
	}
	info, err := m.driver.Info(context.Background())
	if err != nil {
		return "", err
	}
	return "browser is open\n" + formatPageInfo(info), nil
}

// Discard drops a driver that has failed, so the next call starts clean rather
// than reusing a dead browser.
func (m *BrowserManager) Discard() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driver != nil {
		_ = m.driver.Close()
		m.driver = nil
	}
}

func (m *BrowserManager) closeIfIdleLocked() {
	if m.driver == nil || m.idle <= 0 {
		return
	}
	if time.Since(m.lastUsed) < m.idle {
		return
	}
	_ = m.driver.Close()
	m.driver = nil
}

// Sequential reports that browser actions must not run concurrently. They share
// one page, so two actions at once would interleave on the same document and
// produce results neither caller asked for.
func (b *BrowserTool) Sequential() bool        { return true }
func (b *BrowserTool) SequentialQueue() string { return "browser" }

// browserOpenTarget resolves what to navigate to.
//
// A local file is served from disk through a file URL rather than being copied
// anywhere or read into memory. A web address still has to be http or https:
// those are the schemes a browser may be pointed at, and a javascript: URL in
// particular would execute in the page rather than navigate to one.
func browserOpenTarget(args map[string]any) (string, error) {
	if raw, ok := args["url"].(string); ok && strings.TrimSpace(raw) != "" {
		target := strings.TrimSpace(raw)
		if err := validateBrowserURL(target); err != nil {
			return "", err
		}
		return target, nil
	}
	raw, _ := args["path"].(string)
	target, err := localFileURL(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return target, nil
}

// localFileURL turns a path into a file URL a browser can render.
func localFileURL(path string) (string, error) {
	if path == "" {
		return "", errors.New("open requires either \"url\" or \"path\"")
	}
	resolved, err := ResolvePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	if info.IsDir() {
		// A directory has no index document to render, and guessing one would
		// open something other than what was asked for.
		return "", fmt.Errorf("open %s: that is a directory, name a file inside it", path)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String(), nil
}

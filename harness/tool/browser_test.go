package tool

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDriver records what the tool layer asked for, so behaviour can be pinned
// without launching a browser.
type fakeDriver struct {
	mu         sync.Mutex
	events     []string
	info       pageInfo
	text       string
	html       string
	eval       string
	failWith   error
	closed     bool
	launched   int
	screenshot []byte
}

func (f *fakeDriver) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeDriver) Navigate(ctx context.Context, rawURL string) (pageInfo, error) {
	f.record("navigate:" + rawURL)
	return f.info, f.failWith
}

func (f *fakeDriver) Screenshot(ctx context.Context, path string, fullPage bool, selector string) error {
	f.record(fmt.Sprintf("screenshot:%s:full=%v:selector=%s", path, fullPage, selector))
	if f.failWith != nil {
		return f.failWith
	}
	return os.WriteFile(path, f.screenshot, 0o644)
}

func (f *fakeDriver) Text(ctx context.Context, selector string) (string, error) {
	f.record("text:" + selector)
	return f.text, f.failWith
}

func (f *fakeDriver) HTML(ctx context.Context, selector string) (string, error) {
	f.record("html:" + selector)
	return f.html, f.failWith
}

func (f *fakeDriver) Click(ctx context.Context, selector string) error {
	f.record("click:" + selector)
	return f.failWith
}

func (f *fakeDriver) Type(ctx context.Context, selector, text string, submit bool) error {
	f.record(fmt.Sprintf("type:%s:%s:submit=%v", selector, text, submit))
	return f.failWith
}

func (f *fakeDriver) Eval(ctx context.Context, script string) (string, error) {
	f.record("eval:" + script)
	return f.eval, f.failWith
}

func (f *fakeDriver) Info(ctx context.Context) (pageInfo, error) {
	return f.info, f.failWith
}

func (f *fakeDriver) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeDriver) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.events, ",")
}

func newFakeManager() (*BrowserTool, *fakeDriver) {
	driver := &fakeDriver{
		info:       pageInfo{URL: "https://example.com/", Title: "Example"},
		text:       "hello world",
		html:       "<html></html>",
		eval:       `"result"`,
		screenshot: []byte("\x89PNG fake"),
	}
	manager := &BrowserManager{
		idle: time.Hour,
		newDriver: func(context.Context) (browserDriver, error) {
			driver.launched++
			return driver, nil
		},
	}
	return &BrowserTool{manager: manager}, driver
}

// TestBrowserTranslateRejectsMeaninglessArguments pins that a call carrying an
// argument its action cannot use is refused, instead of being silently ignored
// because the model guessed a field name.
func TestBrowserTranslateRejectsMeaninglessArguments(t *testing.T) {
	tool := NewBrowserTool()
	if _, err := tool.Translate(map[string]any{"action": "open", "url": "https://example.com", "script": "alert(1)"}); err == nil {
		t.Fatal("open must not accept a script argument")
	}
	if _, err := tool.Translate(map[string]any{"action": "status", "selector": "body"}); err == nil {
		t.Fatal("status must not accept a selector")
	}
}

// TestBrowserTranslateRequiresItsOwnArguments pins that each action must be given
// what it needs, before a browser is ever launched.
func TestBrowserTranslateRequiresItsOwnArguments(t *testing.T) {
	tool := NewBrowserTool()
	cases := []map[string]any{
		{"action": "open"},
		{"action": "click"},
		{"action": "type", "selector": "#q"},
		{"action": "eval"},
	}
	for _, args := range cases {
		if _, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) should have failed", args)
		}
	}
}

// TestBrowserTranslateRejectsNonWebURLs pins the boundary. A file:// or
// javascript: URL would turn a browsing tool into a way to read this host.
func TestBrowserTranslateRejectsNonWebURLs(t *testing.T) {
	tool := NewBrowserTool()
	for _, url := range []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://host/f", "example.com"} {
		if _, err := tool.Translate(map[string]any{"action": "open", "url": url}); err == nil {
			t.Errorf("url %q should have been rejected", url)
		}
	}
	if _, err := tool.Translate(map[string]any{"action": "open", "url": "https://example.com"}); err != nil {
		t.Errorf("a normal url must be accepted: %v", err)
	}
}

// TestBrowserTranslateRejectsWrongArgumentTypes pins that a malformed argument is
// reported as a mistake rather than coerced.
func TestBrowserTranslateRejectsWrongArgumentTypes(t *testing.T) {
	tool := NewBrowserTool()
	cases := []map[string]any{
		{"action": "screenshot", "full_page": "yes"},
		{"action": "screenshot", "wait_ms": -1},
		{"action": "screenshot", "wait_ms": 99_999.0},
		{"action": "click", "selector": 42},
	}
	for _, args := range cases {
		if _, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) should have failed", args)
		}
	}
}

// TestBrowserActionsReachTheDriver pins that each action is dispatched to the
// right driver call with the right arguments.
func TestBrowserActionsReachTheDriver(t *testing.T) {
	tool, driver := newFakeManager()
	dir := t.TempDir()

	specs := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "open", "url": "https://example.com/"}, "navigate:https://example.com/"},
		{map[string]any{"action": "text"}, "text:"},
		{map[string]any{"action": "text", "selector": "h1"}, "text:h1"},
		{map[string]any{"action": "html", "selector": "main"}, "html:main"},
		{map[string]any{"action": "click", "selector": "#go"}, "click:#go"},
		{map[string]any{"action": "type", "selector": "#q", "text": "rod", "submit": true}, "type:#q:rod:submit=true"},
		{map[string]any{"action": "eval", "script": "1+1"}, "eval:1+1"},
		{map[string]any{"action": "screenshot", "path": dir + "/shot.png", "full_page": true}, "screenshot:" + dir + "/shot.png:full=true"},
	}
	for _, tc := range specs {
		spec, err := tool.Translate(tc.args)
		if err != nil {
			t.Fatalf("Translate(%v): %v", tc.args, err)
		}
		if _, err := tool.RunContext(context.Background(), spec); err != nil {
			t.Fatalf("run %v: %v", tc.args, err)
		}
	}
	seen := driver.seen()
	for _, want := range []string{
		"navigate:https://example.com/",
		"text:h1",
		"html:main",
		"click:#go",
		"type:#q:rod:submit=true",
		"eval:1+1",
		"screenshot:" + dir + "/shot.png:full=true",
	} {
		if !strings.Contains(seen, want) {
			t.Errorf("driver never saw %q; saw %q", want, seen)
		}
	}
}

// TestScreenshotReportsItsPath pins that a screenshot returns the path the agent
// can hand straight to view_image.
func TestScreenshotReportsItsPath(t *testing.T) {
	tool, _ := newFakeManager()
	path := t.TempDir() + "/page.png"
	spec, err := tool.Translate(map[string]any{"action": "screenshot", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("output %q should name the screenshot", out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("screenshot was not written: %v", err)
	}
}

// TestScreenshotDefaultsIntoTheDataDir pins that a screenshot with no path does
// not land in the user's project as an untracked file.
func TestScreenshotDefaultsIntoTheDataDir(t *testing.T) {
	tool, _ := newFakeManager()
	spec, err := tool.Translate(map[string]any{"action": "screenshot"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "browser-shots") {
		t.Fatalf("default screenshot path %q should live in the data directory", out)
	}
}

// TestStatusDoesNotLaunchABrowser pins that asking about browser state must not
// start Chromium, which would cost a 150MB download to answer a question.
func TestStatusDoesNotLaunchABrowser(t *testing.T) {
	tool, driver := newFakeManager()
	spec, _ := tool.Translate(map[string]any{"action": "status"})
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if driver.launched != 0 {
		t.Fatal("status must not launch a browser")
	}
	if !strings.Contains(out, "no browser is open") {
		t.Fatalf("status = %q", out)
	}
}

// TestCloseIsIdempotent pins that closing when nothing is open is not an error,
// so an agent can tidy up unconditionally.
func TestCloseIsIdempotent(t *testing.T) {
	tool, _ := newFakeManager()
	spec, _ := tool.Translate(map[string]any{"action": "close"})
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatalf("closing with no browser open should succeed: %v", err)
	}
	if !strings.Contains(out, "no browser") {
		t.Fatalf("output = %q", out)
	}
}

// TestBrowserIsSharedAcrossActions pins the property that makes a sequence of
// actions meaningful: one browser, reused.
func TestBrowserIsSharedAcrossActions(t *testing.T) {
	tool, driver := newFakeManager()
	for _, url := range []string{"https://a.example/", "https://b.example/"} {
		spec, err := tool.Translate(map[string]any{"action": "open", "url": url})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tool.RunContext(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
	}
	if driver.launched != 1 {
		t.Fatalf("two actions launched %d browsers, want 1", driver.launched)
	}
}

// TestIdleBrowserIsClosed pins that a forgotten page does not keep a Chromium
// process alive forever.
func TestIdleBrowserIsClosed(t *testing.T) {
	driver := &fakeDriver{info: pageInfo{URL: "https://example.com/"}}
	manager := &BrowserManager{
		idle: time.Millisecond,
		newDriver: func(context.Context) (browserDriver, error) {
			driver.launched++
			return driver, nil
		},
	}
	tool := &BrowserTool{manager: manager}
	spec, _ := tool.Translate(map[string]any{"action": "open", "url": "https://example.com/"})
	if _, err := tool.RunContext(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := tool.RunContext(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !driver.closed {
		t.Fatal("an idle browser should have been closed")
	}
	if driver.launched != 2 {
		t.Fatalf("browser was relaunched %d times, want 2", driver.launched)
	}
}

// TestDriverFailureIsReportedAsToolError pins that a browser problem surfaces as
// an ordinary error, naming the action, rather than a panic or a silent success.
func TestDriverFailureIsReportedAsToolError(t *testing.T) {
	tool, driver := newFakeManager()
	driver.failWith = fmt.Errorf("chrome exploded")
	spec, _ := tool.Translate(map[string]any{"action": "open", "url": "https://example.com/"})
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "chrome exploded") {
		t.Fatalf("error %v should include the cause", err)
	}
}

// TestBrowserToolIsSequential pins that browser actions never run concurrently,
// since they share a single page.
func TestBrowserToolIsSequential(t *testing.T) {
	var candidate any = NewBrowserTool()
	sequential, ok := candidate.(SequentialTool)
	if !ok || !sequential.Sequential() {
		t.Fatal("browser actions share a page and must be sequential")
	}
}

// TestBrowserOutputIsBounded pins that a huge page body cannot flood the session.
func TestBrowserOutputIsBounded(t *testing.T) {
	tool, driver := newFakeManager()
	driver.text = strings.Repeat("a", browserMaxOutput*2)
	spec, _ := tool.Translate(map[string]any{"action": "text"})
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > browserMaxOutput+40 {
		t.Fatalf("output not bounded: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("truncated output should say so")
	}
}

// TestBrowserOpenRendersLocalFile pins that a file the agent just wrote can be
// rendered, which is the common case and was previously rejected as a file://
// URL with no supported alternative.
func TestBrowserOpenRendersLocalFile(t *testing.T) {
	tool := NewBrowserTool()
	dir := t.TempDir()
	page := dir + "/landing.html"
	if err := os.WriteFile(page, []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := tool.Translate(map[string]any{"action": "open", "path": page})
	if err != nil {
		t.Fatalf("open by path should be accepted: %v", err)
	}
	target, err := browserOpenTarget(spec.Args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(target, "file://") || !strings.HasSuffix(target, "landing.html") {
		t.Fatalf("target = %q", target)
	}

	if _, err := tool.Translate(map[string]any{"action": "open", "path": dir}); err == nil {
		t.Fatal("opening a directory should be refused rather than guessing an index file")
	}
	if _, err := tool.Translate(map[string]any{"action": "open", "path": dir + "/missing.html"}); err == nil {
		t.Fatal("opening a missing file should fail at translate time")
	}
}

// TestBrowserOpenRequiresExactlyOneTarget pins that open is never left without a
// destination or given two conflicting ones.
func TestBrowserOpenRequiresExactlyOneTarget(t *testing.T) {
	tool := NewBrowserTool()
	if _, err := tool.Translate(map[string]any{"action": "open"}); err == nil {
		t.Fatal("open with no target should fail")
	}
	if _, err := tool.Translate(map[string]any{"action": "open", "url": "https://example.com", "path": "a.html"}); err == nil {
		t.Fatal("open with both url and path should fail")
	}
}

// TestBrowserStillRejectsNonWebSchemes pins that adding a local path did not
// loosen which web schemes a browser may be pointed at.
func TestBrowserStillRejectsNonWebSchemes(t *testing.T) {
	tool := NewBrowserTool()
	for _, url := range []string{"javascript:alert(1)", "data:text/html,<b>x", "ftp://host/f"} {
		if _, err := tool.Translate(map[string]any{"action": "open", "url": url}); err == nil {
			t.Errorf("url %q should still be rejected", url)
		}
	}
}

// TestPrefersSystemBrowserOverDownload pins that a browser already on the host is
// used, so the tool does not download a private copy it does not need.
func TestPrefersSystemBrowserOverDownload(t *testing.T) {
	found, err := resolveBrowserBinary(context.Background())
	if err != nil {
		t.Skipf("no browser available on this host: %v", err)
	}
	if _, installed := executableExists(found); !installed {
		// The downloaded fallback is legitimate when nothing is installed.
		t.Skip("no system browser, so the private download is the only option")
	}
	for _, candidate := range systemBrowserCandidates {
		if expected, ok := executableExists(candidate); ok {
			if expected != found {
				t.Fatalf("resolved %q but %q is installed", found, expected)
			}
			t.Logf("using system browser %s", found)
			return
		}
	}
	t.Fatalf("resolved %q but none of the known system paths matched", found)
}

// TestQuestionIsFreeFormWithoutOptions pins that a question with no options is
// answerable in the user's own words, rather than silently becoming Yes or No.
func TestQuestionIsFreeFormWithoutOptions(t *testing.T) {
	tool := &QuestionTool{}
	spec, err := tool.Translate(map[string]any{"question": "Which path should I use?"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Args["free_form"] != true {
		t.Fatalf("a question with no options must be free-form, got %#v", spec.Args)
	}

	chosen, err := tool.Translate(map[string]any{"question": "Ship it?", "options": []any{"Yes", "No"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := chosen.Args["free_form"]; present {
		t.Fatal("explicit options stay button-only unless free_form is set")
	}
	both, err := tool.Translate(map[string]any{"question": "Ship it?", "options": []any{"Yes", "No"}, "free_form": true})
	if err != nil {
		t.Fatal(err)
	}
	if both.Args["free_form"] != true {
		t.Fatal("free_form with options must be honoured, so the options become suggestions")
	}
}

// The browser description used to enumerate all nine actions and what each
// argument belonged to, which the per-argument descriptions already carry. That
// redundancy made browser the most expensive tool in the registry and did not
// stop a model pairing arguments with the wrong action 14 times running, so the
// description now holds only what a schema cannot express: the page is shared
// state across calls.
func TestBrowserDescriptionOmitsTheActionList(t *testing.T) {
	description := (&BrowserTool{}).Description()
	if len(description) > 400 {
		t.Errorf("description is %d chars; the action list belongs in the schema", len(description))
	}
	for _, leaked := range []string{"navigate to a URL", "capture a PNG", "read page HTML", "shut the browser down"} {
		if strings.Contains(description, leaked) {
			t.Errorf("description still enumerates actions (%q)", leaked)
		}
	}
	// The non-obvious behaviour has to survive.
	if !strings.Contains(description, "shared") {
		t.Error("description dropped the shared-page rule, which the schema cannot state")
	}
	// The action enum and per-action argument ownership must still be present,
	// since that is where the model actually reads them.
	schema := (&BrowserTool{}).Schema()
	action, _ := schema["properties"].(map[string]any)["action"].(map[string]any)
	if action == nil {
		t.Fatal("action property missing")
	}
	actions, _ := action["enum"].([]string)
	if len(actions) != 9 {
		t.Errorf("action enum has %d entries, want 9", len(actions))
	}
	properties, _ := schema["properties"].(map[string]any)
	// Every argument must still name the actions it belongs to, since that is
	// where the model reads that. `selector` is shared by five actions, so it
	// lists them rather than using the "action=x only" form.
	for name, markers := range map[string][]string{
		"url":       {"action=open"},
		"script":    {"action=eval"},
		"full_page": {"action=screenshot"},
		"submit":    {"action=type"},
		"selector":  {"screenshot", "click", "type"},
	} {
		property, _ := properties[name].(map[string]any)
		text, _ := property["description"].(string)
		for _, marker := range markers {
			if !strings.Contains(text, marker) {
				t.Errorf("%s description no longer states %q: %q", name, marker, text)
			}
		}
	}
}

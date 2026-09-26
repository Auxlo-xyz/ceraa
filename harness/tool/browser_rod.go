package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"ceraa/internal/paths"
)

// browserDriver is the surface the tool layer needs. Keeping it this narrow is
// what lets the tool be tested without a browser, and keeps the rod specifics in
// one place.
type browserDriver interface {
	Navigate(ctx context.Context, rawURL string) (pageInfo, error)
	Screenshot(ctx context.Context, path string, fullPage bool, selector string) error
	Text(ctx context.Context, selector string) (string, error)
	HTML(ctx context.Context, selector string) (string, error)
	Click(ctx context.Context, selector string) error
	Type(ctx context.Context, selector, text string, submit bool) error
	Eval(ctx context.Context, script string) (string, error)
	Info(ctx context.Context) (pageInfo, error)
	Close() error
}

type pageInfo struct {
	URL    string
	Title  string
	Status int
}

// rodDriver is the real browser, driven by go-rod.
type rodDriver struct {
	browser *rod.Browser
	mu      sync.Mutex
	page    *rod.Page
}

// newRodDriver downloads Chromium on first use and launches it headless.
//
// The binary lives in the Ceraa data directory rather than the user's project or
// rod's default cache, so it is removed with the rest of Ceraa's state and never
// shows up as an untracked file in a repository.
func newRodDriver(ctx context.Context) (browserDriver, error) {
	launchCtx, cancel := context.WithTimeout(ctx, browserLaunchTimeout)
	defer cancel()

	fetcher := launcher.NewBrowser()
	fetcher.RootDir = browserDownloadDir()
	fetcher.Context = launchCtx
	binary, err := fetcher.Get()
	if err != nil {
		return nil, fmt.Errorf("prepare chromium: %w", err)
	}

	control := launcher.New().
		Headless(true).
		// Containers usually run as root without user namespaces, where Chrome's
		// own sandbox cannot start. Shared memory is small in a container, which
		// is why the usual /dev/shm crash needs disabling too.
		NoSandbox(true).
		Set("disable-dev-shm-usage").
		Leakless(false).
		Bin(binary)

	controlURL, err := control.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chromium: %w", err)
	}

	// The launch context is only for starting up. Binding the browser to it would
	// cancel the whole session the moment this function returned.
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connect to chromium: %w", err)
	}
	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = browser.Close()
		return nil, fmt.Errorf("open page: %w", err)
	}
	// rod's per-call default timeouts are unbounded; bound them so one hung page
	// cannot stall a whole run.
	page = page.Timeout(pageActionTimeout)
	return &rodDriver{browser: browser, page: page}, nil
}

// browserDownloadDir keeps the browser binary with the rest of Ceraa's data.
func browserDownloadDir() string {
	return filepath.Join(paths.DataDir(), "browser")
}

func (d *rodDriver) Navigate(ctx context.Context, rawURL string) (pageInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	page := d.page.Context(ctx).Timeout(pageActionTimeout)
	if err := page.Navigate(rawURL); err != nil {
		return pageInfo{}, fmt.Errorf("navigate to %s: %w", rawURL, err)
	}
	// Navigation can resolve before client-side rendering settles, so wait for
	// the document rather than returning a half-built page.
	if err := page.WaitLoad(); err != nil {
		return d.infoLocked()
	}
	d.page = page
	return d.infoLocked()
}

func (d *rodDriver) Info(ctx context.Context) (pageInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.infoLocked()
}

func (d *rodDriver) infoLocked() (pageInfo, error) {
	if d.page == nil {
		return pageInfo{}, fmt.Errorf("browser has no page")
	}
	info, err := d.page.Info()
	if err != nil {
		return pageInfo{}, fmt.Errorf("read page info: %w", err)
	}
	result := pageInfo{}
	if info.URL != "" {
		result.URL = info.URL
	}
	if info.Title != "" {
		result.Title = info.Title
	}
	return result, nil
}

func (d *rodDriver) Screenshot(ctx context.Context, path string, fullPage bool, selector string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.page == nil {
		return fmt.Errorf("browser has no page")
	}
	var (
		png []byte
		err error
	)
	if selector != "" {
		element, findErr := d.page.Element(selector)
		if findErr != nil {
			return fmt.Errorf("find %q: %w", selector, findErr)
		}
		png, err = element.Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
	} else {
		png, err = d.page.Screenshot(fullPage, nil)
	}
	if err != nil {
		return fmt.Errorf("capture screenshot: %w", err)
	}
	if len(png) == 0 {
		return fmt.Errorf("screenshot of %s produced no image", screenshotTarget(selector))
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return fmt.Errorf("write screenshot to %s: %w", path, err)
	}
	return nil
}

func screenshotTarget(selector string) string {
	if selector != "" {
		return selector
	}
	return "the page"
}

func (d *rodDriver) Text(ctx context.Context, selector string) (string, error) {
	element, err := d.resolve(selector)
	if err != nil {
		return "", err
	}
	text, err := element.Text()
	if err != nil {
		return "", fmt.Errorf("read text: %w", err)
	}
	return strings.TrimSpace(text), nil
}

func (d *rodDriver) HTML(ctx context.Context, selector string) (string, error) {
	element, err := d.resolve(selector)
	if err != nil {
		return "", err
	}
	html, err := element.HTML()
	if err != nil {
		return "", fmt.Errorf("read html: %w", err)
	}
	return html, nil
}

func (d *rodDriver) Click(ctx context.Context, selector string) error {
	element, err := d.resolve(selector)
	if err != nil {
		return err
	}
	if err := element.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click %q: %w", selector, err)
	}
	return nil
}

func (d *rodDriver) Type(ctx context.Context, selector, text string, submit bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	element, err := d.resolveLocked(selector)
	if err != nil {
		return err
	}
	if err := element.Input(text); err != nil {
		return fmt.Errorf("type into %q: %w", selector, err)
	}
	if !submit {
		return nil
	}
	// Enter on the focused element submits most forms, including ones with no
	// submit button of their own.
	page := element.Page()
	if err := page.Keyboard.Press(input.Enter); err != nil {
		return fmt.Errorf("submit after typing: %w", err)
	}
	// A submission that navigates is normal; a page that never finishes loading
	// must not fail the call, because the typing itself already succeeded.
	_ = page.WaitLoad()
	return nil
}

func (d *rodDriver) Eval(ctx context.Context, script string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.page == nil {
		return "", fmt.Errorf("browser has no page")
	}
	page := d.page.Context(ctx).Timeout(pageActionTimeout)

	// rod always sends the script as `function() { return (<js>).apply(this, arguments) }`,
	// so a bare expression such as `document.title` would be called as a function
	// and fail. Wrapping it keeps ordinary expressions working, and a caller who
	// does pass a function still gets it called.
	js := script
	if !isFunctionExpression(script) {
		js = fmt.Sprintf("function() { return (function() { return (%s) }).call(this) }", script)
	}
	result, err := page.Evaluate(&rod.EvalOptions{ByValue: true, JS: js})
	if err != nil && js != script {
		// The script was a statement body rather than an expression, so it cannot
		// be parenthesised. Run it as a body instead.
		result, err = page.Evaluate(&rod.EvalOptions{ByValue: true, JS: "function() { " + script + " }"})
	}
	if err != nil {
		return "", fmt.Errorf("evaluate script: %w", err)
	}
	if result.Value.Nil() && result.UnserializableValue != "" {
		return string(result.UnserializableValue), nil
	}
	encoded, err := json.MarshalIndent(result.Value.Val(), "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", result.Value.Val()), nil
	}
	return string(encoded), nil
}

// isFunctionExpression reports whether a script is already callable, so rod's
// function wrapping is what the caller intended.
func isFunctionExpression(script string) bool {
	trimmed := strings.TrimSpace(script)
	return strings.HasPrefix(trimmed, "function") ||
		strings.HasPrefix(trimmed, "async function") ||
		strings.HasPrefix(trimmed, "async (") ||
		strings.HasPrefix(trimmed, "() =>") ||
		strings.HasPrefix(trimmed, "async () =>")
}

func (d *rodDriver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.browser == nil {
		return nil
	}
	browser := d.browser
	d.browser = nil
	d.page = nil
	// A browser that already exited still counts as closed; reporting it would
	// turn routine cleanup into a tool failure.
	_ = browser.Close()
	return nil
}

// resolve finds a single element, or the page body when no selector is given.
func (d *rodDriver) resolve(selector string) (*rod.Element, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resolveLocked(selector)
}

func (d *rodDriver) resolveLocked(selector string) (*rod.Element, error) {
	if d.page == nil {
		return nil, fmt.Errorf("browser has no page")
	}
	if selector == "" {
		return d.page.Element("body")
	}
	element, err := d.page.Element(selector)
	if err != nil {
		return nil, fmt.Errorf("find %q: %w", selector, err)
	}
	return element, nil
}

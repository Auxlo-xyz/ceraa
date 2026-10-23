package tool

// browser_cli.go is the browser the model reaches through bash rather than
// through a tool schema.
//
// The eight browser tools cost 3,051 bytes of catalogue, which is the cached
// prefix of every request, and they taught a one-action-per-call shape that
// makes a flow of six steps six round trips. A shell command is cheaper than a
// schema and can carry a whole flow, and the model already knows how to run one.
//
// The browser itself is not owned by the calling process: a `ceraa browser`
// invocation is a fresh process each time, so it attaches to a detached
// Chromium recorded in a state file. That is what makes `ceraa browser open
// example.com` followed by `ceraa browser text` two commands over one page,
// with the cookies and scroll position from the first still there.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"ceraa/internal/paths"
)

const (
	// browserCLIIdleTimeout retires a browser no shell command has touched, so a
	// forgotten page does not keep a Chromium process alive forever. The tool
	// path does the same in memory; this one cannot, because no process outlives
	// the command.
	browserCLIIdleTimeout = 30 * time.Minute
	browserCLILaunchWait  = 30 * time.Second
	browserCLIOutputLimit = 20_000
)

// browserCLISession is the detached browser a shell command attaches to.
type browserCLISession struct {
	PID      int   `json:"pid"`
	Port     int   `json:"port"`
	LastUsed int64 `json:"last_used"`
	// Viewport is the page size the last command asked for. It lives in the
	// state file because each command is a new process: without it, a shot
	// would fall back to the launcher's default and quietly lose the phone
	// size an earlier command set.
	Viewport *browserCLIViewport `json:"viewport,omitempty"`
}

// browserCLIViewport is the emulated page size, in CSS pixels.
type browserCLIViewport struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Mobile bool    `json:"mobile"`
	DPR    float64 `json:"dpr"`
}

const (
	browserCLIDefaultWidth  = 1280
	browserCLIDefaultHeight = 800
	browserCLIMinDPR        = 0.5
	browserCLIMaxDPR        = 4
	browserCLIMaxViewport   = 10_000
)

func (v browserCLIViewport) resolved() browserCLIViewport {
	if v.Width <= 0 {
		v.Width = browserCLIDefaultWidth
	}
	if v.Height <= 0 {
		v.Height = browserCLIDefaultHeight
	}
	if v.DPR <= 0 {
		v.DPR = 1
	}
	return v
}

// String is what every action prints, so the page size is visible in the output
// rather than inferred from the screenshot.
func (v browserCLIViewport) String() string {
	v = v.resolved()
	mode := "desktop"
	if v.Mobile {
		mode = "mobile"
	}
	return fmt.Sprintf("%dx%d %s dpr %g", v.Width, v.Height, mode, v.DPR)
}

func sessionBrowserCLIViewport(session browserCLISession) browserCLIViewport {
	if session.Viewport == nil {
		return browserCLIViewport{}.resolved()
	}
	return session.Viewport.resolved()
}

// applyBrowserCLIViewport drives the emulation override. Width and height are
// CSS pixels, so a 390x844 phone with --dpr 2 captures at 780x1688.
func applyBrowserCLIViewport(page *rod.Page, viewport browserCLIViewport) error {
	viewport = viewport.resolved()
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             viewport.Width,
		Height:            viewport.Height,
		DeviceScaleFactor: viewport.DPR,
		Mobile:            viewport.Mobile,
	}); err != nil {
		return fmt.Errorf("set viewport %s: %w", viewport, err)
	}
	touchPoints := 5
	if viewport.Mobile {
		if err := (proto.EmulationSetTouchEmulationEnabled{
			Enabled:        true,
			MaxTouchPoints: &touchPoints,
		}).Call(page); err != nil {
			return fmt.Errorf("enable touch: %w", err)
		}
		return nil
	}
	if err := (proto.EmulationSetTouchEmulationEnabled{Enabled: false}).Call(page); err != nil {
		return fmt.Errorf("disable touch: %w", err)
	}
	return nil
}

func browserCLISessionDir() string {
	return filepath.Join(paths.DataDir(), "browser")
}

func browserCLISessionFile() string {
	return filepath.Join(browserCLISessionDir(), "session.json")
}

func browserCLIProfileDir() string {
	return filepath.Join(browserCLISessionDir(), "profile")
}

func loadBrowserCLISession() (browserCLISession, bool) {
	raw, err := os.ReadFile(browserCLISessionFile())
	if err != nil {
		return browserCLISession{}, false
	}
	var session browserCLISession
	if err := json.Unmarshal(raw, &session); err != nil || session.Port == 0 {
		return browserCLISession{}, false
	}
	return session, true
}

func saveBrowserCLISession(session browserCLISession) error {
	if err := os.MkdirAll(browserCLISessionDir(), 0o700); err != nil {
		return fmt.Errorf("create browser state directory: %w", err)
	}
	session.LastUsed = time.Now().Unix()
	raw, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if err := os.WriteFile(browserCLISessionFile(), raw, 0o600); err != nil {
		return fmt.Errorf("write browser state: %w", err)
	}
	return nil
}

// wsURL asks the running browser for its DevTools endpoint. This is the liveness
// check as well as the address: a state file naming a port nothing listens on is
// a browser that died, and the file has to be treated as advice rather than fact.
func (s browserCLISession) wsURL(ctx context.Context) (string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/version", s.Port), nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("browser endpoint answered %d", response.StatusCode)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		return "", fmt.Errorf("read browser endpoint: %w", err)
	}
	if version.WebSocketDebuggerURL == "" {
		return "", errors.New("browser endpoint has no debugger address")
	}
	return version.WebSocketDebuggerURL, nil
}

// stopBrowserCLISession ends the detached browser and forgets it. The state file
// is removed first, so a half-finished stop cannot leave a later command
// attaching to a process that is on its way out.
func stopBrowserCLISession(session browserCLISession) {
	_ = os.Remove(browserCLISessionFile())
	if session.PID <= 0 {
		return
	}
	_ = syscall.Kill(session.PID, syscall.SIGTERM)
	for waited := 0; waited < 30; waited++ {
		if syscall.Kill(session.PID, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(session.PID, syscall.SIGKILL)
}

// launchBrowserCLISession starts Chromium detached and records where to reach it.
func launchBrowserCLISession(ctx context.Context) (browserCLISession, error) {
	binary, err := resolveBrowserBinary(ctx)
	if err != nil {
		return browserCLISession{}, err
	}
	port, err := freeLocalPort()
	if err != nil {
		return browserCLISession{}, err
	}
	if err := os.MkdirAll(browserCLIProfileDir(), 0o700); err != nil {
		return browserCLISession{}, fmt.Errorf("create browser profile: %w", err)
	}
	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--remote-debugging-address=127.0.0.1",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + browserCLIProfileDir(),
		"about:blank",
	}
	command := exec.Command(binary, args...)
	command.Env = browserLaunchEnv()
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logPath := filepath.Join(browserCLISessionDir(), "browser.log")
	if logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); logErr == nil {
		command.Stdout, command.Stderr = logFile, logFile
		defer logFile.Close()
	}
	if err := command.Start(); err != nil {
		return browserCLISession{}, fmt.Errorf("start %s: %w%s", binary, err, browserLibraryHint(err))
	}
	// Release sets Process.Pid to -1, so the pid is read before the process is
	// released to outlive this command.
	pid := command.Process.Pid
	// The browser must outlive this process, so it is released rather than
	// waited on. It is stopped by the next idle sweep or by `ceraa browser close`.
	_ = command.Process.Release()
	session := browserCLISession{PID: pid, Port: port}
	deadline := time.Now().Add(browserCLILaunchWait)
	for time.Now().Before(deadline) {
		if _, err := session.wsURL(ctx); err == nil {
			if err := saveBrowserCLISession(session); err != nil {
				stopBrowserCLISession(session)
				return browserCLISession{}, err
			}
			return session, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	stopBrowserCLISession(session)
	return browserCLISession{}, fmt.Errorf("browser did not answer on port %d within %s", port, browserCLILaunchWait)
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// attachBrowserCLI connects to the recorded browser and returns the page the
// previous command left open, so the sequence is one page rather than one tab
// per call.
func attachBrowserCLI(ctx context.Context, session browserCLISession) (*rodDriver, error) {
	ws, err := session.wsURL(ctx)
	if err != nil {
		return nil, err
	}
	browser := rod.New().ControlURL(ws)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connect to chromium: %w", err)
	}
	pages, err := browser.Pages()
	if err != nil || len(pages) == 0 {
		page, pageErr := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
		if pageErr != nil {
			_ = browser.Close()
			return nil, fmt.Errorf("open page: %w", pageErr)
		}
		return &rodDriver{browser: browser, page: page.Timeout(pageActionTimeout)}, nil
	}
	return &rodDriver{browser: browser, page: pages[len(pages)-1].Timeout(pageActionTimeout)}, nil
}

// ensureBrowserCLI returns a live browser, launching one if the record is stale.
func ensureBrowserCLI(ctx context.Context) (*rodDriver, error) {
	session, ok := loadBrowserCLISession()
	if ok {
		expired := time.Since(time.Unix(session.LastUsed, 0)) > browserCLIIdleTimeout
		if _, err := session.wsURL(ctx); expired || err != nil {
			stopBrowserCLISession(session)
			ok = false
		}
	}
	if !ok {
		var err error
		session, err = launchBrowserCLISession(ctx)
		if err != nil {
			return nil, err
		}
	}
	driver, err := attachBrowserCLI(ctx, session)
	if err != nil {
		// A record that stopped answering between the check and the attach is
		// replaced once. Anything else is a browser that cannot be driven, and
		// retrying that is how a command hangs.
		stopBrowserCLISession(session)
		session, launchErr := launchBrowserCLISession(ctx)
		if launchErr != nil {
			return nil, fmt.Errorf("%w (relaunching the browser also failed: %v)", err, launchErr)
		}
		driver, err = attachBrowserCLI(ctx, session)
		if err != nil {
			return nil, err
		}
	}
	if err := applyBrowserCLIViewport(driver.page, sessionBrowserCLIViewport(session)); err != nil {
		return nil, err
	}
	_ = saveBrowserCLISession(session)
	return driver, nil
}

// RunBrowserCLI performs one browser action for the `ceraa browser` command.
func RunBrowserCLI(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New(browserCLIUsage())
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]

	switch action {
	case "help", "-h", "--help":
		fmt.Fprint(out, browserCLIUsage())
		return nil
	case "status":
		return browserCLIStatus(ctx, out)
	case "close":
		session, ok := loadBrowserCLISession()
		if !ok {
			fmt.Fprintln(out, "no browser is open")
			return nil
		}
		stopBrowserCLISession(session)
		fmt.Fprintln(out, "browser closed")
		return nil
	}

	// The action is validated before a browser is ensured, so a typo is an
	// error rather than a download and a 30 second launch.
	if !browserCLIActions[action] {
		return fmt.Errorf("unknown browser action %q\n\n%s", action, browserCLIUsage())
	}

	if action == "open" {
		return browserCLIOpen(ctx, out, rest)
	}

	driver, err := ensureBrowserCLI(ctx)
	if err != nil {
		return err
	}

	switch action {
	case "snapshot":
		return browserCLISnapshot(ctx, out, driver, rest)
	case "text":
		return browserCLIText(ctx, out, driver, rest)
	case "html":
		return browserCLIHTML(ctx, out, driver, rest)
	case "click":
		return browserCLIClick(ctx, out, driver, rest)
	case "type":
		return browserCLIType(ctx, out, driver, rest)
	case "eval":
		return browserCLIEval(ctx, out, driver, rest)
	case "shot":
		return browserCLIShot(ctx, out, driver, rest)
	case "viewport":
		return browserCLISetViewport(ctx, out, driver, rest)
	}
	return fmt.Errorf("unknown browser action %q\n\n%s", action, browserCLIUsage())
}

// browserCLIActions is every action the dispatcher handles.
var browserCLIActions = map[string]bool{
	"open": true, "snapshot": true, "text": true, "html": true, "click": true,
	"type": true, "eval": true, "shot": true, "status": true, "close": true,
	"viewport": true,
}

func browserCLIUsage() string {
	return `usage: ceraa browser <action> [arguments]

Actions:
  open <url> [--wait <ms>]         Open a web page (http or https only).
  open --file <path>               Open a local file.
  snapshot [--max <n>]             Page text plus @refs for clickable controls.
  text [selector]                  Visible text of the page or one element.
  html [selector]                  HTML of the page or one element.
  click <@ref|selector>            Click one element.
  type <@ref|selector> <text>...   Type into one element. --submit presses Enter.
  eval <javascript>                Run JavaScript in the page, result as JSON.
  shot [path] [--full] [--wait n]  Screenshot the page. --selector crops one element.
  viewport [<WxH>]                 Show or change the emulated page size.
  status                           Report the browser, page and viewport.
  close                            Close the browser.

Viewport flags, accepted by open, shot and viewport:
  --viewport 390x844   --width 390   --height 844   --dpr 2
  --mobile             emulates a phone (touch plus mobile layout)
  --desktop            the opposite of --mobile
The size is remembered for later commands. A 390x844 phone at --dpr 2
captures at 780x1688 pixels.

The page persists between commands, so a ref read from snapshot works in a
later command until the page changes.`
}

// browserCLIStatus never launches a browser just to report that none is running.
func browserCLIStatus(ctx context.Context, out io.Writer) error {
	session, ok := loadBrowserCLISession()
	if !ok {
		fmt.Fprintln(out, "no browser is open")
		return nil
	}
	if _, err := session.wsURL(ctx); err != nil {
		stopBrowserCLISession(session)
		fmt.Fprintln(out, "no browser is open")
		return nil
	}
	driver, err := attachBrowserCLI(ctx, session)
	if err != nil {
		return err
	}
	info, err := driver.Info(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "browser is open")
	fmt.Fprintln(out, formatPageInfo(info))
	fmt.Fprintf(out, "viewport: %s\n", sessionBrowserCLIViewport(session))
	return nil
}

func browserCLIOpen(ctx context.Context, out io.Writer, args []string) error {
	rest, flags, err := splitBrowserCLIFlags(args,
		[]string{"mobile", "desktop"},
		[]string{"file", "wait", "viewport", "width", "height", "dpr"})
	if err != nil {
		return err
	}
	target, err := browserCLIOpenTarget(rest, flags.value("file"))
	if err != nil {
		return err
	}
	wait, err := flags.intValue("wait")
	if err != nil {
		return err
	}
	driver, err := ensureBrowserCLI(ctx)
	if err != nil {
		return err
	}
	viewport, err := browserCLIApplyViewportFlags(driver, flags)
	if err != nil {
		return err
	}
	info, err := driver.Navigate(ctx, target)
	if err != nil {
		return err
	}
	sleepMillis(wait)
	fmt.Fprintln(out, formatPageInfo(info))
	fmt.Fprintf(out, "viewport: %s\n", viewport)
	return nil
}

// browserCLIOpenTarget resolves the target the same way the tool does, so a
// file: or javascript: URL is refused here for the same reason.
func browserCLIOpenTarget(rest []string, file string) (string, error) {
	switch {
	case file != "" && len(rest) > 0:
		return "", errors.New("open takes either a url or --file, not both")
	case file != "":
		return localFileURL(file)
	case len(rest) == 1:
		target := strings.TrimSpace(rest[0])
		if err := validateBrowserURL(target); err != nil {
			return "", err
		}
		return target, nil
	case len(rest) == 0:
		return "", errors.New("open requires a url, or --file with a path")
	default:
		return "", fmt.Errorf("open takes one url, got %d arguments", len(rest))
	}
}

func browserCLIText(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	selector, err := browserCLISelectorArg("text", args)
	if err != nil {
		return err
	}
	text, err := driver.Text(ctx, selector)
	if err != nil {
		return err
	}
	writeBounded(out, text)
	return nil
}

func browserCLIHTML(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	selector, err := browserCLISelectorArg("html", args)
	if err != nil {
		return err
	}
	html, err := driver.HTML(ctx, selector)
	if err != nil {
		return err
	}
	writeBounded(out, html)
	return nil
}

func browserCLISelectorArg(action string, args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("%s takes at most one selector", action)
	}
	if len(args) == 0 {
		return "", nil
	}
	return browserCLITarget(args[0]), nil
}

func browserCLIClick(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	if len(args) != 1 {
		return errors.New("click requires one @ref or CSS selector")
	}
	selector := browserCLITarget(args[0])
	if err := driver.Click(ctx, selector); err != nil {
		return browserCLIRefHint(args[0], driver, err)
	}
	fmt.Fprintf(out, "clicked %s\n", args[0])
	if info, err := driver.Info(ctx); err == nil {
		fmt.Fprintln(out, formatPageInfo(info))
	}
	return nil
}

func browserCLIType(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	rest, flags, err := splitBrowserCLIFlags(args, []string{"submit"}, nil)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errors.New("type requires an @ref or selector and the text to type")
	}
	selector := browserCLITarget(rest[0])
	text := strings.Join(rest[1:], " ")
	if err := driver.Type(ctx, selector, text, flags.has("submit")); err != nil {
		return browserCLIRefHint(rest[0], driver, err)
	}
	fmt.Fprintf(out, "typed into %s\n", rest[0])
	return nil
}

func browserCLIEval(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	if len(args) == 0 {
		return errors.New("eval requires a JavaScript expression")
	}
	result, err := driver.Eval(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	writeBounded(out, result)
	return nil
}

func browserCLIShot(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	rest, flags, err := splitBrowserCLIFlags(args,
		[]string{"full", "mobile", "desktop"},
		[]string{"selector", "wait", "viewport", "width", "height", "dpr"})
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return errors.New("shot takes at most one output path")
	}
	path := defaultScreenshotPath()
	if len(rest) == 1 {
		path = rest[0]
	}
	path, err = ResolvePath(path)
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	wait, err := flags.intValue("wait")
	if err != nil {
		return err
	}
	viewport, err := browserCLIApplyViewportFlags(driver, flags)
	if err != nil {
		return err
	}
	sleepMillis(wait)
	width, height, err := browserCLICapture(ctx, driver, path, flags.has("full"), browserCLITarget(flags.value("selector")))
	if err != nil {
		return err
	}
	info, statErr := os.Stat(path)
	size := int64(0)
	if statErr == nil {
		size = info.Size()
	}
	fmt.Fprintf(out, "screenshot saved to %s (%dx%d px, %s, viewport %s)\n",
		path, width, height, humanBytes(size), viewport)
	return nil
}

// browserCLICapture writes the page to path and reports the pixel size, which
// is the only way a caller can tell a phone capture from a desktop one.
func browserCLICapture(ctx context.Context, driver *rodDriver, path string, fullPage bool, selector string) (int, int, error) {
	page := driver.actionPage(ctx)
	var (
		raw []byte
		err error
	)
	switch {
	case selector != "":
		element, findErr := page.Element(selector)
		if findErr != nil {
			return 0, 0, fmt.Errorf("find %q: %w", selector, findErr)
		}
		raw, err = element.Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
	case fullPage:
		// A clip against the content size captures the whole page without
		// resizing the viewport, so a mobile override survives the shot. rod's
		// own full-page path rewrites the metrics and then clears them.
		metrics, metricErr := proto.PageGetLayoutMetrics{}.Call(page)
		if metricErr != nil {
			return 0, 0, fmt.Errorf("measure page: %w", metricErr)
		}
		if metrics.CSSContentSize == nil {
			return 0, 0, errors.New("page has no content size")
		}
		raw, err = page.Screenshot(false, &proto.PageCaptureScreenshot{
			Format: proto.PageCaptureScreenshotFormatPng,
			Clip: &proto.PageViewport{
				X:      0,
				Y:      0,
				Width:  metrics.CSSContentSize.Width,
				Height: metrics.CSSContentSize.Height,
				Scale:  1,
			},
			FromSurface:           true,
			CaptureBeyondViewport: true,
		})
	default:
		raw, err = page.Screenshot(false, nil)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("capture screenshot: %w", err)
	}
	if len(raw) == 0 {
		return 0, 0, fmt.Errorf("screenshot of %s produced no image", screenshotTarget(selector))
	}
	config, _, decodeErr := image.DecodeConfig(bytes.NewReader(raw))
	if decodeErr != nil {
		return 0, 0, fmt.Errorf("read screenshot size: %w", decodeErr)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return 0, 0, fmt.Errorf("write screenshot to %s: %w", path, err)
	}
	return config.Width, config.Height, nil
}

func humanBytes(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// browserCLISetViewport changes the emulated size of the page already open.
func browserCLISetViewport(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	rest, flags, err := splitBrowserCLIFlags(args, []string{"mobile", "desktop"},
		[]string{"viewport", "width", "height", "dpr"})
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return errors.New("viewport takes at most one size, written as 390x844")
	}
	if len(rest) == 1 {
		if flags.has("viewport") {
			return errors.New("viewport takes the size once, either as an argument or with --viewport")
		}
		flags["viewport"] = rest[0]
	}
	viewport, err := browserCLIApplyViewportFlags(driver, flags)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "viewport: %s\n", viewport)
	return nil
}

// browserCLIApplyViewportFlags resolves the viewport flags shared by open, shot
// and viewport, applies a change to the live page and records it in the state
// file. With no flags it re-applies the remembered size, which is what keeps a
// later command in the same phone size as the one that set it.
func browserCLIApplyViewportFlags(driver *rodDriver, flags browserCLIFlags) (browserCLIViewport, error) {
	session, ok := loadBrowserCLISession()
	if !ok {
		return browserCLIViewport{}.resolved(), errors.New("browser state is missing")
	}
	current := sessionBrowserCLIViewport(session)
	next, changed, err := browserCLIViewportFromFlags(current, flags)
	if err != nil {
		return current, err
	}
	if err := applyBrowserCLIViewport(driver.page, next); err != nil {
		return current, err
	}
	if changed {
		session.Viewport = &next
		if err := saveBrowserCLISession(session); err != nil {
			return current, err
		}
	}
	return next, nil
}

// browserCLIViewportFromFlags reads --viewport/--width/--height/--dpr/--mobile
// over the current size. It returns whether anything was asked for, so an
// unchanged command does not rewrite the state file.
func browserCLIViewportFromFlags(current browserCLIViewport, flags browserCLIFlags) (browserCLIViewport, bool, error) {
	next := current.resolved()
	changed := false
	if raw := flags.value("viewport"); raw != "" {
		width, height, err := parseBrowserCLIViewport(raw)
		if err != nil {
			return current, false, err
		}
		next.Width, next.Height = width, height
		changed = true
	}
	if raw := flags.value("width"); raw != "" {
		width, err := parseBrowserCLIDimension("width", raw)
		if err != nil {
			return current, false, err
		}
		next.Width = width
		changed = true
	}
	if raw := flags.value("height"); raw != "" {
		height, err := parseBrowserCLIDimension("height", raw)
		if err != nil {
			return current, false, err
		}
		next.Height = height
		changed = true
	}
	if raw := flags.value("dpr"); raw != "" {
		ratio, err := strconv.ParseFloat(raw, 64)
		if err != nil || ratio < browserCLIMinDPR || ratio > browserCLIMaxDPR {
			return current, false, fmt.Errorf("--dpr must be between %g and %g, got %q", float64(browserCLIMinDPR), float64(browserCLIMaxDPR), raw)
		}
		next.DPR = ratio
		changed = true
	}
	mobile, desktop := flags.has("mobile"), flags.has("desktop")
	switch {
	case mobile && desktop:
		return current, false, errors.New("--mobile and --desktop are exclusive")
	case mobile:
		next.Mobile = true
		changed = true
	case desktop:
		next.Mobile = false
		changed = true
		// A phone capture at dpr 2 is deliberate; carrying that ratio into a
		// desktop capture is not, and it doubles the image for no reason.
		if flags.value("dpr") == "" {
			next.DPR = 1
		}
	}
	return next.resolved(), changed, nil
}

func parseBrowserCLIViewport(raw string) (int, int, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	for _, separator := range []string{"x", "*", "×"} {
		if left, right, ok := strings.Cut(normalized, separator); ok {
			width, err := parseBrowserCLIDimension("viewport", strings.TrimSpace(left))
			if err != nil {
				return 0, 0, err
			}
			height, err := parseBrowserCLIDimension("viewport", strings.TrimSpace(right))
			if err != nil {
				return 0, 0, err
			}
			return width, height, nil
		}
	}
	return 0, 0, fmt.Errorf("--viewport must be written as WIDTHxHEIGHT, e.g. 390x844, got %q", raw)
}

func parseBrowserCLIDimension(name, raw string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 || value > browserCLIMaxViewport {
		return 0, fmt.Errorf("--%s must be a number between 1 and %d, got %q", name, browserCLIMaxViewport, raw)
	}
	return value, nil
}

// browserCLISnapshot renders the page the way a model reads it: a bounded text
// body plus numbered controls, so a later command names an element by @N
// instead of by a selector that a class rename breaks.
func browserCLISnapshot(ctx context.Context, out io.Writer, driver *rodDriver, args []string) error {
	rest, flags, err := splitBrowserCLIFlags(args, nil, []string{"max"})
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("snapshot takes no positional arguments")
	}
	maxRefs := 60
	if raw := flags.value("max"); raw != "" {
		maxRefs, err = strconv.Atoi(raw)
		if err != nil || maxRefs <= 0 {
			return fmt.Errorf("--max must be a positive number, got %q", raw)
		}
	}
	script := fmt.Sprintf(`function() {
  const limit = %d;
  document.querySelectorAll('[data-ceraa-ref]').forEach(e => e.removeAttribute('data-ceraa-ref'));
  const selector = 'a,button,input,select,textarea,summary,[role="button"],[role="link"],[role="tab"],[role="menuitem"],[role="checkbox"],[contenteditable="true"]';
  const items = [];
  let n = 0;
  for (const el of document.querySelectorAll(selector)) {
    const rect = el.getBoundingClientRect();
    const style = getComputedStyle(el);
    if (rect.width === 0 || rect.height === 0) continue;
    if (style.display === 'none' || style.visibility === 'hidden') continue;
    n += 1;
    el.setAttribute('data-ceraa-ref', String(n));
    const label = (el.innerText || el.value || el.getAttribute('aria-label') ||
      el.getAttribute('placeholder') || el.getAttribute('name') || el.getAttribute('title') || '')
      .trim().replace(/\s+/g, ' ').slice(0, 80);
    items.push({ref: n, role: (el.getAttribute('role') || el.tagName.toLowerCase()), label});
    if (n >= limit) break;
  }
  const body = document.body ? document.body.innerText : '';
  return {
    url: location.href,
    title: document.title,
    text: body.replace(/\n{3,}/g, '\n\n').trim().slice(0, 4000),
    items: items,
    total: n,
  };
}`, maxRefs)
	result, err := driver.Eval(ctx, script)
	if err != nil {
		return err
	}
	var snapshot struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Text  string `json:"text"`
		Items []struct {
			Ref   int    `json:"ref"`
			Role  string `json:"role"`
			Label string `json:"label"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(result), &snapshot); err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "url: %s\n", snapshot.URL)
	fmt.Fprintf(&builder, "title: %s\n", snapshot.Title)
	if len(snapshot.Items) == 0 {
		builder.WriteString("@refs: none\n")
	} else {
		fmt.Fprintf(&builder, "@refs: %d controls (act with click @N or type @N)\n", len(snapshot.Items))
		for _, item := range snapshot.Items {
			fmt.Fprintf(&builder, "@%d [%s] %q\n", item.Ref, item.Role, item.Label)
		}
	}
	if snapshot.Text != "" {
		builder.WriteString("\ntext:\n")
		builder.WriteString(snapshot.Text)
		builder.WriteString("\n")
	}
	writeBounded(out, builder.String())
	return nil
}

// browserCLITarget turns a snapshot ref into the selector the driver can use.
func browserCLITarget(raw string) string {
	target := strings.TrimSpace(raw)
	if strings.HasPrefix(target, "@") {
		return fmt.Sprintf(`[data-ceraa-ref="%s"]`, strings.TrimPrefix(target, "@"))
	}
	return target
}

// browserCLIRefHint explains a stale ref by asking for a new snapshot, since the
// page changed after the one that produced the number.
func browserCLIRefHint(raw string, driver *rodDriver, err error) error {
	if !strings.HasPrefix(strings.TrimSpace(raw), "@") {
		return err
	}
	return fmt.Errorf("%w\nthe page changed since that snapshot; run `ceraa browser snapshot` and use a current @ref", err)
}

func sleepMillis(milliseconds int) {
	if milliseconds > 0 {
		time.Sleep(time.Duration(milliseconds) * time.Millisecond)
	}
}

func writeBounded(out io.Writer, text string) {
	if len(text) > browserCLIOutputLimit {
		text = text[:browserCLIOutputLimit] + "\n… truncated"
	}
	fmt.Fprintln(out, text)
}

// parseBrowserRef is used by tests and the ref hint to keep the numeric part
// honest.
func parseBrowserRef(raw string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

// browserCLIFlags holds the flags parsed out of an action's arguments.
type browserCLIFlags map[string]string

func (f browserCLIFlags) value(name string) string { return f[name] }

func (f browserCLIFlags) has(name string) bool {
	_, ok := f[name]
	return ok
}

func (f browserCLIFlags) intValue(name string) (int, error) {
	raw := f[name]
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > 60_000 {
		return 0, fmt.Errorf("--%s must be between 0 and 60000, got %q", name, raw)
	}
	return value, nil
}

// splitBrowserCLIFlags pulls known flags out of an argument list wherever they
// appear, so `open https://x --wait 500` and `open --wait 500 https://x` are the
// same call. The stdlib flag package stops at the first positional, which made
// the documented order fail on the one that reads naturally.
func splitBrowserCLIFlags(args []string, boolFlags, valueFlags []string) ([]string, browserCLIFlags, error) {
	boolSet := make(map[string]bool, len(boolFlags))
	for _, name := range boolFlags {
		boolSet[name] = true
	}
	valueSet := make(map[string]bool, len(valueFlags))
	for _, name := range valueFlags {
		valueSet[name] = true
	}
	positional := make([]string, 0, len(args))
	flags := browserCLIFlags{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, inline, hasInline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch {
		case boolSet[name]:
			flags[name] = "true"
		case valueSet[name]:
			if hasInline {
				flags[name] = inline
				continue
			}
			if index+1 >= len(args) {
				return nil, nil, fmt.Errorf("--%s needs a value", name)
			}
			index++
			flags[name] = args[index]
		default:
			return nil, nil, fmt.Errorf("unknown flag %q for this action", arg)
		}
	}
	return positional, flags, nil
}

// Package codexauth implements the Codex OAuth device login flow, presenting the
// authorization URL and user code through Telegram.
//
// The flow is driven by the `codex` CLI, which is spawned once for the login and
// then polled for status. Both commands run through an injectable seam, because
// the flow's real difficulty is concurrency and timing, and that is exactly what
// cannot be tested against a real binary or a real network.
package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"ceraa/internal/secret"
)

const (
	// deviceURL is the page a user opens when the CLI did not print one.
	deviceURL = "https://chatgpt.com/codex/device-auth"
	// authLifetime bounds a flow. The upstream device code expires too, so there
	// is no point waiting longer.
	authLifetime = 15 * time.Minute
	// firstPoll is short so the user code reaches the chat quickly, then the
	// interval relaxes to avoid spawning a process every few seconds for a quarter
	// of an hour.
	firstPoll = 1 * time.Second
	pollStep  = 5 * time.Second
	// codexBinary is the CLI that performs the device handshake.
	codexBinary = "codex"
)

var (
	userCodeRe = regexp.MustCompile(`\b[A-Z0-9]{4,8}-[A-Z0-9]{4,8}\b`)
	authURLRe  = regexp.MustCompile(`https://(?:chatgpt\.com|auth\.openai\.com)/\S*device\S*`)
)

// Status values for a flow.
const (
	StatusWaiting   = "waiting"
	StatusChecking  = "checking"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// loginProcess is the long-running device-auth command.
type loginProcess interface {
	// Output returns everything the process has produced so far.
	Output() string
	// Wait blocks until the process exits, returning its error.
	Wait() error
	// Kill terminates it.
	Kill() error
}

// Runner starts the device-auth command and asks for login status. Both are
// seams so the flow can be tested without the CLI or a network.
type Runner interface {
	// StartDeviceAuth begins a device login and returns once the process is
	// running. It must not wait for the process to finish.
	StartDeviceAuth(ctx context.Context) (loginProcess, error)
	// LoginStatus reports the current authentication state as the CLI words it.
	LoginStatus(ctx context.Context) (string, error)
}

// AuthState tracks one in-progress or finished device login.
type AuthState struct {
	mu sync.Mutex

	flowID    string
	UserCode  string
	AuthURL   string
	Status    string
	Message   string
	ExpiresAt time.Time

	process loginProcess
	cancel  context.CancelFunc
	done    chan struct{}
	// finished guards the close, so a cancel after a success cannot panic. The
	// flow is reachable from a Telegram button, and a double close would take the
	// whole bot down.
	finished bool
}

// Manager coordinates flows.
type Manager struct {
	mu     sync.Mutex
	flows  map[string]*AuthState
	runner Runner
	// firstPoll and pollStep are fields rather than constants so the cadence is
	// inspectable and a test can exercise the loop without waiting seconds per
	// tick. Zero means the default.
	firstPoll time.Duration
	pollStep  time.Duration
}

// NewManager creates a manager that drives the real Codex CLI.
func NewManager() *Manager {
	return &Manager{flows: make(map[string]*AuthState), runner: execRunner{}}
}

// NewManagerWithRunner creates a manager with an injected runner, for tests.
func NewManagerWithRunner(runner Runner) *Manager {
	return &Manager{flows: make(map[string]*AuthState), runner: runner}
}

// CodexAvailable reports whether the CLI can be found, so a caller can say
// something useful before a user starts a flow that cannot work.
func CodexAvailable() error {
	if _, err := exec.LookPath(codexBinary); err != nil {
		return CodexUnavailable()
	}
	return nil
}

// CodexUnavailable is the actionable message for a host without the CLI. A login
// cannot work without it, and a raw exec error tells the user nothing.
func CodexUnavailable() error {
	return fmt.Errorf("the %q CLI is not installed on this host, so device login cannot start; "+
		"install it and restart Ceraa, or run the login from a machine that has it", codexBinary)
}

// Start begins a device login and returns as soon as the process is running.
//
// It deliberately does not wait for the process. The CLI prints a code and then
// blocks until a human authorizes it, which is minutes; blocking here would mean
// the user never saw the code until the flow had already timed out.
func (m *Manager) Start() (*AuthState, error) {
	ctx, cancel := context.WithCancel(context.Background())
	process, err := m.runner.StartDeviceAuth(ctx)
	if err != nil {
		cancel()
		if errors.Is(err, errNoCodexBinary) {
			return nil, fmt.Errorf("start codex login: %w", CodexUnavailable())
		}
		return nil, fmt.Errorf("start codex login: %w", err)
	}

	state := &AuthState{
		flowID:    fmt.Sprintf("codex-%d", time.Now().UnixNano()),
		Status:    StatusWaiting,
		Message:   "Waiting for the authorization code",
		ExpiresAt: time.Now().Add(authLifetime),
		cancel:    cancel,
		process:   process,
		done:      make(chan struct{}),
	}
	state.refreshDetails()

	m.mu.Lock()
	m.flows[state.flowID] = state
	m.mu.Unlock()

	go m.watch(state)
	return state, nil
}

// watch polls until the login succeeds, the flow expires, or it is cancelled.
func (m *Manager) watch(state *AuthState) {
	first, step := m.pollCadence()
	interval := first
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-state.done:
			return
		case <-ticker.C:
			if interval < step {
				interval = step
				ticker.Reset(interval)
			}
			if state.isExpired() {
				m.finish(state, StatusFailed, "Authorization expired; start a new login")
				return
			}
			// The CLI prints the code shortly after starting, so re-reading it
			// each poll keeps the chat accurate without another goroutine.
			state.refreshDetails()

			state.setStatus(StatusChecking, "Checking with Codex")
			output, err := m.runner.LoginStatus(context.Background())
			if err != nil {
				state.setStatus(StatusWaiting, "Waiting for authorization")
				continue
			}
			switch classifyStatus(output) {
			case statusLoggedIn:
				// The marker is written before the flow is marked finished, so a
				// status check that races the completion still sees it.
				if secretErr := secret.Set("codex_authenticated", "true"); secretErr != nil {
					m.finish(state, StatusFailed, "Logged in, but the credential marker could not be stored: "+secretErr.Error())
					return
				}
				m.finish(state, StatusSucceeded, "Login successful")
				return
			case statusLoggedOut:
				state.setStatus(StatusWaiting, "Waiting for authorization")
			default:
				state.setStatus(StatusWaiting, "Waiting for authorization")
			}
		}
	}
}

func (m *Manager) pollCadence() (first, step time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	first, step = m.firstPoll, m.pollStep
	if first <= 0 {
		first = firstPoll
	}
	if step <= 0 {
		step = pollStep
	}
	return first, step
}

type authStatus int

const (
	statusUnknown authStatus = iota
	statusLoggedIn
	statusLoggedOut
)

// classifyStatus reads the CLI's wording. Treated as unknown unless it clearly
// says one thing or the other, so an unrecognised phrasing keeps waiting rather
// than reporting a login that did not happen.
func classifyStatus(output string) authStatus {
	lowered := strings.ToLower(output)
	switch {
	case strings.Contains(lowered, "not logged in"), strings.Contains(lowered, "not authenticated"), strings.Contains(lowered, "no credentials"):
		return statusLoggedOut
	case strings.Contains(lowered, "logged in") && !strings.Contains(lowered, "not logged in"):
		return statusLoggedIn
	}
	return statusUnknown
}

// finish marks a flow terminal and releases the process exactly once.
func (m *Manager) finish(state *AuthState, status, message string) {
	state.mu.Lock()
	if state.finished {
		state.mu.Unlock()
		return
	}
	state.finished = true
	state.Status = status
	state.Message = message
	if state.cancel != nil {
		state.cancel()
	}
	if state.process != nil {
		_ = state.process.Kill()
	}
	close(state.done)
	state.mu.Unlock()

	m.mu.Lock()
	delete(m.flows, state.flowID)
	m.mu.Unlock()
}

// Cancel terminates an in-progress login.
//
// It is idempotent and safe after completion. The cancel button stays on screen
// after a flow ends, so a second tap is a normal thing for a user to do, not an
// error to report, and closing an already-closed channel would take the bot down.
// Check stays strict, because an unknown id there is a real mistake.
func (m *Manager) Cancel(flowID string) error {
	state, err := m.lookup(flowID)
	if err != nil {
		return nil
	}
	m.finish(state, StatusCanceled, "Login cancelled")
	return nil
}

func (m *Manager) lookup(flowID string) (*AuthState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.flows[flowID]
	if !ok {
		return nil, fmt.Errorf("flow %q not found", flowID)
	}
	return state, nil
}

// ListFlows returns flows that are still live, which is what a status check wants.
func (m *Manager) ListFlows() []*AuthState {
	m.mu.Lock()
	defer m.mu.Unlock()
	flows := make([]*AuthState, 0, len(m.flows))
	for _, state := range m.flows {
		flows = append(flows, state)
	}
	return flows
}

// Check returns a flow's current state.
func (m *Manager) Check(flowID string) (*AuthState, error) {
	return m.lookup(flowID)
}

// isExpired reports whether a flow has outlived its authorization window. It
// takes the lock, because the expiry is read from the polling goroutine while
// the flow is otherwise being written.
func (s *AuthState) isExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().After(s.ExpiresAt)
}

// refreshDetails pulls the user code and URL out of whatever the CLI has printed
// so far. A code printed after the first read is picked up on a later poll.
func (s *AuthState) refreshDetails() {
	if s.process == nil {
		return
	}
	output := stripVTControlCharacters(s.process.Output())
	if output == "" {
		return
	}
	code, url := parseLoginDetails(output)
	s.mu.Lock()
	if code != "" {
		s.UserCode = code
	}
	if url != "" {
		s.AuthURL = url
	} else if s.AuthURL == "" && s.UserCode != "" {
		s.AuthURL = deviceURL
	}
	s.mu.Unlock()
}

func (s *AuthState) setStatus(status, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.Status = status
	s.Message = message
}

// parseLoginDetails extracts the code and URL from CLI output. The code is
// matched on its own so a line of unrelated text cannot be mistaken for one.
func parseLoginDetails(output string) (code, url string) {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if url == "" {
			if match := authURLRe.FindString(trimmed); match != "" {
				url = match
			}
		}
		if code == "" {
			if match := userCodeRe.FindString(trimmed); match != "" {
				code = match
			}
		}
	}
	return code, url
}

// InlineKeyboardJSON returns the Telegram keyboard for a flow.
func (s *AuthState) InlineKeyboardJSON() ([]byte, error) {
	s.mu.Lock()
	flowID, authURL := s.flowID, s.AuthURL
	s.mu.Unlock()
	if authURL == "" {
		authURL = deviceURL
	}
	return json.Marshal(map[string]any{
		"inline_keyboard": [][]map[string]any{
			{{"text": "🔗 Open Authorization URL", "url": authURL}},
			{
				{"text": "✓ Check Login Status", "callback_data": "codex:check:" + flowID},
				{"text": "✗ Cancel", "callback_data": "codex:cancel:" + flowID},
			},
		},
	})
}

// Snapshot is a consistent copy of the state fields, for callers that need to
// read several of them together.
type Snapshot struct {
	FlowID    string
	UserCode  string
	AuthURL   string
	Status    string
	Message   string
	ExpiresAt time.Time
}

// Snapshot returns the current state.
//
// The accessors below all take the lock, so a caller must never hold it across
// one of them; the lock is not reentrant and doing so deadlocks. This exists so
// reading the whole state at once does not tempt anyone into that.
func (s *AuthState) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		FlowID:    s.flowID,
		UserCode:  s.UserCode,
		AuthURL:   s.AuthURL,
		Status:    s.Status,
		Message:   s.Message,
		ExpiresAt: s.ExpiresAt,
	}
}

// ResultMessage renders the flow state for a chat.
func (s *AuthState) ResultMessage() string {
	return renderStatus(s.Snapshot())
}

// renderStatus is separated so rendering never has to reason about locking.
func renderStatus(state Snapshot) string {
	switch state.Status {
	case StatusSucceeded:
		return fmt.Sprintf("✅ Codex login successful!\nUser code: %s\n\nCodex can now be used as an agent.", state.UserCode)
	case StatusFailed:
		return fmt.Sprintf("❌ Login failed: %s", state.Message)
	case StatusCanceled:
		return "✗ Login cancelled."
	case StatusChecking:
		return fmt.Sprintf("🔄 Checking…\nCode: <code>%s</code>", state.UserCode)
	default:
		lines := []string{"🔐 Codex Device Login", ""}
		if state.AuthURL != "" {
			lines = append(lines, "1. Open: "+state.AuthURL)
		} else {
			lines = append(lines, "1. Open: "+deviceURL)
		}
		if state.UserCode != "" {
			lines = append(lines, "2. Enter code: <code>"+state.UserCode+"</code>")
		} else {
			lines = append(lines, "2. Waiting for the code from the Codex CLI…")
		}
		lines = append(lines, "3. Tap 'Check Login Status' after authorizing")
		return strings.Join(lines, "\n")
	}
}

// ansiSequence matches a terminal escape sequence, which the CLI uses for colour.
// Only dropping the control character is not enough: what follows the escape is
// ordinary text, and leaving it in place glues onto the authorization code and
// hides it from the parser.
var ansiSequence = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07]*\x07|[@-Z\\-_])`)

// stripVTControlCharacters removes terminal escapes and other control characters
// so the CLI's drawing does not end up in a chat message.
func stripVTControlCharacters(s string) string {
	cleaned := ansiSequence.ReplaceAllString(s, "")
	var buf bytes.Buffer
	for _, r := range cleaned {
		switch r {
		case '\n', '\t', '\r':
			buf.WriteRune(r)
			continue
		}
		if r < 32 || r == 0x7f {
			continue
		}
		buf.WriteRune(r)
	}
	return buf.String()
}

// errNoCodexBinary is what a missing CLI looks like to the runner.
var errNoCodexBinary = errors.New("codex CLI not found")

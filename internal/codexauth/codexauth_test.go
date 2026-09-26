package codexauth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// fakeRunner stands in for the Codex CLI.
//
// The login is represented as a process that stays running and produces its code
// after a delay, which is what the real one does: it prints a code and then blocks
// until a human authorizes it.
type fakeRunner struct {
	mu sync.Mutex
	// loginOutput is what the login command prints, delivered after loginDelay.
	loginOutput string
	loginDelay  time.Duration
	// statusOutput is what the status command reports.
	statusOutput string
	// statusAfter makes the status report "logged in" only after N calls, which is
	// how a human authorizing partway through is modelled.
	statusAfter int

	loginCalls  int
	statusCalls int
	process     *fakeProcess
	killed      bool
}

type fakeProcess struct {
	mu     sync.Mutex
	buf    strings.Builder
	closed bool
	killed bool
}

func (p *fakeProcess) Output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

func (p *fakeProcess) Wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killed = true
	return nil
}

func (f *fakeRunner) StartDeviceAuth(ctx context.Context) (loginProcess, error) {
	f.mu.Lock()
	f.loginCalls++
	process := &fakeProcess{}
	f.process = process
	output, delay := f.loginOutput, f.loginDelay
	f.mu.Unlock()

	// Delivered asynchronously, because the caller must be able to read an empty
	// output and get a code later.
	go func() {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		}
		process.mu.Lock()
		process.buf.WriteString(output)
		process.mu.Unlock()
	}()
	return process, nil
}

func (f *fakeRunner) LoginStatus(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.statusAfter > 0 && f.statusCalls >= f.statusAfter {
		return f.statusOutput, nil
	}
	return "You are not logged in.", nil
}

func (f *fakeRunner) setStatusOutput(output string, after int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusOutput = output
	f.statusAfter = after
}

func (f *fakeRunner) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls, f.statusCalls
}

func (f *fakeRunner) wasKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.process == nil {
		return false
	}
	return f.process.killed
}

func newManagerWithFake(t *testing.T, runner *fakeRunner) *Manager {
	t.Helper()
	// The credential marker is written on success, so give the test its own store.
	t.Setenv("CERAA_HOME", t.TempDir())
	dir := os.Getenv("CERAA_HOME")
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	manager := NewManagerWithRunner(runner)
	// A tight cadence so the loop is exercised without multi-second waits.
	manager.firstPoll = 15 * time.Millisecond
	manager.pollStep = 30 * time.Millisecond
	return manager
}

// TestStartReturnsWithoutWaitingForAuthorization is the bug that made the flow
// impossible. The CLI prints a code and then blocks until a human authorizes it,
// which is minutes; Start used to block on that, so the user never saw the code
// before the flow timed out.
func TestStartReturnsWithoutWaitingForAuthorization(t *testing.T) {
	runner := &fakeRunner{
		loginOutput:  "To sign in, open https://chatgpt.com/codex/device-auth\nYour code: WXYZ-1234\n",
		loginDelay:   30 * time.Second,
		statusOutput: "You are not logged in.",
	}
	manager := newManagerWithFake(t, runner)

	done := make(chan struct{})
	var state *AuthState
	var startErr error
	go func() {
		state, startErr = manager.Start()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start blocked on the login process instead of returning")
	}
	if startErr != nil {
		t.Fatal(startErr)
	}
	manager.Cancel(state.flowID)
}

// TestAuthorizationCodeReachesTheChat pins that the code the CLI printed is shown
// to the user, which is the whole point of the flow.
func TestAuthorizationCodeReachesTheChat(t *testing.T) {
	runner := &fakeRunner{
		loginOutput:  "Open https://chatgpt.com/codex/device-auth\ncode: ABCD-EFGH\n",
		loginDelay:   20 * time.Millisecond,
		statusOutput: "You are not logged in.",
	}
	manager := newManagerWithFake(t, runner)
	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Cancel(state.flowID)

	waitFor(t, func() bool { return state.Snapshot().UserCode == "ABCD-EFGH" },
		"the user code never appeared")

	message := state.ResultMessage()
	if !strings.Contains(message, "ABCD-EFGH") {
		t.Fatalf("message should carry the code: %q", message)
	}
	if !strings.Contains(message, "chatgpt.com/codex/device-auth") {
		t.Fatalf("message should carry the url: %q", message)
	}
}

// TestPollDetectsSuccess pins that authorising partway through is noticed, and
// that the credential marker is written before the flow is reported done.
func TestPollDetectsSuccess(t *testing.T) {
	runner := &fakeRunner{
		loginOutput: "code: ABCD-EFGH",
		loginDelay:  10 * time.Millisecond,
	}
	runner.setStatusOutput("Logged in using ChatGPT", 2)
	manager := newManagerWithFake(t, runner)

	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return state.Snapshot().Status == StatusSucceeded },
		"the successful login was never detected")

	value, err := secret.Get("codex_authenticated")
	if err != nil || value != "true" {
		t.Fatalf("credential marker = %q, %v", value, err)
	}
}

// TestUnrecognisedStatusKeepsWaiting pins that phrasing the CLI does not use
// cannot be mistaken for a completed login. Reporting success that did not happen
// is worse than waiting.
func TestUnrecognisedStatusKeepsWaiting(t *testing.T) {
	runner := &fakeRunner{loginOutput: "code: ABCD-EFGH", loginDelay: 10 * time.Millisecond}
	runner.setStatusOutput("something entirely unexpected happened", 1)
	manager := newManagerWithFake(t, runner)

	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Cancel(state.flowID)

	time.Sleep(150 * time.Millisecond)
	if state.Snapshot().Status == StatusSucceeded {
		t.Fatal("an unrecognised status must not be reported as a successful login")
	}
	if _, err := secret.Get("codex_authenticated"); err == nil {
		t.Fatal("the credential marker must not be written for an unrecognised status")
	}
}

// TestCancelIsSafeAfterSuccessAndTwice pins the crash. The cancel button stays on
// screen after a flow finishes, so tapping it closed an already-closed channel and
// took the bot down.
func TestCancelIsSafeAfterSuccessAndTwice(t *testing.T) {
	runner := &fakeRunner{loginOutput: "code: ABCD-EFGH", loginDelay: 10 * time.Millisecond}
	runner.setStatusOutput("Logged in using ChatGPT", 1)
	manager := newManagerWithFake(t, runner)

	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return state.Snapshot().Status == StatusSucceeded },
		"login never succeeded")

	// A finished flow is no longer listed, and cancelling it is a quiet no-op.
	if err := manager.Cancel(state.flowID); err != nil {
		t.Fatalf("cancelling a finished flow should be a no-op, got %v", err)
	}
	// Check is the strict one: an unknown id there is a real mistake.
	if _, err := manager.Check("no-such-flow"); err == nil {
		t.Fatal("Check should reject an unknown flow")
	}

	cancellable := &AuthState{
		flowID:  "codex-test",
		Status:  StatusWaiting,
		done:    make(chan struct{}),
		process: &fakeProcess{},
		cancel:  func() {},
	}
	manager.mu.Lock()
	manager.flows[cancellable.flowID] = cancellable
	manager.mu.Unlock()

	if err := manager.Cancel(cancellable.flowID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	// A second tap on a spent button must be a quiet no-op, not a panic and not
	// a complaint shown to the user.
	if err := manager.Cancel(cancellable.flowID); err != nil {
		t.Fatalf("second cancel must be a quiet no-op, got %v", err)
	}
	if !cancellable.process.(*fakeProcess).killed {
		t.Fatal("cancel should terminate the process")
	}
}

// TestCancelTerminatesTheProcess pins that cancelling actually stops the CLI,
// which it could not do before because the process handle was never stored.
func TestCancelTerminatesTheProcess(t *testing.T) {
	runner := &fakeRunner{loginOutput: "code: ABCD-EFGH", loginDelay: time.Minute}
	manager := newManagerWithFake(t, runner)
	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Cancel(state.flowID); err != nil {
		t.Fatal(err)
	}
	if !runner.wasKilled() {
		t.Fatal("the login process was not terminated")
	}
}

// TestPollingBacksOff pins that the status check does not spawn a process every
// second for a quarter of an hour.
func TestPollingBacksOff(t *testing.T) {
	runner := &fakeRunner{loginOutput: "code: ABCD-EFGH", loginDelay: 10 * time.Millisecond}
	manager := newManagerWithFake(t, runner)
	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Cancel(state.flowID)

	time.Sleep(400 * time.Millisecond)
	_, statusCalls := runner.counts()
	// The cadence is 15ms then 30ms, so a 400ms window must not produce a check
	// per 15ms forever.
	if statusCalls > 20 {
		t.Fatalf("status was checked %d times in 400ms; the interval is not backing off", statusCalls)
	}
	if statusCalls < 1 {
		t.Fatal("status was never checked")
	}
}

// TestFlowExpires pins that a forgotten login does not poll forever.
func TestFlowExpires(t *testing.T) {
	runner := &fakeRunner{loginOutput: "code: ABCD-EFGH", loginDelay: 10 * time.Millisecond}
	runner.setStatusOutput("You are not logged in.", 1)
	manager := newManagerWithFake(t, runner)
	state, err := manager.Start()
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.ExpiresAt = time.Now().Add(-time.Second)
	state.mu.Unlock()

	waitFor(t, func() bool { return state.Snapshot().Status == StatusFailed },
		"an expired flow should fail rather than wait")
}

// TestParseLoginDetails pins the parsing, including that unrelated text cannot
// masquerade as a code.
func TestParseLoginDetails(t *testing.T) {
	cases := []struct {
		name, output, code, url string
	}{
		{
			name:   "code and url on separate lines",
			output: "Open https://chatgpt.com/codex/device-auth\nYour code: WXYZ-1234\n",
			code:   "WXYZ-1234",
			url:    "https://chatgpt.com/codex/device-auth",
		},
		{
			name:   "terminal escapes are stripped",
			output: "\x1b[1m\x1b[32mABCD-EFGH\x1b[0m\n",
			code:   "ABCD-EFGH",
		},
		{
			name:   "no code present",
			output: "something else entirely",
		},
		{
			name:   "lowercase is not a code",
			output: "error: invalid-request",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, url := parseLoginDetails(stripVTControlCharacters(tc.output))
			if code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
			if tc.url != "" && url != tc.url {
				t.Errorf("url = %q, want %q", url, tc.url)
			}
		})
	}
}

// TestClassifyStatus pins that only clear wording counts, in either direction.
func TestClassifyStatus(t *testing.T) {
	cases := map[string]authStatus{
		"Logged in using ChatGPT":   statusLoggedIn,
		"you are logged in":         statusLoggedIn,
		"You are not logged in":     statusLoggedOut,
		"Not authenticated":         statusLoggedOut,
		"no credentials found":      statusLoggedOut,
		"":                          statusUnknown,
		"error: something happened": statusUnknown,
	}
	for output, want := range cases {
		if got := classifyStatus(output); got != want {
			t.Errorf("classifyStatus(%q) = %v, want %v", output, got, want)
		}
	}
}

// TestMissingCLIIsReportedActionably pins that a host without the CLI says so
// plainly, rather than surfacing a raw exec error at the user. A login cannot
// work without the binary, and the user is the one who has to fix it.
func TestMissingCLIIsReportedActionably(t *testing.T) {
	// Point PATH at an empty directory so the binary cannot be found.
	t.Setenv("PATH", t.TempDir())
	err := CodexAvailable()
	if err == nil {
		t.Skip("a codex binary is genuinely on this host")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error should say the CLI is missing, got %v", err)
	}
	if !strings.Contains(err.Error(), "install it") {
		t.Fatalf("error should say what to do, got %v", err)
	}

	// And the manager must surface the same thing rather than a raw exec error.
	manager := NewManager()
	if _, startErr := manager.Start(); startErr == nil {
		t.Fatal("Start should fail without the CLI")
	} else if !strings.Contains(startErr.Error(), "not installed") {
		t.Fatalf("Start error = %v", startErr)
	}
}

// TestResultMessageNeverClaimsSuccessWhileWaiting pins the failure mode that
// started all of this: an agent being told a login finished when it had not.
func TestResultMessageNeverClaimsSuccessWhileWaiting(t *testing.T) {
	state := &AuthState{Status: StatusWaiting, UserCode: "ABCD-EFGH", AuthURL: deviceURL, done: make(chan struct{})}
	if message := state.ResultMessage(); strings.Contains(message, "successful") {
		t.Fatalf("a waiting flow must not read as a success: %q", message)
	}
	failed := &AuthState{Status: StatusFailed, Message: "expired", done: make(chan struct{})}
	if message := failed.ResultMessage(); !strings.Contains(message, "expired") {
		t.Fatalf("a failed flow should say why: %q", message)
	}
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

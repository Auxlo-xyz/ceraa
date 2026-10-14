package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAdapter speaks header-framed DAP from a script table. It records every
// command it received so tests can assert the handshake order, and answers from
// canned bodies, so no debugger is needed on the test host.
type fakeAdapter struct {
	mu       sync.Mutex
	commands []string
	// answer returns the frames to write for one command, in order.
	answer func(command string, seq int) [][]byte
}

type fakeConn struct {
	stdin  io.Writer
	stdout io.Reader
	reader *bufio.Reader
}

func (c *fakeConn) Stdin() io.Writer  { return c.stdin }
func (c *fakeConn) Stdout() io.Reader { return c.stdout }
func (c *fakeConn) Close() error      { return nil }

// Send/Receive put the fake on the same interface the real TCP session uses, so
// the tests exercise the manager and not a stdio path it no longer has.
func (c *fakeConn) Send(body []byte) error {
	return writeFrame(c.stdin, body)
}

func (c *fakeConn) Receive() ([]byte, error) {
	if c.reader == nil {
		c.reader = bufio.NewReader(c.stdout)
	}
	return readFrame(c.reader)
}

func response(seq int, success bool, body string, message string) []byte {
	// id mirrors the request's sequence, which is what the manager correlates on.
	// A response without one is not a response to anything, and the manager is
	// right to keep reading past it.
	payload := map[string]any{
		"type": "response", "seq": seq, "id": seq, "request_seq": seq,
		"success": success, "command": "", "message": message,
		"body": json.RawMessage(body),
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func eventFrame(event string, body string) []byte {
	payload := map[string]any{
		"type": "event", "event": event, "seq": 0, "body": json.RawMessage(body),
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func runFakeAdapter(t *testing.T, answer func(command string, seq int) [][]byte) (*fakeAdapter, Spawner) {
	t.Helper()
	adapter := &fakeAdapter{answer: answer}
	toAdapterR, toAdapterW := io.Pipe()
	fromAdapterR, fromAdapterW := io.Pipe()
	go func() {
		reader := bufio.NewReader(toAdapterR)
		for {
			frame, err := readFrame(reader)
			if err != nil {
				return
			}
			var req struct {
				Seq     int    `json:"seq"`
				Command string `json:"command"`
			}
			if err := json.Unmarshal(frame, &req); err != nil {
				continue
			}
			adapter.mu.Lock()
			adapter.commands = append(adapter.commands, req.Command)
			adapter.mu.Unlock()
			for _, out := range adapter.answer(req.Command, req.Seq) {
				if err := writeFrame(fromAdapterW, out); err != nil {
					return
				}
			}
		}
	}()
	spawn := func(ctx context.Context, command string, args []string, dir string) (Conn, error) {
		// The reader is built once, because a per-call bufio.Reader strands
		// buffered bytes and the next call blocks on a drained stream.
		return &fakeConn{stdin: toAdapterW, stdout: fromAdapterR, reader: bufio.NewReader(fromAdapterR)}, nil
	}
	return adapter, spawn
}

func (a *fakeAdapter) saw(command string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, got := range a.commands {
		if got == command {
			n++
		}
	}
	return n
}

func (a *fakeAdapter) order() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.commands...)
}

func testManager(t *testing.T, answer func(command string, seq int) [][]byte) (*Manager, *fakeAdapter) {
	t.Helper()
	adapter, spawn := runFakeAdapter(t, answer)
	m, err := NewManagerWithSpawn("/fake/dlv", t.TempDir(), spawn)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(m.Close)
	return m, adapter
}

func okScript(command string, seq int) [][]byte {
	switch command {
	case "initialize":
		return [][]byte{response(seq, true, `{"supportsConfigurationDoneRequest":true}`, "")}
	case "launch":
		// A real adapter announces `initialized` before it answers launch, and
		// the manager waits for both. A fake that sent only the response let the
		// earlier version pass while the real debugger never did.
		return [][]byte{
			eventFrame("process", `{"name":"prog"}`),
			eventFrame("initialized", `{}`),
			response(seq, true, `{}`, ""),
		}
	case "setBreakpoints":
		return [][]byte{response(seq, true, `{"breakpoints":[{"line":7,"verified":true}]}`, "")}
	case "stackTrace":
		return [][]byte{response(seq, true, `{"stackFrames":[{"id":1,"name":"main.main","line":7,"source":{"path":"/tmp/main.go"}}]}`, "")}
	case "scopes":
		return [][]byte{response(seq, true, `{"scopes":[{"name":"Locals","variablesReference":10}]}`, "")}
	case "variables":
		return [][]byte{response(seq, true, `{"variables":[{"name":"input","value":"42","type":"int"}]}`, "")}
	case "continue", "next", "stepIn", "stepOut":
		// A resumed program stops again at the next breakpoint; the manager
		// waits for that stop before reporting success.
		return [][]byte{response(seq, true, `{"allThreadsContinued":false}`, ""), eventFrame("stopped", `{"threadId":1,"reason":"breakpoint"}`)}
	case "configurationDone":
		return [][]byte{response(seq, true, `{}`, ""), eventFrame("stopped", `{"threadId":1,"reason":"breakpoint"}`)}
	case "terminate":
		return [][]byte{response(seq, true, `{}`, "")}
	case "disconnect":
		return [][]byte{response(seq, true, `{}`, "")}
	default:
		return [][]byte{response(seq, true, `{}`, "")}
	}
}

func TestLaunchHandshakeOrder(t *testing.T) {
	m, adapter := testManager(t, okScript)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	order := adapter.order()
	if len(order) < 2 || order[0] != "initialize" || order[1] != "launch" {
		t.Errorf("command order = %v, want initialize then launch", order)
	}
	if !m.Status().Running {
		t.Error("status should report a running session after launch")
	}
}

func TestLaunchRefusesASecondSession(t *testing.T) {
	m, _ := testManager(t, okScript)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err == nil {
		t.Error("a second launch should be refused")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want it to name the conflict", err)
	}
}

func TestStackTraceRequiresAStop(t *testing.T) {
	m, _ := testManager(t, okScript)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := m.StackTrace(context.Background(), 20); err == nil {
		t.Error("stack trace accepted while the program is running")
	} else if !strings.Contains(err.Error(), "not stopped") {
		t.Errorf("error = %q, want the precondition named", err)
	}
	if err := m.Continue(context.Background()); err != nil {
		t.Fatalf("continue: %v", err)
	}
	frames, err := m.StackTrace(context.Background(), 20)
	if err != nil {
		t.Fatalf("stack trace: %v", err)
	}
	if len(frames) != 1 || frames[0].Name != "main.main" || frames[0].Line != 7 {
		t.Errorf("frames = %+v, want the scripted frame", frames)
	}
}

func TestSetBreakpointReportsTheBoundLine(t *testing.T) {
	m, _ := testManager(t, okScript)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	line, verified, err := m.SetBreakpoint(context.Background(), "/tmp/main.go", 7)
	if err != nil {
		t.Fatalf("set breakpoint: %v", err)
	}
	if line != 7 || !verified {
		t.Errorf("line=%d verified=%v, want 7 verified", line, verified)
	}
}

func TestVariablesWalkScopes(t *testing.T) {
	m, _ := testManager(t, okScript)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := m.Continue(context.Background()); err != nil {
		t.Fatalf("continue: %v", err)
	}
	scopes, err := m.Scopes(context.Background(), 1)
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0].Name != "Locals" || scopes[0].Value != "ref:10" {
		t.Errorf("scopes = %+v, want the Locals group carrying its reference", scopes)
	}
	vars, err := m.Variables(context.Background(), 10)
	if err != nil {
		t.Fatalf("variables: %v", err)
	}
	if len(vars) != 1 || vars[0].Name != "input" || vars[0].Value != "42" {
		t.Errorf("vars = %+v, want the scripted variable", vars)
	}
}

func TestServerFailureNamesTheCommand(t *testing.T) {
	answer := func(command string, seq int) [][]byte {
		if command == "initialize" || command == "launch" {
			return okScript(command, seq)
		}
		return [][]byte{response(seq, false, `{}`, "no program is being debugged")}
	}
	m, _ := testManager(t, answer)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := m.Continue(context.Background()); err == nil {
		t.Fatal("expected the adapter failure to surface")
	} else if !strings.Contains(err.Error(), "no program is being debugged") {
		t.Errorf("error = %q, want the adapter message", err)
	}
}

func TestAdapterRequestIsRefusedNotHung(t *testing.T) {
	answer := func(command string, seq int) [][]byte {
		if command == "initialize" || command == "launch" {
			return okScript(command, seq)
		}
		if command == "continue" {
			// The adapter asks the client something mid-request. Waiting for a
			// response nobody sends is the hang this guards.
			payload := map[string]any{"type": "request", "seq": 99, "command": "startDebugging", "arguments": map[string]any{}}
			raw, _ := json.Marshal(payload)
			return [][]byte{raw}
		}
		return okScript(command, seq)
	}
	m, _ := testManager(t, answer)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Continue(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected the refused adapter request to end the call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("continue hung on an adapter request")
	}
}

func TestExitedProgramIsReportedNotHung(t *testing.T) {
	answer := func(command string, seq int) [][]byte {
		switch command {
		case "initialize", "launch":
			return okScript(command, seq)
		case "continue":
			// The exit follows the response, so Continue's awaitStop sees it and
			// records the program as finished rather than blocking.
			return [][]byte{response(seq, true, `{}`, ""), eventFrame("exited", `{"exitCode":0}`)}
		default:
			return okScript(command, seq)
		}
	}
	m, _ := testManager(t, answer)
	if err := m.Launch(context.Background(), "/tmp/prog", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := m.Continue(context.Background()); err != nil {
		t.Fatalf("continue: %v", err)
	}
	status := m.Status()
	if !status.Exited || status.ExitCode != 0 {
		t.Errorf("status = %+v, want an exited program with code 0", status)
	}
	if _, err := m.StackTrace(context.Background(), 20); err == nil {
		t.Error("stack trace accepted after the program exited")
	} else if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %q, want it to name the exit", err)
	}
}

func TestNoSessionIsAClearError(t *testing.T) {
	m, _ := testManager(t, okScript)
	if err := m.Continue(context.Background()); err == nil {
		t.Error("continue accepted with no session")
	} else if !strings.Contains(err.Error(), "no session") {
		t.Errorf("error = %q, want it to say launch first", err)
	}
}

func TestResolveServerReportsTheInstall(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveServer(""); err == nil {
		t.Fatal("expected an error with no dlv on PATH")
	} else if !strings.Contains(err.Error(), "dlv") {
		t.Errorf("error = %q, want it to name the missing binary", err)
	}
}

// A prebuilt binary is the common case, and "debug" mode hands the path to
// `go build`, which fails on one. Source paths still build.
func TestLaunchModeFollowsTheTarget(t *testing.T) {
	if got := launchMode("/tmp/prog"); got != "exec" {
		t.Errorf("binary launch mode = %q, want exec", got)
	}
	if got := launchMode("/tmp/main.go"); got != "debug" {
		t.Errorf("source launch mode = %q, want debug", got)
	}
}

// The correlation key is request_seq. A real adapter sends no id, so matching on
// id alone means every launch waits out its timeout with the answer unread.
func TestResponsesCorrelateOnRequestSeq(t *testing.T) {
	withRequestSeq := rpcFrame{Type: "response", RequestSeq: 3}
	if !withRequestSeq.answers(3) || withRequestSeq.answers(4) {
		t.Error("request_seq correlation is wrong")
	}
	withID := rpcFrame{Type: "response", ID: intPtr(5)}
	if !withID.answers(5) || withID.answers(6) {
		t.Error("id correlation is wrong")
	}
	zeroBoth := rpcFrame{Type: "response"}
	if zeroBoth.answers(0) {
		t.Error("a response with no correlation key must match nothing")
	}
	event := rpcFrame{Type: "event", RequestSeq: 3}
	if event.answers(3) {
		t.Error("an event must not satisfy a response correlation")
	}
}

func intPtr(n int) *int { return &n }

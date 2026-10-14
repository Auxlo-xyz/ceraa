package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DAP is the Debug Adapter Protocol: the same header-framed JSON-RPC shape LSP
// uses, but a stateful conversation rather than a question-and-answer one.
//
// The state matters for correctness here. A DAP server emits unsolicited
// `event` frames (initialized, stopped, exited, output) alongside its responses,
// and several requests block until the debuggee hits a breakpoint. So the read
// loop cannot discard what it does not recognise the way the LSP loop does: an
// event carries a body that describes the program's current state, and dropping
// it loses the only signal that the program stopped.
//
// Only launch is implemented. Attach would let the agent seize any process on
// the host, and evaluate would run arbitrary expressions inside the debuggee,
// which is the same power as bash with no approval and no audit. Neither is
// here, deliberately.

// Conn is the narrow duplex surface the manager needs. Tests substitute
// scripted pipes.
type Conn interface {
	Send(body []byte) error
	Receive() ([]byte, error)
	Close() error
}

// dlvConn is a TCP DAP session. dlv's `dap` subcommand is a headless TCP
// server, not a stdio server: it prints "DAP server listening at: host:port"
// on stdout and the protocol runs over that socket. Speaking stdio to it, as an
// earlier version of this file did, produces a connection that is opened and
// never used, which is why a launch against the real debugger hung instead of
// failing.
type dlvConn struct {
	conn net.Conn
	mu   sync.Mutex
	rd   *bufio.Reader
}

func (c *dlvConn) Send(body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeFrame(c.conn, body)
}

func (c *dlvConn) Receive() ([]byte, error) {
	// One reader for the connection's lifetime: a fresh bufio.Reader per call
	// strands buffered bytes in a discarded reader and the next call blocks.
	return readFrame(c.rd)
}

func (c *dlvConn) Close() error { return c.conn.Close() }

// Spawner starts the debug adapter.
type Spawner func(ctx context.Context, command string, args []string, dir string) (Conn, error)

// listeningRe matches the line dlv prints before it accepts a connection.
var listeningRe = regexp.MustCompile(`listening at:\s*(\S+)`)

// spawnTCP starts `dlv dap`, reads the port it announces, and dials it.
//
// The process is deliberately not bound to ctx: the debuggee outlives the
// launch call, and a context-bound process is killed when the call returns.
func spawnTCP(ctx context.Context, command string, args []string, dir string) (Conn, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Stderr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("debug: start dlv: %w", err)
	}
	address := ""
	announcements := bufio.NewReader(stdout)
	for i := 0; i < 20 && address == ""; i++ {
		line, readErr := announcements.ReadString('\n')
		if readErr != nil && line == "" {
			break
		}
		if match := listeningRe.FindStringSubmatch(line); match != nil {
			address = match[1]
		}
	}
	if address == "" {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("debug: dlv did not announce a DAP port; it may have exited")
	}
	// Loopback only. dlv can expose an unauthenticated debugger to whatever the
	// address reaches, so a listener must never be bound more widely than the
	// client that connects to it.
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("debug: connect to dlv at %s: %w", address, err)
	}
	return &dlvConn{conn: conn, rd: bufio.NewReader(conn)}, nil
}

// Timeouts. Launch waits for the adapter to report it is ready, which for a
// Go program includes compiling it; 120s is generous on purpose because a
// cold build is not a hang. Everything after that is fast.
const (
	launchTimeout = 120 * time.Second
	stepTimeout   = 60 * time.Second
	idleTimeout   = 10 * time.Minute
)

// ResolveServer finds dlv project-local first, then PATH. A missing binary is
// an ordinary error so registries without a debugger still construct.
func ResolveServer(workDir string) (string, error) {
	if strings.TrimSpace(workDir) != "" {
		local := strings.TrimSpace(workDir) + "/dlv"
		if info, err := exec.LookPath(local); err == nil {
			return info, nil
		}
	}
	path, err := exec.LookPath("dlv")
	if err != nil {
		return "", fmt.Errorf("debug unavailable: dlv not found on PATH; install github.com/go-delve/delve/cmd/dlv and retry")
	}
	return path, nil
}

// Manager owns one debug session: the adapter process, and the state the
// adapter reports back.
type Manager struct {
	dlvPath string
	rootDir string
	spawn   Spawner
	// proc is the dlv process behind the connection, kept so Close can end the
	// server process as well as the socket.
	proc *exec.Cmd

	mu      sync.Mutex
	conn    Conn
	nextID  int
	stopped bool
	thread  int
	// exited records that the debuggee finished, so a step after termination
	// reports that rather than hanging on a dead socket.
	exited     bool
	exitedCode int
	lastOutput string
	idle       *time.Timer
	// frames and readErr belong to the session's single reader goroutine. Both
	// are nil until a session starts.
	frames  chan rpcFrame
	readErr chan error
	// configured records that configurationDone has been sent. dlv will not
	// process a control request before it, so every call after the first
	// breakpoint set depends on it having happened.
	configured bool
	// hasBreakpoint records that a verified breakpoint exists, so
	// configurationDone knows to wait for a stop rather than for the exit.
	hasBreakpoint bool
}

// NewManager builds a manager. rootDir is the debuggee's working directory.
func NewManager(dlvPath, rootDir string) (*Manager, error) {
	if strings.TrimSpace(dlvPath) == "" {
		return nil, fmt.Errorf("debug unavailable: no dlv path")
	}
	return &Manager{dlvPath: dlvPath, rootDir: rootDir, spawn: spawnTCP}, nil
}

// NewManagerWithSpawn builds a manager with an explicit spawner, so tests can
// drive the protocol against scripted pipes instead of a real debugger.
func NewManagerWithSpawn(dlvPath, rootDir string, spawn Spawner) (*Manager, error) {
	if strings.TrimSpace(dlvPath) == "" {
		return nil, fmt.Errorf("debug unavailable: no dlv path")
	}
	return &Manager{dlvPath: dlvPath, rootDir: rootDir, spawn: spawn}, nil
}

func writeFrame(w io.Writer, body []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		name, value, _ := strings.Cut(trimmed, ":")
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("dap: bad Content-Length %q", value)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("dap: frame without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

type rpcFrame struct {
	Type    string `json:"type"`
	Event   string `json:"event"`
	Command string `json:"command"`
	Seq     int    `json:"seq"`
	ID      *int   `json:"id"`
	// RequestSeq is how a DAP response names the request it answers. A real
	// adapter sends this and no id at all, so correlating on id alone means
	// waiting for a response that never arrives: every launch blocks until its
	// timeout while the answer sits unread in the stream. It carries the seq the
	// request was sent with.
	RequestSeq int             `json:"request_seq"`
	Body       json.RawMessage `json:"body"`
	Success    bool            `json:"success"`
	Message    string          `json:"message"`
}

// answers reports whether this frame is the response to the request sent with
// seq want. id is accepted as well so a test or a nonstandard adapter that
// answers with one is still matched.
func (f rpcFrame) answers(want int) bool {
	if f.Type != "response" {
		return false
	}
	if f.RequestSeq != 0 {
		return f.RequestSeq == want
	}
	return f.ID != nil && *f.ID == want
}

// send writes one request and returns its id.
func (m *Manager) sendLocked(method string, params any) (int, error) {
	m.nextID++
	id := m.nextID
	body, err := json.Marshal(map[string]any{
		"type": "request", "seq": m.nextID, "command": method, "arguments": params,
	})
	if err != nil {
		return 0, err
	}
	if m.conn == nil {
		return 0, fmt.Errorf("debug: no session")
	}
	if err := m.conn.Send(body); err != nil {
		m.shutdownLocked()
		return 0, fmt.Errorf("debug: write %s: %w", method, err)
	}
	return id, nil
}

// waitLocked reads frames until the response for wantID arrives. It records the
// events it passes over, because a `stopped` event between the request and the
// response is the reason the request will resolve at all. It refuses a `reverse
// request` (the adapter asking the client something, which dlv does for
// `startDebugging` in some modes) with a clear error rather than hanging,
// because answering one is out of scope here.
//
// The deadline is enforced by selecting against a timer, not by checking
// between frames. Checking between frames looks equivalent and is not: a frame
// that never arrives parks the read forever and the deadline never comes up
// again to notice. That is how a launch against an adapter that dies
// mid-handshake became an unbounded block instead of a failed call.
//
// One reader goroutine per session drains the stream and publishes decoded
// frames on a buffered channel. A goroutine per frame was tried and leaks one
// per frame the caller skips, because each parks in a blocking read the caller
// has already abandoned.
func (m *Manager) waitLocked(ctx context.Context, wantID int, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = stepTimeout
	}
	if err := m.ensureReaderLocked(); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-m.frames:
			if !ok {
				m.shutdownLocked()
				return nil, fmt.Errorf("debug: the adapter closed the connection")
			}
			switch msg.Type {
			case "event":
				m.recordEventLocked(msg)
			case "request":
				// The adapter wants something from us. Refuse rather than ignore,
				// and say which command, so the failure is diagnosable.
				m.replyLocked(msg.ID, false, "debug: adapter request not supported")
				// An adapter-initiated request means this adapter needs a client
				// that answers requests, and this one does not. Continuing to wait
				// out the step timeout would report a timeout, which names the
				// wrong problem; the session cannot make progress without a
				// client that participates, so the waiting call fails here.
				return nil, fmt.Errorf("debug: the adapter asked for %q, which this client does not support", msg.Command)
			case "response":
				if !msg.answers(wantID) {
					continue
				}
				if !msg.Success {
					reason := msg.Message
					if body := string(msg.Body); reason == "" && body != "" && body != "null" {
						reason = body
					}
					return nil, fmt.Errorf("debug: %s", firstLine(reason))
				}
				return msg.Body, nil
			}
		case err := <-m.readErr:
			m.shutdownLocked()
			return nil, fmt.Errorf("debug: read response: %w", err)
		case <-timer.C:
			return nil, fmt.Errorf("debug: timed out after %s waiting for a response", timeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ensureReaderLocked starts the session's single reader goroutine. Callers hold
// mu.
func (m *Manager) ensureReaderLocked() error {
	if m.frames != nil {
		return nil
	}
	if m.conn == nil {
		return fmt.Errorf("debug: no session")
	}
	frames := make(chan rpcFrame, 64)
	readErr := make(chan error, 1)
	conn := m.conn
	m.frames, m.readErr = frames, readErr
	go func() {
		for {
			frame, err := conn.Receive()
			if err != nil {
				select {
				case readErr <- err:
				default:
				}
				close(frames)
				return
			}
			var msg rpcFrame
			if err := json.Unmarshal(frame, &msg); err != nil {
				continue
			}
			frames <- msg
		}
	}()
	return nil
}

// replyLocked refuses an adapter-initiated request.
//
// The write is bounded by a deadline rather than left blocking, because the
// whole point is to end a call that is otherwise stuck: a reply that parks
// forever on an adapter that is not reading would turn a hang into a different
// hang. An adapter that never reads its responses gets no refusal, and the
// waiting request still fails on its own deadline.
func (m *Manager) replyLocked(id *int, success bool, message string) {
	if id == nil || m.conn == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"type": "response", "request_seq": 0, "success": success, "command": "", "message": message, "body": map[string]any{},
	})
	if err != nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- m.conn.Send(body) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// recordEventLocked folds the events the protocol reports into manager state.
func (m *Manager) recordEventLocked(msg rpcFrame) {
	switch msg.Event {
	case "stopped":
		m.stopped = true
		var body struct {
			ThreadID int `json:"threadId"`
		}
		_ = json.Unmarshal(msg.Body, &body)
		if body.ThreadID != 0 {
			m.thread = body.ThreadID
		}
	case "continued":
		m.stopped = false
	case "exited", "terminated":
		// A stop outranks a following exit. dlv emits `terminated` when the
		// program halts at a breakpoint as well as when it finishes, so treating
		// either as an exit makes a good halt look like a program that ran to
		// completion - and an exited session then refuses every stack and
		// variables request, which is the whole point of stopping there.
		if m.stopped {
			return
		}
		m.exited = true
		var body struct {
			ExitCode int `json:"exitCode"`
		}
		_ = json.Unmarshal(msg.Body, &body)
		m.exitedCode = body.ExitCode
	case "output":
		var body struct {
			Output string `json:"output"`
		}
		_ = json.Unmarshal(msg.Body, &body)
		if body.Output != "" {
			m.lastOutput = body.Output
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func (m *Manager) shutdownLocked() {
	if m.idle != nil {
		m.idle.Stop()
		m.idle = nil
	}
	if m.conn != nil {
		_ = m.conn.Close()
		m.conn = nil
	}
	if m.proc != nil {
		_ = m.proc.Process.Kill()
		m.proc = nil
	}
	m.frames = nil
	m.readErr = nil
	m.stopped = false
	m.exited = false
}

// Close terminates the session, disconnecting politely first so dlv cleans up
// the debuggee.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		if id, err := m.sendLocked("disconnect", map[string]any{"terminateDebuggee": true}); err == nil {
			_, _ = m.waitLocked(context.Background(), id, 5*time.Second)
		}
	}
	m.shutdownLocked()
}

func (m *Manager) touchLocked() {
	if m.idle != nil {
		m.idle.Stop()
	}
	m.idle = time.AfterFunc(idleTimeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.shutdownLocked()
	})
}

// Launch compiles and starts a Go program under the debugger.
//
// This is the only way a session starts. There is no attach: attaching would
// let the agent seize any process on the host, and that power is not one a
// tool gets by being registered.
func (m *Manager) Launch(ctx context.Context, program string, args []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		return fmt.Errorf("debug: a session is already running; terminate it first")
	}
	// Detached from the caller's context: the debuggee outlives this request,
	// and binding it to a per-request context would kill it on return.
	conn, err := m.spawn(context.WithoutCancel(ctx), m.dlvPath,
		[]string{"dap", "--listen=127.0.0.1:0"}, m.rootDir)
	if err != nil {
		return fmt.Errorf("debug: start dlv: %w", err)
	}
	m.conn = conn
	m.touchLocked()
	initID, err := m.sendLocked("initialize", map[string]any{
		"clientID":              "ceraa",
		"adapterID":             "go",
		"linesStartAt1":         true,
		"columnsStartAt1":       true,
		"pathFormat":            "path",
		"supportsVariableType":  true,
		"supportsRunInTerminal": false,
	})
	if err != nil {
		return err
	}
	if _, err := m.waitLocked(ctx, initID, launchTimeout); err != nil {
		m.shutdownLocked()
		return err
	}
	launchID, err := m.sendLocked("launch", map[string]any{
		"mode": launchMode(program), "program": program, "arguments": args,
	})
	if err != nil {
		return err
	}
	// The launch completes on the `initialized` event, not on the response.
	//
	// dlv sends `initialized` before it answers launch, and it is the signal
	// that the debuggee is ready for breakpoints. Waiting for the response
	// instead consumed the event as an ignorable aside and returned early, so
	// the program was never declared ready: every later `continue` came back
	// "Unable to process" while the breakpoint sat bound and verified.
	//
	// Both conditions are honoured, and both are collected in this one loop: the
	// response still has to arrive, because a launch that failed (bad binary,
	// build error) must fail here rather than hang until the timeout. Reading on
	// past `initialized` for the response is what keeps the next call from
	// mistaking a leftover frame for its own answer.
	ready := false
	answered := false
	timer := time.NewTimer(launchTimeout)
	defer timer.Stop()
	for !ready || !answered {
		select {
		case msg, ok := <-m.frames:
			if !ok {
				m.shutdownLocked()
				return fmt.Errorf("debug: the adapter closed the connection during launch")
			}
			switch msg.Type {
			case "event":
				if msg.Event == "initialized" {
					ready = true
					continue
				}
				m.recordEventLocked(msg)
			case "response":
				if msg.answers(launchID) {
					if !msg.Success {
						m.shutdownLocked()
						return fmt.Errorf("debug: launch failed: %s", firstLine(msg.Message))
					}
					answered = true
				}
			}
		case err := <-m.readErr:
			m.shutdownLocked()
			return fmt.Errorf("debug: launch: %w", err)
		case <-timer.C:
			return fmt.Errorf("debug: launch timed out after %s", launchTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Fold in any frame already queued behind the launch pair. A buffered
	// reader can hand the launch response before the event that precedes it in
	// the stream has been interpreted, and leaving that behind means the next
	// call reads a stale frame and mistakes it for its own answer. Only frames
	// already in hand are taken; the socket is not waited on.
	for {
		select {
		case msg := <-m.frames:
			m.recordEventLocked(msg)
		default:
			return nil
		}
	}
}

// ConfigurationDone declares this client's breakpoint set complete, after which
// the adapter will run the program. Exported because the tool layer owns the
// breakpoint sequence: an undeclared set means dlv refuses to continue.
func (m *Manager) ConfigurationDone(ctx context.Context) error { return m.configurationDone(ctx) }

// configurationDone tells the adapter that this client's breakpoint set is
// complete and it may start the program.
//
// It is a request, not a notification, and it must follow the breakpoints. dlv
// answers a control request sent before this with "Unable to process
// `continue`", because until the set is declared finished the adapter still
// expects more breakpoints and will not run anything.
func (m *Manager) configurationDone(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return err
	}
	id, err := m.sendLocked("configurationDone", map[string]any{})
	if err != nil {
		return err
	}
	_, err = m.waitLocked(ctx, id, stepTimeout)
	if err != nil {
		return err
	}
	m.configured = true
	m.touchLocked()
	// configurationDone is what starts the program, so it is also where the
	// first breakpoint is hit. A breakpoint set before it binds here rather
	// than needing a continue that dlv refuses, because by the time anyone asks
	// to continue the program is already running.
	//
	// When no breakpoint was set the program runs to completion instead, and the
	// exit is recorded here.
	if m.hasBreakpoint {
		return m.awaitStop(ctx)
	}
	return nil
}

// awaitStop reads frames until the debuggee stops or exits.
func (m *Manager) awaitStop(ctx context.Context) error {
	timer := time.NewTimer(launchTimeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-m.frames:
			if !ok {
				return fmt.Errorf("debug: the adapter closed the connection")
			}
			switch msg.Type {
			case "event":
				m.recordEventLocked(msg)
				if msg.Event == "stopped" || msg.Event == "exited" || msg.Event == "terminated" {
					return nil
				}
			case "response":
				if !msg.Success && msg.Command == "continue" {
					return fmt.Errorf("debug: %s", firstLine(msg.Message))
				}
			}
		case err := <-m.readErr:
			return fmt.Errorf("debug: %w", err)
		case <-timer.C:
			return fmt.Errorf("debug: timed out waiting for the program to stop")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// configured records that the breakpoint set has been declared complete, so a
// second SetBreakpoint re-sends configurationDone rather than assuming the
// adapter already knows.
func (m *Manager) markConfigured() {
	m.mu.Lock()
	m.configured = true
	m.mu.Unlock()
}

func (m *Manager) isConfigured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configured
}

func (m *Manager) notifyLocked(command string, args any) error {
	body, err := json.Marshal(map[string]any{
		"type": "request", "command": command, "arguments": args,
	})
	if err != nil {
		return err
	}
	return m.conn.Send(body)
}

// StackFrame is one frame of the stopped program's call stack.
type StackFrame struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Line int    `json:"line"`
	File string `json:"source"`
}

// Variable is one inspected value.
type Variable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// launchMode picks how dlv starts the target. "debug" hands the path to `go
// build`, which fails when the path is already a binary, and a prebuilt target
// is what an agent inspecting a running program has. A source path still builds.
func launchMode(program string) string {
	if strings.HasSuffix(program, ".go") {
		return "debug"
	}
	return "exec"
}

// SetBreakpoint places a breakpoint at a 1-based line and reports the lines the
// adapter actually bound, which can differ when the requested line is not
// executable.
func (m *Manager) SetBreakpoint(ctx context.Context, path string, line int) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return 0, false, err
	}
	id, err := m.sendLocked("setBreakpoints", map[string]any{
		"source":      map[string]any{"path": path},
		"breakpoints": []map[string]any{{"line": line}},
	})
	if err != nil {
		return 0, false, err
	}
	raw, err := m.waitLocked(ctx, id, stepTimeout)
	if err != nil {
		return 0, false, err
	}
	var body struct {
		Breakpoints []struct {
			Line     int  `json:"line"`
			Verified bool `json:"verified"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, false, err
	}
	if len(body.Breakpoints) == 0 {
		return line, false, fmt.Errorf("debug: no breakpoint bound at %s:%d", path, line)
	}
	bp := body.Breakpoints[0]
	// A new breakpoint set invalidates the previous declaration that the set is
	// finished, so the next one is declared again rather than assumed.
	m.configured = false
	if bp.Verified {
		m.hasBreakpoint = true
	}
	return bp.Line, bp.Verified, nil
}

// primaryThreadLocked is the thread a control request targets before any stop
// event has named one.
//
// The thread id is read from the adapter rather than assumed. dlv names its
// halted thread -1 ("Current"), not 1, and answers `continue` on thread 1 with
// "Unable to process `continue`" - which is indistinguishable from the program
// having run to completion. Guessing 1 looks correct right up until it fails.
func (m *Manager) primaryThreadLocked() int {
	if m.thread != 0 {
		return m.thread
	}
	return -1
}

// discoverThreadLocked asks the adapter which thread is current. Called once,
// before the first control request, so the id comes from the protocol rather
// than from a guess that happens to work for one adapter.
func (m *Manager) discoverThreadLocked(ctx context.Context) int {
	if m.thread != 0 {
		return m.thread
	}
	id, err := m.sendLocked("threads", map[string]any{})
	if err != nil {
		return -1
	}
	raw, err := m.waitLocked(ctx, id, stepTimeout)
	if err != nil {
		return -1
	}
	var body struct {
		Threads []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		for _, thread := range body.Threads {
			if thread.Name == "Current" {
				m.thread = thread.ID
				return m.thread
			}
		}
		if len(body.Threads) > 0 {
			m.thread = body.Threads[0].ID
			return m.thread
		}
	}
	return -1
}

// Continue resumes the debuggee until it stops again or exits.
func (m *Manager) Continue(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return err
	}
	if m.exited {
		return fmt.Errorf("debug: the program has exited (code %d)", m.exitedCode)
	}
	thread := m.primaryThreadLocked()
	id, err := m.sendLocked("continue", map[string]any{"threadId": thread})
	if err != nil {
		return err
	}
	if _, err := m.waitLocked(ctx, id, stepTimeout); err != nil {
		return err
	}
	m.touchLocked()
	return m.awaitStop(ctx)
}

// step executes one of the step commands against the current thread.
func (m *Manager) step(ctx context.Context, command string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return err
	}
	id, err := m.sendLocked(command, map[string]any{"threadId": m.discoverThreadLocked(ctx)})
	if err != nil {
		return err
	}
	_, err = m.waitLocked(ctx, id, stepTimeout)
	m.touchLocked()
	return err
}

// StepOver advances to the next line in the current frame.
func (m *Manager) StepOver(ctx context.Context) error { return m.step(ctx, "next") }

// StepIn descends into the call on the current line.
func (m *Manager) StepIn(ctx context.Context) error { return m.step(ctx, "stepIn") }

// StepOut returns from the current frame.
func (m *Manager) StepOut(ctx context.Context) error { return m.step(ctx, "stepOut") }

// StackTrace returns the call stack of the stopped thread.
func (m *Manager) StackTrace(ctx context.Context, depth int) ([]StackFrame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return nil, err
	}
	if !m.stopped {
		return nil, fmt.Errorf("debug: the program is not stopped; continue to a breakpoint first")
	}
	if depth <= 0 {
		depth = 20
	}
	id, err := m.sendLocked("stackTrace", map[string]any{"threadId": m.discoverThreadLocked(ctx), "startFrame": 0, "levels": depth})
	if err != nil {
		return nil, err
	}
	raw, err := m.waitLocked(ctx, id, stepTimeout)
	if err != nil {
		return nil, err
	}
	var body struct {
		StackFrames []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Line   int    `json:"line"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"stackFrames"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("debug: decode stack: %w", err)
	}
	frames := make([]StackFrame, 0, len(body.StackFrames))
	for _, f := range body.StackFrames {
		frames = append(frames, StackFrame{ID: f.ID, Name: f.Name, Line: f.Line, File: f.Source.Path})
	}
	return frames, nil
}

// Scopes returns the variable groups visible in a stack frame, keyed by the
// reference the next call needs.
func (m *Manager) Scopes(ctx context.Context, frameID int) ([]Variable, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return nil, err
	}
	id, err := m.sendLocked("scopes", map[string]any{"frameId": frameID})
	if err != nil {
		return nil, err
	}
	raw, err := m.waitLocked(ctx, id, stepTimeout)
	if err != nil {
		return nil, err
	}
	var body struct {
		Scopes []struct {
			Name               string `json:"name"`
			VariablesReference int    `json:"variablesReference"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("debug: decode scopes: %w", err)
	}
	out := make([]Variable, 0, len(body.Scopes))
	for _, s := range body.Scopes {
		out = append(out, Variable{Name: s.Name, Value: fmt.Sprintf("ref:%d", s.VariablesReference), Type: "scope"})
	}
	return out, nil
}

// Variables expands a variables reference (from Scopes or Variables) into the
// values it names.
func (m *Manager) Variables(ctx context.Context, ref int) ([]Variable, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireSessionLocked(); err != nil {
		return nil, err
	}
	id, err := m.sendLocked("variables", map[string]any{"variablesReference": ref})
	if err != nil {
		return nil, err
	}
	raw, err := m.waitLocked(ctx, id, stepTimeout)
	if err != nil {
		return nil, err
	}
	var body struct {
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			Type  string `json:"type"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("debug: decode variables: %w", err)
	}
	out := make([]Variable, 0, len(body.Variables))
	for _, v := range body.Variables {
		out = append(out, Variable{Name: v.Name, Value: v.Value, Type: v.Type})
	}
	return out, nil
}

// Status is the session's observable state, reported by debug_status and used
// to make every other action's precondition checkable by the model.
type Status struct {
	Running  bool
	Stopped  bool
	Exited   bool
	ExitCode int
	Thread   int
	Output   string
}

// Status reports the current session state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Running:  m.conn != nil,
		Stopped:  m.stopped,
		Exited:   m.exited,
		ExitCode: m.exitedCode,
		Thread:   m.thread,
		Output:   m.lastOutput,
	}
}

// Terminate stops the debuggee and ends the session.
func (m *Manager) Terminate(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		return nil
	}
	if id, err := m.sendLocked("terminate", map[string]any{}); err == nil {
		_, _ = m.waitLocked(ctx, id, 10*time.Second)
	}
	m.shutdownLocked()
	return nil
}

func (m *Manager) requireSessionLocked() error {
	if m.conn == nil {
		return fmt.Errorf("debug: no session; launch a program first")
	}
	if m.exited {
		return fmt.Errorf("debug: the program has exited (code %d); launch it again to continue", m.exitedCode)
	}
	return nil
}

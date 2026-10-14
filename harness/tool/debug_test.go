package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/dap"
	"ceraa/harness/operation"
)

func debugToolsByName(t *testing.T, base *DebugTool) map[string]Tool {
	t.Helper()
	out := map[string]Tool{}
	for _, tool := range DebugActionTools(base) {
		out[tool.Name()] = tool
	}
	return out
}

func TestEachDebugActionIsItsOwnTool(t *testing.T) {
	base := NewDebugTool()
	tools := DebugActionTools(base)
	byName := debugToolsByName(t, base)
	want := []string{"debug_launch", "debug_step", "debug_inspect"}
	if len(tools) != len(want) {
		t.Fatalf("debug actions = %d, want %d", len(tools), len(want))
	}
	for _, name := range want {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("%s missing from the offered catalogue", name)
			continue
		}
		actionTool, ok := tool.(*DebugActionTool)
		if !ok {
			t.Errorf("%T is not a DebugActionTool", tool)
			continue
		}
		if actionTool.base != base {
			t.Errorf("%s does not share the session it was built with", name)
		}
		if !tool.(SequentialTool).Sequential() {
			t.Errorf("%s is not sequential, so a batch could interleave two debug steps", name)
		}
		if queue, ok := tool.(SequentialQueueTool); !ok || queue.SequentialQueue() != "debug" {
			t.Errorf("%s has no debug lane", name)
		}
	}
}

// The gate is the design. Attach would let the agent seize any process on the
// host and evaluate would run expressions inside the debuggee, so neither is
// offered. This test is the reason that cannot change quietly.
func TestDebugOffersNoAttachOrEvaluate(t *testing.T) {
	for _, tool := range DefaultRegistry().Offered() {
		if tool.Name() == "debug_attach" || tool.Name() == "debug_evaluate" {
			t.Errorf("%s is offered; attach and evaluate are deliberately out of scope", tool.Name())
		}
	}
	for _, def := range debugActionDefs {
		for _, action := range def.actions {
			if action == "attach" || action == "evaluate" {
				t.Errorf("action %q is registered; attach and evaluate are deliberately out of scope", action)
			}
		}
	}
}

func TestCombinedDebugIsNotOfferedButStillRuns(t *testing.T) {
	reg := DefaultRegistry()
	if _, ok := reg.Get("debug"); !ok {
		t.Fatal("combined debug driver is not registered, so specs of type debug cannot execute")
	}
	for _, tool := range reg.Offered() {
		if tool.Name() == "debug" {
			t.Fatal("combined debug driver is offered, teaching the retired shape")
		}
	}
}

func TestDebugTranslatesIntoTheCombinedSpec(t *testing.T) {
	tools := debugToolsByName(t, NewDebugTool())
	spec, err := tools["debug_step"].Translate(map[string]any{"path": "main.go", "line": 12})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "debug" || spec.Args["action"] != "breakpoint" || spec.Args["line"] != 12 {
		t.Errorf("spec = %+v, want a debug breakpoint spec", spec)
	}
	if _, err := tools["debug_launch"].Translate(map[string]any{}); err == nil {
		t.Error("debug_launch accepted a call with no program")
	}
	if _, err := tools["debug_inspect"].Translate(map[string]any{"path": "main.go"}); err == nil {
		t.Error("debug_inspect accepted a path it cannot use")
	}
	if _, err := tools["debug_step"].Translate(map[string]any{"program": "main.go"}); err == nil {
		t.Error("debug_step accepted a program argument it cannot use")
	}
}

func TestEveryDebugToolExplainsItself(t *testing.T) {
	for _, tool := range DefaultRegistry().Offered() {
		if !strings.HasPrefix(tool.Name(), "debug_") {
			continue
		}
		if len(tool.Description()) < 40 {
			t.Errorf("%s has a description of %d chars", tool.Name(), len(tool.Description()))
		}
	}
}

// The launcher is the only session start. Without one, every other action must
// say so in a way the model can correct from, rather than reporting an
// unrelated protocol failure.
func TestDebugActionsRequireASession(t *testing.T) {
	base := NewDebugTool()
	for _, action := range []string{"continue", "step_over", "step_in", "step_out", "stack", "variables", "breakpoint", "terminate"} {
		_, err := base.RunContext(context.Background(), operation.Spec{
			Type: "debug", Args: map[string]any{"action": action, "workdir": t.TempDir(), "path": "main.go", "line": 3, "reference": 5},
		})
		if err == nil {
			t.Errorf("debug mode %s accepted a call with no session", action)
			continue
		}
		if !strings.Contains(err.Error(), "launch a program first") {
			t.Errorf("debug mode %s error = %q, want it to name the missing session", action, err.Error())
		}
	}
}

func TestDebugStatusWithoutSessionIsReadable(t *testing.T) {
	base := NewDebugTool()
	out, err := base.RunContext(context.Background(), operation.Spec{
		Type: "debug", Args: map[string]any{"action": "status"},
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "no debug session") {
		t.Errorf("status = %q, want it to say there is no session", out)
	}
}

func TestDebugUnavailableNamesTheInstall(t *testing.T) {
	base := NewDebugTool()
	base.newManager = func(rootDir string) (*dap.Manager, error) {
		return nil, fmt.Errorf("debug unavailable: dlv not found on PATH; install github.com/go-delve/delve/cmd/dlv and retry")
	}
	_, err := base.RunContext(context.Background(), operation.Spec{
		Type: "debug", Args: map[string]any{"action": "launch", "program": "main.go", "workdir": t.TempDir()},
	})
	if err == nil {
		t.Fatal("expected the missing binary to fail")
	}
	if !strings.Contains(err.Error(), "dlv") {
		t.Errorf("error = %q, want it to name the missing binary", err)
	}
}

// debugFakeAdapter scripts DAP responses so the whole launch-breakpoint-continue
// path runs in a test with no debugger present.
func debugFakeAdapter(t *testing.T) dap.Spawner {
	t.Helper()
	toAdapterR, toAdapterW := io.Pipe()
	fromAdapterR, fromAdapterW := io.Pipe()
	go func() {
		reader := bufio.NewReader(toAdapterR)
		for {
			length := -1
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimSpace(line) == "" {
					break
				}
				if name, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok &&
					strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
					fmt.Sscanf(strings.TrimSpace(value), "%d", &length)
				}
			}
			if length < 0 {
				return
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}
			var req struct {
				Seq     int    `json:"seq"`
				Command string `json:"command"`
			}
			_ = json.Unmarshal(body, &req)
			// Each frame is written separately: an event first, then the
			// response the manager is waiting for.
			var frames []string
			switch req.Command {
			case "initialize":
				frames = []string{dapOK(req.Seq)}
			case "launch":
				// A real adapter announces `initialized` before it answers
				// launch; the manager waits for both, so a fake that sent only
				// the response let the tool layer pass against a driver that
				// could never have launched.
				frames = []string{`{"type":"event","event":"process","body":{"name":"prog"}}`, `{"type":"event","event":"initialized","body":{}}`, dapOK(req.Seq)}
			case "configurationDone":
				// configurationDone starts the program and the breakpoint stops
				// it, so the manager waits for the stop here.
				frames = []string{dapOK(req.Seq), `{"type":"event","event":"stopped","body":{"threadId":1}}`}
			case "setBreakpoints":
				frames = []string{dapBody(req.Seq, `{"breakpoints":[{"line":5,"verified":true}]}`)}
			case "continue":
				frames = []string{dapOK(req.Seq), `{"type":"event","event":"stopped","body":{"threadId":1}}`}
			default:
				frames = []string{dapOK(req.Seq)}
			}
			for _, frame := range frames {
				fmt.Fprintf(fromAdapterW, "Content-Length: %d\r\n\r\n%s", len(frame), frame)
			}
		}
	}()
	return func(ctx context.Context, command string, args []string, dir string) (dap.Conn, error) {
		return &debugFakeConn{stdin: toAdapterW, stdout: fromAdapterR, reader: bufio.NewReader(fromAdapterR)}, nil
	}
}

type debugFakeConn struct {
	stdin  io.Writer
	stdout io.Reader
	reader *bufio.Reader
}

func (c *debugFakeConn) Stdin() io.Writer  { return c.stdin }
func (c *debugFakeConn) Stdout() io.Reader { return c.stdout }
func (c *debugFakeConn) Close() error      { return nil }

// The fake sits on the same interface the real TCP session uses, so the test
// exercises the manager rather than a stdio path it no longer has.
func (c *debugFakeConn) Send(body []byte) error {
	return dapWriteFrame(c.stdin, body)
}

func (c *debugFakeConn) Receive() ([]byte, error) {
	return dapReadFrame(c.reader)
}

// A real adapter correlates on request_seq and sends no id, so the fake does
// the same. A fake that answered with id would pass against a client that only
// reads id, which is the bug this replaced.
func dapOK(seq int) string {
	return fmt.Sprintf(`{"type":"response","request_seq":%d,"success":true,"command":"","body":{}}`, seq)
}

func dapBody(seq int, body string) string {
	return fmt.Sprintf(`{"type":"response","request_seq":%d,"success":true,"command":"","body":%s}`, seq, body)
}

func dapWriteFrame(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body))
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func dapReadFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if name, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok &&
			strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			fmt.Sscanf(strings.TrimSpace(value), "%d", &length)
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

// TestDebugSeamLaunchBreakpointContinue drives the whole gated path through a
// real operation manager: Translate, dispatch, run, formatted result. If the
// spec type stops resolving to the hidden driver, this fails the way a tool
// that is only registered fails in production.
func TestDebugSeamLaunchBreakpointContinue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	spawn := debugFakeAdapter(t)
	base := NewDebugTool()
	base.timeout = 5 * time.Second
	base.newManager = func(rootDir string) (*dap.Manager, error) {
		return dap.NewManagerWithSpawn("/fake/dlv", rootDir, spawn)
	}
	reg := NewRegistry()
	reg.RegisterHidden(base)
	for _, actionTool := range DebugActionTools(base) {
		reg.Register(actionTool)
	}
	var byName = map[string]Tool{}
	for _, tool := range reg.Offered() {
		byName[tool.Name()] = tool
	}

	runOne := func(toolName string, args map[string]any, opID string) string {
		t.Helper()
		spec, err := byName[toolName].Translate(args)
		if err != nil {
			t.Fatalf("%s translate: %v", toolName, err)
		}
		spec.Args["workdir"] = dir
		opMgr := operation.NewWithBuffer(&seamRunner{reg: reg}, 8)
		opMgr.Add(operation.Operation{ID: opID, Spec: spec})
		select {
		case result := <-opMgr.Results():
			if result.Error != "" {
				t.Fatalf("%s error: %s", toolName, result.Error)
			}
			return result.Output
		case <-time.After(30 * time.Second):
			t.Fatalf("%s produced no result", toolName)
		}
		return ""
	}

	if out := runOne("debug_launch", map[string]any{"program": "main.go"}, "op1"); !strings.Contains(out, "launched") {
		t.Errorf("launch output = %q", out)
	}
	// Setting a breakpoint declares the set and runs the program to it, so the
	// stop is reached here rather than by a separate resume.
	if out := runOne("debug_step", map[string]any{"path": "main.go", "line": 5}, "op2"); !strings.Contains(out, "bound at") || !strings.Contains(out, "stopped") {
		t.Errorf("breakpoint output = %q", out)
	}
	if out := runOne("debug_inspect", map[string]any{"mode": "stack"}, "op3"); !strings.Contains(out, "stack frames") {
		t.Errorf("stack output = %q", out)
	}
	if out := runOne("debug_inspect", map[string]any{"mode": "status"}, "op4"); !strings.Contains(out, "stopped") {
		t.Errorf("status output = %q", out)
	}
}

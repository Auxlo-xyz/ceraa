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
	"sync"
	"testing"
	"time"

	"ceraa/harness/lsp"
	"ceraa/harness/operation"
)

func lspToolsByName(t *testing.T, tools []Tool) map[string]Tool {
	t.Helper()
	out := map[string]Tool{}
	for _, tool := range tools {
		out[tool.Name()] = tool
	}
	return out
}

func TestEachLspActionIsItsOwnTool(t *testing.T) {
	base := NewLspTool()
	tools := LspActionTools(base)
	if len(tools) != 5 {
		t.Fatalf("lsp actions = %d, want five", len(tools))
	}
	byName := lspToolsByName(t, tools)
	for _, name := range []string{"lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols"} {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		actionTool, ok := tool.(*LspActionTool)
		if !ok {
			t.Errorf("%T is not an LspActionTool", tool)
			continue
		}
		if actionTool.base != base {
			t.Errorf("%s does not share the driver it was built with", name)
		}
		if !tool.(SequentialTool).Sequential() {
			t.Errorf("%s is not sequential, so a batch could interleave two server steps", name)
		}
		if queue, ok := tool.(SequentialQueueTool); !ok || queue.SequentialQueue() != "lsp" {
			t.Errorf("%s has no lsp lane, so a server call would serialize against unrelated edits", name)
		}
	}
}

// The combined driver stays registered so old specs and the runner resolve,
// but the model is offered one tool per action, because a combined schema is
// the shape the browser split removed.
func TestTheCombinedLspIsNotOfferedButStillRuns(t *testing.T) {
	reg := DefaultRegistry()
	if _, ok := reg.Get("lsp"); !ok {
		t.Fatal("combined lsp driver is not registered, so specs of type lsp cannot execute")
	}
	for _, tool := range reg.Offered() {
		if tool.Name() == "lsp" {
			t.Fatal("combined lsp driver is offered, teaching the retired shape")
		}
	}
}

// Translating pins the action, so the arguments still go through the one
// validation path rather than a second copy of it.
func TestLspToolsTranslateIntoTheCombinedSpec(t *testing.T) {
	reg := DefaultRegistry()
	spec, err := lspToolsByName(t, reg.Offered())["lsp_references"].
		Translate(map[string]any{"path": "a.go", "line": 3})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "lsp" || spec.Args["action"] != "references" || spec.Args["path"] != "a.go" {
		t.Errorf("spec = %+v, want an lsp references spec", spec)
	}
	if _, err := lspToolsByName(t, reg.Offered())["lsp_diagnostics"].
		Translate(map[string]any{}); err == nil {
		t.Error("lsp_diagnostics accepted a call with no path")
	}
	if _, err := lspToolsByName(t, reg.Offered())["lsp_definition"].
		Translate(map[string]any{"path": "a.go"}); err == nil {
		t.Error("lsp_definition accepted a call with no line")
	}
}

// A model that names an action on a single-action tool has guessed. Saying so
// is better than quietly overriding it.
func TestASingleLspToolRejectsAnActionArgument(t *testing.T) {
	_, err := lspToolsByName(t, DefaultRegistry().Offered())["lsp_hover"].
		Translate(map[string]any{"action": "hover", "path": "a.go", "line": 1})
	if err == nil {
		t.Fatal("expected the call to be rejected")
	}
	if !strings.Contains(err.Error(), "single-action tool") {
		t.Errorf("message = %q, want it to explain the tool takes no action argument", err)
	}
}

// A crossed argument names the mistake, so the model can correct itself from
// the message instead of guessing again.
func TestLspRejectsCrossedArguments(t *testing.T) {
	_, err := lspToolsByName(t, DefaultRegistry().Offered())["lsp_diagnostics"].
		Translate(map[string]any{"path": "a.go", "line": 3})
	if err == nil {
		t.Fatal("lsp_diagnostics accepted a line it cannot use")
	}
	if !strings.Contains(err.Error(), "does not accept") {
		t.Errorf("message = %q, want the refusal", err)
	}
}

// The description states the contract; the prompt states when to reach for it.
func TestEveryLspToolExplainsItself(t *testing.T) {
	for _, tool := range DefaultRegistry().Offered() {
		if !strings.HasPrefix(tool.Name(), "lsp_") {
			continue
		}
		description := tool.Description()
		if len(description) < 40 {
			t.Errorf("%s has a description of %d chars: %q", tool.Name(), len(description), description)
		}
	}
}

// The two approved toolrules lines, pinned verbatim. The prompt is the only
// place usage guidance may live, so this test fails if the lines are edited,
// moved, or dropped without the owner.
func TestThePromptTeachesTheLspActions(t *testing.T) {
	prompt := readPrompt(t)
	first := "LSP actions query the project's language server and must be performed one at a time. Use one tool per action. Available actions: lsp_diagnostics, lsp_definition, lsp_references, lsp_hover, lsp_symbols."
	second := "LSP needs a running language server (gopls for Go); when none is available the tool says so. Prefer LSP over rg when tracing a definition or its references across files."
	if !strings.Contains(prompt, first) {
		t.Error("the prompt does not carry the approved LSP action line verbatim")
	}
	if !strings.Contains(prompt, second) {
		t.Error("the prompt does not carry the approved LSP availability line verbatim")
	}
}

// lspFakeServer answers framed JSON-RPC from a script, proving the tool layer
// against a server without a language toolchain on the test host.
type lspFakeServer struct {
	mu       sync.Mutex
	received []string
}

type lspFakeProc struct {
	stdin  io.Writer
	stdout io.Reader
}

func (p *lspFakeProc) Stdin() io.Writer  { return p.stdin }
func (p *lspFakeProc) Stdout() io.Reader { return p.stdout }
func (p *lspFakeProc) Close() error      { return nil }

func lspTestSpawner(server *lspFakeServer) lsp.Spawner {
	return func(ctx context.Context, command string, args []string, dir string) (lsp.ProcConn, error) {
		toServerR, toServerW := io.Pipe()
		fromServerR, fromServerW := io.Pipe()
		go func() {
			reader := bufio.NewReader(toServerR)
			for {
				frame, err := readLspFrame(reader)
				if err != nil {
					return
				}
				var req struct {
					ID     *int            `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal(frame, &req); err != nil {
					continue
				}
				server.mu.Lock()
				server.received = append(server.received, req.Method)
				server.mu.Unlock()
				if req.ID == nil {
					continue
				}
				var body string
				switch req.Method {
				case "initialize":
					body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"capabilities":{}}}`, *req.ID)
				case "textDocument/definition":
					body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":[{"targetUri":"file:///x/store.go","targetRange":{"start":{"line":41,"character":5},"end":{"line":41,"character":10}}}]}`, *req.ID)
				default:
					body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":null}`, *req.ID)
				}
				_, _ = fmt.Fprintf(fromServerW, "Content-Length: %d\r\n\r\n%s", len(body), body)
			}
		}()
		return &lspFakeProc{stdin: toServerW, stdout: fromServerR}, nil
	}
}

func readLspFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		name, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			_, _ = fmt.Sscanf(strings.TrimSpace(value), "%d", &length)
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("frame without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// seamRunner resolves specs through a registry exactly like the coordinator's
// toolRunner, so the test below proves the AGENTS.md seam: prepareTools shape
// to operation manager to executor to tool.
type seamRunner struct {
	reg *Registry
}

func (r *seamRunner) RunTool(ctx context.Context, spec operation.Spec) (string, error) {
	t, ok := r.reg.Get(spec.Type)
	if !ok {
		return "", fmt.Errorf("unknown tool type: %s", spec.Type)
	}
	if runner, ok := t.(operation.ContextRunner); ok {
		return runner.RunContext(ctx, spec)
	}
	runner, ok := t.(Runner)
	if !ok {
		return "", fmt.Errorf("tool %s does not support Run", spec.Type)
	}
	return runner.Run(spec)
}

// TestLspSeamTranslateDispatchExecute drives a definition call from Translate
// through a real operation manager to the fake server, reading the formatted
// result off Results(). If the spec type ever stops resolving to the hidden
// driver, this fails: a tool that is only registered is offered to the model
// and then fails on every call.
func TestLspSeamTranslateDispatchExecute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	content := "package main\n\nfunc run() {}\n\nfunc main() { run() }\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	server := &lspFakeServer{}
	base := NewLspTool()
	spawn := lspTestSpawner(server)
	base.newManager = func(rootDir string) (*lsp.Manager, error) {
		return lsp.NewManagerWithSpawn("/fake/gopls", rootDir, spawn, time.Minute)
	}
	reg := NewRegistry()
	reg.RegisterHidden(base)
	for _, actionTool := range LspActionTools(base) {
		reg.Register(actionTool)
	}
	var actionTool Tool
	for _, tool := range reg.Offered() {
		if tool.Name() == "lsp_definition" {
			actionTool = tool
		}
	}
	if actionTool == nil {
		t.Fatal("lsp_definition not offered")
	}
	spec, err := actionTool.Translate(map[string]any{"path": "main.go", "line": 5, "symbol": "run"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	spec.Args["workdir"] = dir
	opMgr := operation.NewWithBuffer(&seamRunner{reg: reg}, 8)
	opMgr.Add(operation.Operation{ID: "op1", Spec: spec})
	select {
	case result := <-opMgr.Results():
		if result.Error != "" {
			t.Fatalf("result error: %s", result.Error)
		}
		if !strings.Contains(result.Output, "/x/store.go:42") {
			t.Errorf("output = %q, want the linked definition as path:line", result.Output)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no result from the operation manager")
	}
}

// TestLspUnavailableNamesTheInstall proves a host without gopls fails as a
// named gap instead of a hang or a panic. The factory error is the whole path:
// no server is ever spawned.
func TestLspUnavailableNamesTheInstall(t *testing.T) {
	base := NewLspTool()
	base.newManager = func(rootDir string) (*lsp.Manager, error) {
		return nil, fmt.Errorf("lsp unavailable: gopls not found on PATH; install golang.org/x/tools/gopls and retry")
	}
	out, err := base.RunContext(context.Background(), operation.Spec{
		Type: "lsp",
		Args: map[string]any{"action": "diagnostics", "path": "a.go", "workdir": t.TempDir()},
	})
	if err == nil {
		t.Fatalf("expected the missing binary to fail, got %q", out)
	}
	if !strings.Contains(err.Error(), "gopls") {
		t.Errorf("error = %q, want it to name the missing binary", err)
	}
}

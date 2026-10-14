package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager owns one gopls subprocess and speaks JSON-RPC to it over stdio.
//
// The process sits behind this type so a crash surfaces as an ordinary tool
// error instead of taking the caller down, and so tests can substitute a fake
// server without a real language toolchain. One manager serves one project
// root; the tool layer keeps one per root directory.
//
// gopls frames messages with Content-Length headers, not bare newlines, so the
// transport reads headers first and then exactly the announced body. Server
// notifications (which carry no id) are consumed and dropped by the read loop:
// they are progress chatter, and holding them for a later reader would misroute
// a response.
type Manager struct {
	goplsPath   string
	rootDir     string
	spawn       Spawner
	idleTimeout time.Duration

	mu        sync.Mutex
	proc      ProcConn
	reader    *bufio.Reader
	nextID    int
	opened    map[string]bool
	idleTimer *time.Timer
}

// ProcConn is the narrow process surface the manager needs. The production
// spawner wraps os/exec; tests substitute scripted pipes.
type ProcConn interface {
	Stdin() io.Writer
	Stdout() io.Reader
	Close() error
}

// Spawner starts the language server. Exported so callers can substitute a
// fake server in tests; production always uses spawnExec.
type Spawner func(ctx context.Context, command string, args []string, dir string) (ProcConn, error)

type execProc struct {
	cmd    *exec.Cmd
	stdin  io.Writer
	stdout io.Reader
}

func (p *execProc) Stdin() io.Writer  { return p.stdin }
func (p *execProc) Stdout() io.Reader { return p.stdout }
func (p *execProc) Close() error      { return p.cmd.Process.Kill() }

func spawnExec(ctx context.Context, command string, args []string, dir string) (ProcConn, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	// gopls must not inherit the daemon's stderr: a server that logs there
	// pollutes the supervisor log with chatter no operator asked for.
	cmd.Stderr = nil
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// Timeouts: a language server that cannot answer in 30 seconds is wedged, and
// failing the call beats stalling the run. Idle shutdown mirrors the browser
// manager so a forgotten server does not live forever.
const (
	lspRequestTimeout = 30 * time.Second
	lspIdleTimeout    = 5 * time.Minute
)

// ResolveServer finds gopls the way OMP finds language binaries: project-local
// bin directories first, then PATH. A missing binary is an ordinary error, not
// a build failure, so registries without a Go toolchain still construct.
func ResolveServer(workDir string) (string, error) {
	if strings.TrimSpace(workDir) != "" {
		for _, local := range []string{filepath.Join(workDir, "bin", "gopls"), filepath.Join(workDir, "gopls")} {
			if info, err := os.Stat(local); err == nil && !info.IsDir() {
				return local, nil
			}
		}
	}
	path, err := exec.LookPath("gopls")
	if err != nil {
		return "", fmt.Errorf("lsp unavailable: gopls not found on PATH; install golang.org/x/tools/gopls and retry")
	}
	return path, nil
}

// NewManager builds a manager that spawns goplsPath on first use. rootDir may
// be relative; it is made absolute because the initialize handshake needs a
// file URI and a relative one names nothing.
func NewManager(goplsPath, rootDir string) (*Manager, error) {
	return NewManagerWithSpawn(goplsPath, rootDir, spawnExec, lspIdleTimeout)
}

// NewManagerWithSpawn builds a manager with an explicit spawner and idle
// timeout. It exists so tests can drive the full protocol against scripted
// pipes instead of a real language toolchain.
func NewManagerWithSpawn(goplsPath, rootDir string, spawn Spawner, idleTimeout time.Duration) (*Manager, error) {
	if strings.TrimSpace(goplsPath) == "" {
		return nil, fmt.Errorf("lsp unavailable: no gopls path")
	}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	return &Manager{
		goplsPath:   goplsPath,
		rootDir:     abs,
		spawn:       spawn,
		opened:      map[string]bool{},
		idleTimeout: idleTimeout,
	}, nil
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// StripFileURI renders a file:// URI back into a path, unescaping it. A bare
// path passes through, because not every server bothers with the scheme.
func StripFileURI(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		if unescaped, err := url.PathUnescape(strings.TrimPrefix(uri, "file://")); err == nil {
			return unescaped
		}
		return strings.TrimPrefix(uri, "file://")
	}
	return uri
}

// ensureStartedLocked spawns and handshakes exactly once per process lifetime.
// Callers hold mu.
func (m *Manager) ensureStartedLocked(ctx context.Context) error {
	if m.proc != nil {
		return nil
	}
	// The process outlives any single request, so it is spawned detached from
	// the call's timeout: binding it to a per-request context would kill the
	// server the moment the first call returned.
	proc, err := m.spawn(context.WithoutCancel(ctx), m.goplsPath, []string{"serve"}, m.rootDir)
	if err != nil {
		return fmt.Errorf("lsp: start gopls: %w", err)
	}
	m.proc = proc
	m.reader = bufio.NewReader(proc.Stdout())
	m.opened = map[string]bool{}
	params := map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      fileURI(m.rootDir),
		"capabilities": map[string]any{},
	}
	if _, err := m.callLocked(ctx, "initialize", params); err != nil {
		m.shutdownLocked()
		return fmt.Errorf("lsp: initialize: %w", err)
	}
	if err := m.notifyLocked("initialized", map[string]any{}); err != nil {
		m.shutdownLocked()
		return fmt.Errorf("lsp: initialized: %w", err)
	}
	return nil
}

// touchLocked restarts the idle timer. A forgotten server exits on its own.
func (m *Manager) touchLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	timeout := m.idleTimeout
	if timeout <= 0 {
		timeout = lspIdleTimeout
	}
	m.idleTimer = time.AfterFunc(timeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.shutdownLocked()
	})
}

func (m *Manager) shutdownLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	if m.proc != nil {
		_ = m.proc.Close()
		m.proc = nil
	}
	m.reader = nil
	m.opened = map[string]bool{}
}

// Close releases the server, if any. A later call restarts it on demand.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdownLocked()
}

func writeFrame(w io.Writer, body []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body))
	if err != nil {
		return err
	}
	_, err = w.Write(body)
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
				return nil, fmt.Errorf("lsp: bad Content-Length %q", value)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("lsp: frame without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// callLocked sends one request and reads until its response arrives.
// Notifications interleaved on the stream are dropped. Callers hold mu.
func (m *Manager) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	m.nextID++
	id := m.nextID
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}
	if err := writeFrame(m.proc.Stdin(), body); err != nil {
		m.shutdownLocked()
		return nil, fmt.Errorf("lsp: write %s: %w", method, err)
	}
	// One reader per process lifetime. A fresh bufio.Reader per call would strand
	// bytes the previous reader already buffered, and the next call would block
	// on a stream whose data was consumed into a discarded buffer.
	reader := m.reader
	for {
		if ctx.Err() != nil {
			m.shutdownLocked()
			return nil, fmt.Errorf("lsp: %s: %w", method, ctx.Err())
		}
		frame, err := readFrame(reader)
		if err != nil {
			m.shutdownLocked()
			return nil, fmt.Errorf("lsp: read %s: %w", method, err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(frame, &resp); err != nil {
			continue
		}
		if resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("lsp: %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (m *Manager) notifyLocked(method string, params any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	return writeFrame(m.proc.Stdin(), body)
}

// openLocked sends didOpen once per file per process lifetime. gopls answers
// most requests against open documents, and reopening on every call would also
// reset the server's view of unsaved state the agent just wrote.
func (m *Manager) openLocked(path string) error {
	if m.opened[path] {
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	err = m.notifyLocked("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        fileURI(path),
			"languageId": "go",
			"version":    1,
			"text":       string(content),
		},
	})
	if err != nil {
		return err
	}
	m.opened[path] = true
	return nil
}

// Position is 0-based, matching the protocol. The tool layer converts the
// 1-based lines the model uses with read.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span in one document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location names a span in a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic is one pull-diagnostics item.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     any    `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

// Symbol is one flattened document symbol.
type Symbol struct {
	Name string `json:"name"`
	Kind int    `json:"kind"`
	Path string `json:"path"`
	Line int    `json:"line"`
}

// requireAbs refuses a relative path at the manager boundary. Resolution
// against the session workdir is the tool layer's job; by the time a path gets
// here there is no workdir left to resolve it against, and guessing one would
// send gopls a URI that names nothing.
func requireAbs(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("lsp: relative path %q reached the server; resolve it first", path)
	}
	return nil
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, lspRequestTimeout)
}

// Diagnostics pulls the server's current diagnostics for one file. Pull, not
// push: subscribing to publishDiagnostics would need a background reader and a
// cache, which is a maintained index wearing a subscription hat.
func (m *Manager) Diagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	if err := requireAbs(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	m.touchLocked()
	if err := m.ensureStartedLocked(ctx); err != nil {
		return nil, err
	}
	if err := m.openLocked(path); err != nil {
		return nil, err
	}
	raw, err := m.callLocked(ctx, "textDocument/diagnostic", map[string]any{
		"textDocument":     map[string]any{"uri": fileURI(path)},
		"previousResultId": "",
	})
	if err != nil {
		return nil, err
	}
	var report struct {
		Kind  string       `json:"kind"`
		Items []Diagnostic `json:"items"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("lsp: decode diagnostics: %w", err)
	}
	return report.Items, nil
}

// normalizeLocations accepts every shape the spec allows for a goto answer: a
// single Location, an array of them, or an array of LocationLinks.
func normalizeLocations(raw json.RawMessage) ([]Location, error) {
	var single Location
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		return []Location{single}, nil
	}
	var many []Location
	if err := json.Unmarshal(raw, &many); err == nil {
		// Unknown fields are ignored, so a LocationLink array also decodes
		// here as locations with empty URIs. Only a decode that actually
		// named documents counts.
		named := many[:0]
		for _, loc := range many {
			if loc.URI != "" {
				named = append(named, loc)
			}
		}
		if len(named) > 0 {
			return named, nil
		}
	}
	var links []struct {
		TargetURI   string `json:"targetUri"`
		TargetRange Range  `json:"targetRange"`
	}
	if err := json.Unmarshal(raw, &links); err != nil {
		return nil, fmt.Errorf("lsp: decode locations: %w", err)
	}
	out := make([]Location, 0, len(links))
	for _, link := range links {
		out = append(out, Location{URI: link.TargetURI, Range: link.TargetRange})
	}
	return out, nil
}

func (m *Manager) gotoRequest(ctx context.Context, method, path string, line, character int) ([]Location, error) {
	if err := requireAbs(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	m.touchLocked()
	if err := m.ensureStartedLocked(ctx); err != nil {
		return nil, err
	}
	if err := m.openLocked(path); err != nil {
		return nil, err
	}
	raw, err := m.callLocked(ctx, method, map[string]any{
		"textDocument": map[string]any{"uri": fileURI(path)},
		"position":     Position{Line: line, Character: character},
	})
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	return normalizeLocations(raw)
}

// Definition locates where the symbol at a 0-based position is defined.
func (m *Manager) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	return m.gotoRequest(ctx, "textDocument/definition", path, line, character)
}

// References lists where the symbol at a 0-based position is used, including
// its declaration.
func (m *Manager) References(ctx context.Context, path string, line, character int) ([]Location, error) {
	if err := requireAbs(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	m.touchLocked()
	if err := m.ensureStartedLocked(ctx); err != nil {
		return nil, err
	}
	if err := m.openLocked(path); err != nil {
		return nil, err
	}
	raw, err := m.callLocked(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(path)},
		"position":     Position{Line: line, Character: character},
		"context":      map[string]any{"includeDeclaration": true},
	})
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	return normalizeLocations(raw)
}

// Hover returns the server's hover text for a 0-based position, or "" when the
// server has nothing to say about it. Empty is a usable answer, not a failure:
// hovering whitespace legitimately yields nothing.
func (m *Manager) Hover(ctx context.Context, path string, line, character int) (string, error) {
	if err := requireAbs(path); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	m.touchLocked()
	if err := m.ensureStartedLocked(ctx); err != nil {
		return "", err
	}
	if err := m.openLocked(path); err != nil {
		return "", err
	}
	raw, err := m.callLocked(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(path)},
		"position":     Position{Line: line, Character: character},
	})
	if err != nil {
		return "", err
	}
	if string(raw) == "null" {
		return "", nil
	}
	var hover struct {
		Contents any `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", fmt.Errorf("lsp: decode hover: %w", err)
	}
	return hoverText(hover.Contents), nil
}

func hoverText(contents any) string {
	switch value := contents.(type) {
	case string:
		return value
	case map[string]any:
		text, _ := value["value"].(string)
		return text
	case []any:
		var parts []string
		for _, item := range value {
			if text := hoverText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// DocumentSymbols lists the symbols declared in one file, flattened to two
// levels. Document scope, not workspace scope: a workspace-wide symbol query is
// a maintained index query with the serial numbers filed off, and this tool is
// not that.
func (m *Manager) DocumentSymbols(ctx context.Context, path string) ([]Symbol, error) {
	if err := requireAbs(path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	m.touchLocked()
	if err := m.ensureStartedLocked(ctx); err != nil {
		return nil, err
	}
	if err := m.openLocked(path); err != nil {
		return nil, err
	}
	raw, err := m.callLocked(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(path)},
	})
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var tree []rawSymbol
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("lsp: decode symbols: %w", err)
	}
	var out []Symbol
	for _, sym := range tree {
		out = append(out, Symbol{Name: sym.Name, Kind: sym.Kind, Path: path, Line: sym.Range.Start.Line + 1})
		for _, child := range sym.Children {
			out = append(out, Symbol{Name: sym.Name + "." + child.Name, Kind: child.Kind, Path: path, Line: child.Range.Start.Line + 1})
		}
	}
	return out, nil
}

type rawSymbol struct {
	Name     string      `json:"name"`
	Kind     int         `json:"kind"`
	Range    Range       `json:"range"`
	Children []rawSymbol `json:"children"`
}

// SymbolKindName renders the LSP SymbolKind number a person can read. Unknown
// numbers print as-is rather than failing, because a server with a newer spec
// than this table is still usable.
func SymbolKindName(kind int) string {
	names := map[int]string{
		1: "File", 2: "Module", 3: "Namespace", 4: "Package", 5: "Class",
		6: "Method", 7: "Property", 8: "Field", 9: "Constructor", 10: "Enum",
		11: "Interface", 12: "Function", 13: "Variable", 14: "Constant",
		15: "String", 16: "Number", 17: "Boolean", 18: "Array", 19: "Object",
		20: "Key", 21: "Null", 22: "EnumMember", 23: "Struct", 24: "Event",
		25: "Operator", 26: "TypeParameter",
	}
	if name, ok := names[kind]; ok {
		return name
	}
	return strconv.Itoa(kind)
}

// ResolveCharacter turns the model's line-level addressing into the protocol's
// character offset. A bare line is not enough: a line holds several symbols and
// the server needs to know which one. The model either names the character
// directly or names the symbol text, which is looked up on that line. Both
// absent is a caller error, stated as one, because guessing a column would
// answer about the wrong symbol.
func ResolveCharacter(lineText, symbol string, character int, hasCharacter bool) (int, error) {
	if hasCharacter {
		if character < 0 {
			return 0, fmt.Errorf("character must be 0 or greater")
		}
		return character, nil
	}
	if symbol == "" {
		return 0, fmt.Errorf("character or symbol is required; a line alone names no symbol")
	}
	first := strings.Index(lineText, symbol)
	if first < 0 {
		return 0, fmt.Errorf("symbol %q not found on the given line", symbol)
	}
	if strings.Contains(lineText[first+len(symbol):], symbol) {
		return 0, fmt.Errorf("symbol %q occurs more than once on the given line; pass character to pick one", symbol)
	}
	return first, nil
}

// ReadLine returns one 1-based line of a file for symbol resolution.
func ReadLine(path string, line int) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if line < 1 || line > len(lines) {
		return "", fmt.Errorf("line %d out of range; %s has %d lines", line, path, len(lines))
	}
	return lines[line-1], nil
}

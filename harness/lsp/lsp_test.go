package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer speaks header-framed JSON-RPC from a script table. It records
// every method it was asked for, so tests can assert the handshake shape, and
// answers from canned bodies, so no language toolchain is needed.
type fakeServer struct {
	mu       sync.Mutex
	received []string
	// answer returns the frames to write for one request, in order. A
	// multi-frame answer lets a test interleave a server notification before
	// the real response, proving the read loop drops it instead of decoding it.
	answer func(method string, id int) [][]byte
}

type fakeProc struct {
	stdin  io.Writer
	stdout io.Reader
	close  func() error
}

func (p *fakeProc) Stdin() io.Writer  { return p.stdin }
func (p *fakeProc) Stdout() io.Reader { return p.stdout }
func (p *fakeProc) Close() error      { return p.close() }

func runFake(t *testing.T, answer func(method string, id int) [][]byte) (*fakeServer, Spawner) {
	t.Helper()
	server := &fakeServer{answer: answer}
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	go func() {
		reader := bufio.NewReader(toServerR)
		for {
			frame, err := readFrame(reader)
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
			for _, frame := range server.answer(req.Method, *req.ID) {
				if err := writeFrame(fromServerW, frame); err != nil {
					return
				}
			}
		}
	}()
	spawn := func(ctx context.Context, command string, args []string, dir string) (ProcConn, error) {
		return &fakeProc{stdin: toServerW, stdout: fromServerR, close: func() error { return nil }}, nil
	}
	return server, spawn
}

func okResult(id int, result string) [][]byte {
	return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result))}
}

func errResult(id int, message string) [][]byte {
	return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":%q}}`, id, message))}
}

func notifyFrame() []byte {
	return []byte(`{"jsonrpc":"2.0","method":"window/logMessage","params":{}}`)
}

func (s *fakeServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, got := range s.received {
		if got == method {
			n++
		}
	}
	return n
}

func testManager(t *testing.T, server *fakeServer, spawn Spawner, root string) *Manager {
	t.Helper()
	m, err := NewManagerWithSpawn("/fake/gopls", root, spawn, time.Minute)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func writeTempGo(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/main.go"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}

func TestDefinitionRoundTripOpensOnce(t *testing.T) {
	path := writeTempGo(t, "package main\n\nfunc run() {}\n\nfunc main() { run() }\n")
	server, spawn := runFake(t, func(method string, id int) [][]byte {
		switch method {
		case "initialize":
			return okResult(id, `{"capabilities":{}}`)
		case "textDocument/definition":
			return okResult(id, `[{"targetUri":"file:///x/a.go","targetRange":{"start":{"line":1,"character":5},"end":{"line":1,"character":8}}}]`)
		default:
			return okResult(id, `null`)
		}
	})
	m := testManager(t, server, spawn, t.TempDir())
	ctx := context.Background()
	first, err := m.Definition(ctx, path, 3, 20)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if len(first) != 1 || first[0].URI != "file:///x/a.go" || first[0].Range.Start.Line != 1 {
		t.Errorf("locations = %+v, want the linked target", first)
	}
	if _, err := m.Definition(ctx, path, 3, 20); err != nil {
		t.Fatalf("second definition: %v", err)
	}
	if got := server.count("initialize"); got != 1 {
		t.Errorf("initialize sent %d times, want once", got)
	}
	if got := server.count("textDocument/didOpen"); got != 1 {
		t.Errorf("didOpen sent %d times, want once per file per process", got)
	}
}

func TestNotificationsAreDroppedNotMisrouted(t *testing.T) {
	path := writeTempGo(t, "package main\n")
	server, spawn := runFake(t, func(method string, id int) [][]byte {
		if method == "initialize" {
			return okResult(id, `{"capabilities":{}}`)
		}
		// The notification carries no id. If the read loop treated it as the
		// answer, the hover below would decode chatter instead of the result.
		return [][]byte{notifyFrame(), okResult(id, `{"contents":"func run()"}`)[0]}
	})
	m := testManager(t, server, spawn, t.TempDir())
	text, err := m.Hover(context.Background(), path, 0, 0)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if text != "func run()" {
		t.Errorf("hover = %q, want the response after the dropped notification", text)
	}
}

func TestServerErrorNamesTheMethod(t *testing.T) {
	path := writeTempGo(t, "package main\n")
	_, spawn := runFake(t, func(method string, id int) [][]byte {
		if method == "initialize" {
			return okResult(id, `{"capabilities":{}}`)
		}
		return errResult(id, "no such method")
	})
	m := testManager(t, &fakeServer{}, spawn, t.TempDir())
	if _, err := m.References(context.Background(), path, 0, 0); err == nil {
		t.Fatal("expected the server error to surface")
	} else if !strings.Contains(err.Error(), "textDocument/references") || !strings.Contains(err.Error(), "no such method") {
		t.Errorf("error = %q, want method and server message", err)
	}
}

func TestRelativePathNeverReachesTheServer(t *testing.T) {
	called := false
	spawn := func(ctx context.Context, command string, args []string, dir string) (ProcConn, error) {
		called = true
		return nil, fmt.Errorf("must not spawn")
	}
	m, err := NewManagerWithSpawn("/fake/gopls", t.TempDir(), spawn, time.Minute)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := m.Diagnostics(context.Background(), "relative/file.go"); err == nil {
		t.Fatal("expected a relative path to be refused")
	} else if !strings.Contains(err.Error(), "relative path") {
		t.Errorf("error = %q, want the resolution complaint", err)
	}
	if called {
		t.Error("the server was spawned for a path it could never name")
	}
}

func TestResolveCharacterRules(t *testing.T) {
	line := "func run() {}"
	if got, err := ResolveCharacter(line, "", 5, true); err != nil || got != 5 {
		t.Errorf("explicit char = %d, %v; want 5, nil", got, err)
	}
	if _, err := ResolveCharacter(line, "", -1, true); err == nil {
		t.Error("negative character accepted")
	}
	if got, err := ResolveCharacter(line, "run", 0, false); err != nil || got != 5 {
		t.Errorf("symbol char = %d, %v; want 5, nil", got, err)
	}
	if _, err := ResolveCharacter(line, "missing", 0, false); err == nil {
		t.Error("absent symbol accepted")
	}
	if _, err := ResolveCharacter("run run", "run", 0, false); err == nil {
		t.Error("ambiguous symbol accepted without a character")
	} else if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error = %q, want the disambiguation ask", err)
	}
	if _, err := ResolveCharacter(line, "", 0, false); err == nil {
		t.Error("bare line accepted with neither character nor symbol")
	}
}

func TestReadLineBounds(t *testing.T) {
	path := writeTempGo(t, "one\ntwo\n")
	if got, err := ReadLine(path, 2); err != nil || got != "two" {
		t.Errorf("line 2 = %q, %v; want two, nil", got, err)
	}
	for _, line := range []int{0, 3, -1} {
		if _, err := ReadLine(path, line); err == nil {
			t.Errorf("line %d accepted out of range", line)
		}
	}
}

func TestResolveServerReportsTheInstall(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveServer(""); err == nil {
		t.Fatal("expected an error with no gopls on PATH")
	} else if !strings.Contains(err.Error(), "gopls") {
		t.Errorf("error = %q, want it to name the missing binary", err)
	}
}

func TestSymbolKindFallsBackToNumber(t *testing.T) {
	if got := SymbolKindName(12); got != "Function" {
		t.Errorf("kind 12 = %q, want Function", got)
	}
	if got := SymbolKindName(99); got != "99" {
		t.Errorf("unknown kind = %q, want the number", got)
	}
}

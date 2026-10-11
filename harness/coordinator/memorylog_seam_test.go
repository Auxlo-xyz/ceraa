package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Memory.md is the sidekick source: the append-only log the runner reads for
// what has been learned.
//
// What is left has to be proven at the wire, not at annotate. annotate is one term
// in a sum at the runner's result loop, and a sum can lose a term: every unit test
// below calls the state directly, which is the shape that passes while the model
// never sees the note. These drive a whole Runner.Run against a fake provider and
// read the tool-result body off the request, which is what the model actually got.

// memoryLogSeamRun drives one real run against a fake provider: the model reads a
// file, then answers. It returns the tool-result bodies and the user message
// bodies exactly as they went on the wire, so a test can assert on what the
// model saw rather than on what was assembled.
//
// It configures no store and no third channel. The log, when the caller wants one,
// is configured through extra: that is the whole point of the shape, because a
// helper that quietly supplied the store would let a broken log path look fine.
func memoryLogSeamRun(t *testing.T, sessionID string, extra func(*RunnerConfig)) ([]string, []string, *sessionstore.Session, *Runner) {
	t.Helper()
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := os.WriteFile(target, []byte("the file the agent read"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var toolResults []string
	var userTexts []string
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, m := range body.Messages {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
			if m.Role == "user" {
				userTexts = append(userTexts, m.Content)
			}
		}
		mu.Unlock()

		var payload map[string]any
		if requests.Add(1) == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"id":       "call1",
					"function": map[string]any{"name": "read", "arguments": fmt.Sprintf(`{"path":%q}`, target)},
				}},
			}}}}
		} else {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}}}}
		}
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := RunnerConfig{
		SessionID:       sessionID,
		SystemPrompt:    "STATIC",
		Model:           llm.FromProvider(server.URL, "test", "test"),
		ModelCatalogURL: "http://127.0.0.1:1",
		WorkDir:         work,
		MaxRequests:     4,
		MemoryDelivery:  string(memory.DeliverySidekick),
	}
	if extra != nil {
		extra(&cfg)
	}
	runner := New(cfg, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "read the notes"); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), toolResults...), append([]string(nil), userTexts...), sess, runner
}

// Memory reaches the model exactly once per run, as a tail line on the newest
// user message, rather than as a turn record written after the run or a note
// on every tool result.
func TestMemoryTailLineRidesTheNewestUserMessageOnce(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "Memory.md")
	appendLog(t, memory.OpenMemoryFile(logPath),
		"TAIL_ALPHA: prefer tabs over spaces",
		"TAIL_BETA: ship on fridays",
	)
	runner := New(RunnerConfig{
		SessionID:      "tail",
		SystemPrompt:   "you are a test agent",
		Model:          llm.FromProvider("http://127.0.0.1:1", "k", "m"),
		ContextLimit:   32000,
		WorkDir:        dir,
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryFile:     logMemoryFile(logPath),
	}, nil, tool.NewRegistry())
	sess := &sessionstore.Session{
		ID: "tail",
		Messages: []sessionstore.Message{
			{Role: llm.RoleUser, Content: "first question"},
			{Role: llm.RoleAssistant, Content: "first answer"},
			{Role: llm.RoleUser, Content: "latest question"},
		},
	}
	msgs := runner.messages(sess, nil)
	if len(msgs) == 0 {
		t.Fatal("messages built nothing, so the test proved nothing")
	}
	carrying := 0
	for _, m := range msgs {
		carrying += strings.Count(m.Text, "<memory>")
	}
	if carrying != 1 {
		t.Fatalf("the memory block reaches the model %d times, want exactly once: %#v", carrying, msgs)
	}
	newest := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			newest = i
			break
		}
	}
	if newest < 0 {
		t.Fatal("no user message in the built request")
	}
	got := msgs[newest].Text
	if !strings.HasPrefix(got, "latest question") {
		t.Errorf("the tail line prepended rather than suffixed: %q", got)
	}
	if !strings.HasSuffix(got, "</memory>") {
		t.Errorf("the memory block is not at the end of the newest user message: %q", got)
	}
	for _, want := range []string{"TAIL_ALPHA", "TAIL_BETA"} {
		if !strings.Contains(got, want) {
			t.Errorf("the tail line lost %q: %q", want, got)
		}
	}
	for _, m := range msgs[:newest] {
		if strings.Contains(m.Text, "<memory>") {
			t.Errorf("memory leaked outside the newest user message: %+v", m)
		}
	}
}

// With no log the request is untouched: a missing file must not reshape the bytes the model sees.
func TestMessagesWithoutMemoryAreByteIdentical(t *testing.T) {
	mkSess := func() *sessionstore.Session {
		return &sessionstore.Session{
			ID: "tail-empty",
			Messages: []sessionstore.Message{
				{Role: llm.RoleUser, Content: "hello"},
			},
		}
	}
	// One workdir for all three runners: the system prompt names it, so a
	// fresh dir per runner would differ before memory is even considered.
	dir := t.TempDir()
	mkRunner := func(mf func() *memory.MemoryFile) *Runner {
		return New(RunnerConfig{
			SessionID:    "tail-empty",
			SystemPrompt: "you are a test agent",
			Model:        llm.FromProvider("http://127.0.0.1:1", "k", "m"),
			ContextLimit: 32000,
			WorkDir:      dir,
			MemoryFile:   mf,
		}, nil, tool.NewRegistry())
	}
	emptyPath := filepath.Join(t.TempDir(), "Memory.md")
	if err := os.WriteFile(emptyPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	builds := map[string][]llm.Item{
		"nil reader":   mkRunner(nil).messages(mkSess(), nil),
		"missing file": mkRunner(logMemoryFile(filepath.Join(t.TempDir(), "absent", "Memory.md"))).messages(mkSess(), nil),
		"empty log":    mkRunner(logMemoryFile(emptyPath)).messages(mkSess(), nil),
	}
	base := builds["nil reader"]
	for name, msgs := range builds {
		if len(msgs) != len(base) {
			t.Fatalf("%s built %d messages, want %d", name, len(msgs), len(base))
		}
		for i := range base {
			a, b := base[i], msgs[i]
			if a.Kind != b.Kind || a.Role != b.Role || a.Text != b.Text {
				t.Errorf("%s message %d differs: %+v vs %+v", name, i, b, a)
			}
			if strings.Contains(b.Text, "<memory>") {
				t.Errorf("%s carries a memory block with nothing logged: %q", name, b.Text)
			}
		}
	}
}

// The log alone, at the wire. This is the configuration production has now: one
// source, no store. If the log stopped reaching the model - because the
// construction condition no longer builds the state, or because the tail read
// stopped - the run would still succeed and the agent would silently lose
// everything it had learned.
func TestTheMemoryLogAloneReachesTheModelOnTheTailLine(t *testing.T) {
	toolResults, userTexts, _, runner := memoryLogSeamRun(t, "memfile-only", func(cfg *RunnerConfig) {
		file := memory.OpenMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
		appendLog(t, file,
			"GENERIC: an older standing note",
			"LOG_ONLY: "+filepath.Join(cfg.WorkDir, "notes.txt")+" is generated, never hand-edit it",
		)
		cfg.MemoryFile = logMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
	})

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	// Tool results carry no memory: the tail line is the single delivery.
	for _, text := range toolResults {
		if strings.Contains(text, "LOG_ONLY") || strings.Contains(text, "<memory>") {
			t.Errorf("a tool result carries memory text: %q", text)
		}
	}
	// The newest user message carries the entry exactly once per request.
	carrying := 0
	for _, text := range userTexts {
		if !strings.Contains(text, "LOG_ONLY") {
			continue
		}
		carrying++
		if !strings.HasSuffix(text, "</memory>") {
			t.Errorf("the memory block is not a suffix of the user message: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no user message the model reads carries the log entry: %#v", userTexts)
	}
	if got := runner.systemPrompt(); strings.Contains(got, "LOG_ONLY") {
		t.Error("the log entry is in the system prompt, so the cache prefix is volatile again")
	}
}

// The log alone builds the state: no other source is configured, and the log
// has to be enough on its own.
func TestTheSidekickIsBuiltAndReadsWithOnlyTheMemoryFileSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path),
		"UNIT_ONLY: /srv/api is the backend",
	)
	runner := New(RunnerConfig{
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryFile:     logMemoryFile(path),
	}, nil, tool.NewRegistry())

	if runner.sidekick == nil {
		t.Fatal("no sidekick state was built from the log alone, so the log has no path to the model")
	}
	if tail := runner.memoryTailLine(); !strings.Contains(tail, "UNIT_ONLY") {
		t.Errorf("the tail line did not render the log entry: %q", tail)
	}
}

// Nothing configured means nothing built and nothing delivered. A nil source has
// to fall all the way through to "" rather than returning a half-answer.
func TestASidekickWithNoSourceAtAllDeliversNothing(t *testing.T) {
	runner := New(RunnerConfig{
		MemoryDelivery: string(memory.DeliverySidekick),
	}, nil, tool.NewRegistry())
	if runner.sidekick != nil {
		t.Error("sidekick state was built with no log")
	}
	if tail := runner.memoryTailLine(); tail != "" {
		t.Errorf("a run with no source delivered: %q", tail)
	}
	// The state itself, constructed the way a test or a future caller might: no
	// log at all.
	bare := newSidekickState("/srv")
	if tail := bare.memTail(); tail != nil {
		t.Errorf("a state with no source read %q", tail)
	}
}

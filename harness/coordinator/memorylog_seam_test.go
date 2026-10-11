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
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Memory.md is now the only sidekick source. It was the second of two, alongside
// the ranked store, and the bond was a third channel riding ahead of both on the
// first tool result. That third channel is gone: its content and its mechanism
// moved into the append-only log, so a separate delivery path would have been
// dead weight rather than a spare.
//
// What is left has to be proven at the wire, not at annotate. annotate is one term
// in a sum at the runner's result loop, and a sum can lose a term: every unit test
// below calls the state directly, which is the shape that passes while the model
// never sees the note. These drive a whole Runner.Run against a fake provider and
// read the tool-result body off the request, which is what the model actually got.

// memoryLogSeamRun drives one real run against a fake provider: the model reads a
// file, then answers. It returns the tool-result bodies exactly as they went on
// the wire, so a test can assert on what the model saw rather than on what was
// assembled.
//
// It configures no store and no third channel. The log, when the caller wants one,
// is configured through extra: that is the whole point of the shape, because a
// helper that quietly supplied the store would let a broken log path look fine.
func memoryLogSeamRun(t *testing.T, sessionID string, extra func(*RunnerConfig)) ([]string, *sessionstore.Session, *Runner) {
	t.Helper()
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := os.WriteFile(target, []byte("the file the agent read"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var toolResults []string
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
		MemoryContextFunc: func() (string, error) {
			return "<memory>project memory</memory>", nil
		},
		MemoryDelivery: string(memory.DeliverySidekick),
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
	return append([]string(nil), toolResults...), sess, runner
}

// The log alone, at the wire. This is the configuration production has now: one
// source, no store, no bond. If the log stopped reaching the model - because the
// construction condition no longer builds the state, because the note was dropped
// from the result line, or because the tail read stopped - the run would still
// succeed and the agent would silently lose everything it had learned.
func TestTheMemoryLogAloneReachesTheModelOnTheToolResult(t *testing.T) {
	toolResults, _, runner := memoryLogSeamRun(t, "memfile-only", func(cfg *RunnerConfig) {
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
	carrying := 0
	for _, text := range toolResults {
		if !strings.Contains(text, "LOG_ONLY") {
			continue
		}
		carrying++
		if !strings.Contains(text, "<sidekick") || !strings.HasSuffix(strings.TrimSpace(text), "</sidekick>") {
			t.Errorf("the note is not a whole sidekick block: %q", text)
		}
		if !strings.Contains(text, "the file the agent read") {
			t.Errorf("the note replaced the tool output rather than accompanying it: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no tool result the model reads carries the log entry: %#v", toolResults)
	}
	// Once, not once per call: the entry names one path and the operation touched
	// it once, and a note repeated into the transcript is re-sent on every later
	// request for the rest of the session.
	if carrying != 1 {
		t.Errorf("the log entry reached the model %d times in one run", carrying)
	}
	if got := runner.systemPrompt(); strings.Contains(got, "LOG_ONLY") {
		t.Error("the log entry is in the system prompt, so the cache prefix is volatile again")
	}
	report := runner.DeliveryReport()
	if report.Mode != string(memory.DeliverySidekick) || report.TargetedInjects == 0 {
		t.Errorf("the run recorded no log delivery: %+v", report)
	}
	// A sidekick run must record nothing on the bond field: the channel is gone,
	// so a non-zero figure would mean a path survived that should not exist.
	if report.BondInjected != 0 {
		t.Errorf("the delivery report still reports %d bond bytes after the channel was deleted", report.BondInjected)
	}
}

// The state is built from the log alone, with no store configured. MemoryEntries
// is documented as optional, so this is a legal caller - and it is exactly the one
// that the old construction condition had to be widened for, with a bond standing
// in for "a source that is not the store". The log now has to be worth the same
// widening on its own.
func TestTheSidekickIsBuiltAndDeliversWithOnlyTheMemoryFileSet(t *testing.T) {
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
	got := runner.sidekickNote(sessionWithOp("a", "read", "/srv/api/main.go"), operation.ResultEvent{OpID: "a"})
	if !strings.Contains(got, "UNIT_ONLY") {
		t.Errorf("the log entry did not reach the tool result: %q", got)
	}
}

// The store path is untouched by the deletion, and it is still reachable: a caller
// mid-migration with a store and no log must keep delivering. Proved at the wire
// rather than at annotate because the same sum at the result loop serves it.
func TestTheStorePathStillReachesTheModelWithoutTheLog(t *testing.T) {
	toolResults, _, runner := memoryLogSeamRun(t, "store-only", func(cfg *RunnerConfig) {
		target := filepath.Join(cfg.WorkDir, "notes.txt")
		cfg.MemoryEntries = func() ([]memory.Entry, error) {
			return []memory.Entry{{
				Content: "STORE_ONLY: " + target + " is generated, never hand-edit it",
				Seen:    8, SeenAt: time.Now(), Applies: target,
			}}, nil
		}
	})

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	carrying := 0
	for _, text := range toolResults {
		if !strings.Contains(text, "STORE_ONLY") {
			continue
		}
		carrying++
		if !strings.Contains(text, "the file the agent read") {
			t.Errorf("the store note replaced the tool output rather than accompanying it: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no tool result the model reads carries the store memory: %#v", toolResults)
	}
	if carrying != 1 {
		t.Errorf("the store memory reached the model %d times in one run", carrying)
	}
	if got := runner.DeliveryReport(); got.Mode != string(memory.DeliverySidekick) || got.TargetedInjects == 0 {
		t.Errorf("the run recorded no store delivery: %+v", got)
	}
	if strings.Contains(runner.systemPrompt(), "STORE_ONLY") {
		t.Error("the store memory is in the system prompt, so the cache prefix is volatile again")
	}
}

// Nothing configured means nothing built and nothing delivered, and the deletion
// removed the early returns that used to answer for a channel that no longer
// exists. A nil source has to fall all the way through to "" rather than returning
// a half-answer: the only callers that ever got a non-empty block with no source
// were the ones carrying a bond.
func TestASidekickWithNoSourceAtAllDeliversNothing(t *testing.T) {
	runner := New(RunnerConfig{
		MemoryDelivery: string(memory.DeliverySidekick),
	}, nil, tool.NewRegistry())
	if runner.sidekick != nil {
		t.Error("sidekick state was built with no store, no log and no bond channel")
	}
	if got := runner.sidekickNote(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"}); got != "" {
		t.Errorf("a run with no source delivered: %q", got)
	}
	// The state itself, constructed the way a test or a future caller might: no
	// lookup, no log. This is the guard the bond used to short-circuit.
	bare := newSidekickState(nil, "/srv")
	if got := bare.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"}); got != "" {
		t.Errorf("a state with neither source delivered %q", got)
	}
	if report := bare.deliveryReport(); report != (memory.DeliveryReport{}) {
		t.Errorf("a state that delivered nothing reported delivery: %+v", report)
	}
}

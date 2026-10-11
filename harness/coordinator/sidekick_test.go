package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// A memory that did not arrive is worse than one that arrived uselessly, so the
// prompt block and the sidekick note have to reach the model through different
// paths, and only one may be live at a time.
func TestSidekickModeSuppressesThePromptBlock(t *testing.T) {
	entries := func() ([]memory.Entry, error) {
		return []memory.Entry{{
			Content: "prefers dark mode", Seen: 6,
			SeenAt: time.Now(), Applies: "/srv/api",
		}}, nil
	}

	prompt := &Runner{cfg: RunnerConfig{
		MemoryContextFunc: func() (string, error) { return "<memory>block</memory>", nil },
	}}
	prompt.delivery = memory.DeliveryPrompt
	if got := prompt.memoryContext(); !strings.Contains(got, "<memory>") {
		t.Errorf("prompt mode lost the memory block: %q", got)
	}

	sidekick := &Runner{cfg: RunnerConfig{
		MemoryContextFunc: func() (string, error) { return "<memory>block</memory>", nil },
	}}
	sidekick.delivery = memory.DeliverySidekick
	sidekick.sidekick = newSidekickState(entries, "/srv/api")
	if got := sidekick.memoryContext(); got != "" {
		t.Errorf("sidekick mode still rendered the prompt block: %q", got)
	}
}

// The point of suppressing the block: the system prompt becomes fully static, so
// the whole thing is one cached prefix.
func TestSidekickModeIsReportedForMeasurement(t *testing.T) {
	sidekick := &Runner{cfg: RunnerConfig{
		MemoryContextFunc: func() (string, error) { return "<memory>x</memory>", nil },
	}}
	sidekick.delivery = memory.DeliverySidekick
	sidekick.sidekick = newSidekickState(func() ([]memory.Entry, error) { return nil, nil }, "/srv")

	report := sidekick.DeliveryReport()
	if report.Mode != string(memory.DeliverySidekick) {
		t.Errorf("mode = %q", report.Mode)
	}
	if report.PromptBlock {
		t.Error("the report claims a prompt block in sidekick mode, so a later analysis cannot trust it")
	}
}

func sessionWithOp(id, specType, path string) *sessionstore.Session {
	sess := &sessionstore.Session{}
	spec := map[string]any{}
	if path != "" {
		spec["path"] = path
	}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: id, SpecType: specType, SpecArgs: spec,
	})
	return sess
}

// The injection is a per-operation trigger, not a repeat of the prompt block. The
// general set fires once per run, because a pathless memory matches every
// operation and would otherwise be written into the transcript once per call.
func TestGeneralMemoryFiresOncePerRunNotOncePerCall(t *testing.T) {
	general := []memory.Entry{{
		Content: "prefers dark mode", Seen: 8, SeenAt: time.Now(),
	}}
	state := newSidekickState(func() ([]memory.Entry, error) { return general, nil }, "/srv")

	first := state.annotate(sessionWithOp("a", "write", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	second := state.annotate(sessionWithOp("b", "write", "/srv/b.txt"), operation.ResultEvent{OpID: "b"})

	if !strings.Contains(first, "prefers dark mode") {
		t.Fatalf("the first call delivered nothing: %q", first)
	}
	if strings.Contains(second, "prefers dark mode") {
		t.Errorf("the standing preference repeated on a later call, which is a copy per tool call in the transcript: %q", second)
	}
}

// Once per run, not once per session, and that asymmetry is deliberate. A standing
// preference has no trigger to fire on, so the only question is how often to repeat
// it; the targeted set is the one that repeats on a trigger of its own. Folding the
// general set into the session-scoped delivered set would stop re-delivering the
// baseline exactly when a compacted transcript has dropped it, which is the one
// moment it is worth having.
func TestGeneralMemoriesFireAgainOnTheNextRun(t *testing.T) {
	general := []memory.Entry{{
		ID: "id-dark", Content: "prefers dark mode", Seen: 8, SeenAt: time.Now(),
	}}
	lookup := func() ([]memory.Entry, error) { return general, nil }

	first := &sessionstore.Session{ID: "runs", Version: 3}
	first.Operations = append(first.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/a.txt"},
	})
	if got := newSidekickState(lookup, "/srv").annotate(first, operation.ResultEvent{OpID: "a"}); !strings.Contains(got, "dark mode") {
		t.Fatalf("the first run delivered nothing: %q", got)
	}
	// Same session, next run: the runner is rebuilt, and the general set re-arms
	// with it. The targeted set does not, and that difference is the point.
	second := &sessionstore.Session{ID: "runs", Version: 3, MemoryDelivered: first.MemoryDelivered}
	second.Operations = append(second.Operations, sessionstore.Operation{
		ID: "b", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/b.txt"},
	})
	if got := newSidekickState(lookup, "/srv").annotate(second, operation.ResultEvent{OpID: "b"}); !strings.Contains(got, "dark mode") {
		t.Errorf("the standing preference was not re-armed on the next run: %q", got)
	}
}

// A targeted memory reaches the operation that touches its path, and only that
// one. This is the whole mechanism: the paper's finding was that uniform
// injection is what hurts, so relocating memory without gating it would have
// changed where the noise appears rather than how much of it there is.
func TestTargetedMemoryReachesOnlyItsOperation(t *testing.T) {
	entries := []memory.Entry{{
		Content: "JAVA_HOME is set here", Why: "gradle picked the wrong jdk",
		Seen: 8, SeenAt: time.Now(), Applies: "/srv/api",
	}}
	state := newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")

	match := state.annotate(sessionWithOp("a", "write", "/srv/api/main.go"), operation.ResultEvent{OpID: "a"})
	if !strings.Contains(match, "JAVA_HOME is set here") {
		t.Errorf("the matching operation was not annotated: %q", match)
	}
	if !strings.Contains(match, "gradle picked the wrong jdk") {
		t.Errorf("the rationale was dropped: %q", match)
	}

	other := state.annotate(sessionWithOp("b", "write", "/srv/web/index.js"), operation.ResultEvent{OpID: "b"})
	if strings.Contains(other, "JAVA_HOME") {
		t.Errorf("an unrelated operation was annotated: %q", other)
	}

	// The half that shipped. A second operation that also matches the same memory
	// used to be annotated with the same text again, and that copy was written into
	// the tool result and re-sent on every request for the rest of the session. The
	// test above could never catch it because it only ever drove one matching
	// operation.
	again := state.annotate(sessionWithOp("c", "write", "/srv/api/server.go"), operation.ResultEvent{OpID: "c"})
	if strings.Contains(again, "JAVA_HOME") {
		t.Errorf("a second matching operation repeated the memory: %q", again)
	}
	if report := state.deliveryReport(); report.Deduped != 1 {
		t.Errorf("the deduped delivery was not recorded, so a session where it did all the work is indistinguishable: %+v", report)
	}
}

// The same rule with ids, which is how a real store hands memories over. The entry
// above has none, so this is the path production actually takes.
func TestATargetedMemoryIsDeliveredOncePerID(t *testing.T) {
	entries := []memory.Entry{
		{ID: "id-api", Content: "the backend pins jdk 21", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api"},
		{ID: "id-web", Content: "the site builds on node 22", Seen: 8, SeenAt: time.Now(), Applies: "/srv/web"},
	}
	state := newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")

	first := state.annotate(sessionWithOp("a", "write", "/srv/api/main.go"), operation.ResultEvent{OpID: "a"})
	if !strings.Contains(first, "jdk 21") {
		t.Fatalf("the matching operation was not annotated: %q", first)
	}
	repeat := state.annotate(sessionWithOp("b", "read", "/srv/api/main.go"), operation.ResultEvent{OpID: "b"})
	if strings.Contains(repeat, "jdk 21") {
		t.Errorf("the memory repeated on a second read of the same file: %q", repeat)
	}
	// Suppressing one memory must not suppress the others: a different path is
	// different work, and it is the only other place the note can come from.
	other := state.annotate(sessionWithOp("c", "write", "/srv/web/app.js"), operation.ResultEvent{OpID: "c"})
	if !strings.Contains(other, "node 22") {
		t.Errorf("a different path lost its own memory: %q", other)
	}
	if strings.Contains(other, "jdk 21") {
		t.Errorf("an already-delivered memory rode along on another path's note: %q", other)
	}
}

// The scope is the session, not the run. The bridge builds a coordinator per
// Telegram message, so a set held only in the runner re-fires every memory on the
// next message - the same copy-per-call cost with the timing moved outside the run
// it was scoped to. This drives the round trip through a real store file, because
// "it is persisted" is the whole claim.
func TestATargetedMemoryStaysDeliveredInTheNextRun(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries := []memory.Entry{{
		ID: "id-api", Content: "the backend pins jdk 21", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api",
	}}
	lookup := func() ([]memory.Entry, error) { return entries, nil }

	sess := &sessionstore.Session{ID: "scope", Version: 3}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})
	first := newSidekickState(lookup, "/srv").annotate(sess, operation.ResultEvent{OpID: "a"})
	if !strings.Contains(first, "jdk 21") {
		t.Fatalf("the first run delivered nothing: %q", first)
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	// A new coordinator, which is what the next Telegram message builds.
	reloaded, err := store.Load("scope")
	if err != nil {
		t.Fatal(err)
	}
	reloaded.Operations = append(reloaded.Operations, sess.Operations...)
	next := newSidekickState(lookup, "/srv").annotate(reloaded, operation.ResultEvent{OpID: "a"})
	if strings.Contains(next, "jdk 21") {
		t.Errorf("the next run repeated a memory the transcript already carries: %q", next)
	}
}

// A compaction drops the tool results that carried the notes, so the memory behind
// one is no longer in context and delivering it again is not a repeat. This is the
// seam the epoch work calls; it has to clear both halves, because the persisted list
// is folded back in on the next call.
func TestForgettingDeliveredMemoryRearmsItForTheNextEpoch(t *testing.T) {
	entries := []memory.Entry{{
		ID: "id-api", Content: "the backend pins jdk 21", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api",
	}}
	sess := &sessionstore.Session{ID: "epoch", Version: 3}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})
	lookup := func() ([]memory.Entry, error) { return entries, nil }

	runner := &Runner{cfg: RunnerConfig{SessionID: "epoch"}, reg: tool.DefaultRegistry()}
	runner.sidekick = newSidekickState(lookup, "/srv")

	if got := runner.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); !strings.Contains(got, "jdk 21") {
		t.Fatalf("the first epoch delivered nothing: %q", got)
	}
	runner.ForgetDeliveredMemory(sess)
	if len(sess.MemoryDelivered) != 0 {
		t.Errorf("the persisted half survived the epoch reset: %v", sess.MemoryDelivered)
	}
	if got := runner.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); !strings.Contains(got, "jdk 21") {
		t.Errorf("the memory did not come back after the transcript dropped it: %q", got)
	}
}

// A carried set from an earlier epoch is honoured rather than re-derived, which is
// what makes restoring one worth having.
func TestMarkedMemoriesAreNotDeliveredAgain(t *testing.T) {
	entries := []memory.Entry{{
		ID: "id-api", Content: "the backend pins jdk 21", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api",
	}}
	sess := &sessionstore.Session{ID: "carried", Version: 3}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "write", SpecArgs: map[string]any{"path": "/srv/api/main.go"},
	})
	runner := &Runner{cfg: RunnerConfig{SessionID: "carried"}, reg: tool.DefaultRegistry()}
	runner.sidekick = newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")

	runner.MarkDeliveredMemory(sess, "id-api")
	if got := runner.sidekick.annotate(sess, operation.ResultEvent{OpID: "a"}); strings.Contains(got, "jdk 21") {
		t.Errorf("a memory marked as already delivered was injected again: %q", got)
	}
	if len(sess.MemoryDelivered) != 1 || sess.MemoryDelivered[0] != "id-api" {
		t.Errorf("the marked id was not carried onto the session: %v", sess.MemoryDelivered)
	}
}

// The per-block ceiling is not a per-run bound. Forty operations each rendering a
// full note is still forty notes, which is the flood the per-call problem was fixed
// to stop arriving by another route. Past the ceiling an operation gets nothing
// rather than a fragment, and what was withheld is recorded so a silent stop is not
// mistaken for a store with nothing to say.
func TestTheRunCeilingStopsInjectionRatherThanTrimmingIt(t *testing.T) {
	var entries []memory.Entry
	for i := 0; i < 30; i++ {
		entries = append(entries, memory.Entry{
			ID:      fmt.Sprintf("id%02d", i),
			Content: fmt.Sprintf("memory %d about this path", i),
			Why:     "because the build needs it",
			Seen:    8, SeenAt: time.Now(), Applies: fmt.Sprintf("/srv/p%02d", i),
		})
	}
	state := newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")

	delivered, spent := 0, 0
	for i := 0; i < 30; i++ {
		path := fmt.Sprintf("/srv/p%02d/main.go", i)
		note := state.annotate(sessionWithOp(fmt.Sprintf("op%02d", i), "write", path), operation.ResultEvent{OpID: fmt.Sprintf("op%02d", i)})
		if note == "" {
			continue
		}
		if len(note) > memory.SidekickBlockBytes {
			t.Fatalf("a single note is %d bytes, over the block ceiling", len(note))
		}
		if !strings.HasSuffix(note, "</sidekick>") {
			t.Fatalf("a note was cut inside its own markup: %q", note)
		}
		delivered++
		spent += len(note)
	}
	if spent > memory.SidekickRunBytes {
		t.Errorf("the run wrote %d bytes of notes, over the %d ceiling", spent, memory.SidekickRunBytes)
	}
	if delivered == 30 {
		t.Error("the run ceiling never engaged, so the test is not exercising it")
	}
	report := state.deliveryReport()
	if report.BudgetWithheld == 0 {
		t.Errorf("the withheld memories were not recorded: %+v", report)
	}
}

// A flood on one path is capped and says so, at the seam. The per-operation limit
// already caps how many memories reach the renderer, so the fixture is built to the
// line bound rather than to a count: three general lines and four targeted ones at
// the full 96 runes, plus a long path in the heading, come to about 1260 bytes,
// which is what makes the ceiling bind here instead of in a test that sets its own
// budget. The renderer owns the ceiling and the memory package owns its own test of
// it; this pins that the coordinator hands the selection over whole rather than
// pre-trimming behind the renderer's back, which would store bytes nobody reads.
func TestAFloodOnOnePathIsCappedInTheDeliveredNote(t *testing.T) {
	widest := strings.Repeat("a", 96)
	entries := []memory.Entry{
		{ID: "id-g0", Content: widest, Seen: 8, SeenAt: time.Now()},
		{ID: "id-g1", Content: widest, Seen: 8, SeenAt: time.Now()},
		{ID: "id-g2", Content: widest, Seen: 8, SeenAt: time.Now()},
	}
	path := "/srv/" + strings.Repeat("deep/", 8) + "api/main.go"
	for i := 0; i < 4; i++ {
		entries = append(entries, memory.Entry{
			ID: fmt.Sprintf("id%02d", i), Content: widest, Why: strings.Repeat("b", 96),
			Seen: 8, SeenAt: time.Now(), Applies: "/srv",
		})
	}
	state := newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")
	note := state.annotate(sessionWithOp("a", "write", path), operation.ResultEvent{OpID: "a"})

	if len(note) > memory.SidekickBlockBytes {
		t.Errorf("the note is %d bytes, over the %d ceiling", len(note), memory.SidekickBlockBytes)
	}
	if !strings.Contains(note, fmt.Sprint(memory.SidekickBlockBytes)) {
		t.Errorf("the note does not name its own ceiling: %q", note)
	}
	if !strings.Contains(note, "not shown") {
		t.Errorf("a truncated note does not admit what it left out: %q", note)
	}
	if !strings.HasSuffix(note, "</sidekick>") {
		t.Errorf("the note was cut inside its own markup: %q", note)
	}
}

// The other two modes put memory in the prompt and are untouched by any of this.
// Nothing here dedups, caps or withholds: the block is rendered whole by
// memoryBlock and lives in one place, so there is no per-call repetition to guard
// against in the first place. Cached puts it ahead of the seam rather than after
// it, which is why it reads through memoryBlock and not through memoryContext.
func TestPromptAndCachedModesCarryTheWholeBlockAndNoNote(t *testing.T) {
	entries := []memory.Entry{{
		ID: "id-api", Content: "the backend pins jdk 21", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api",
	}}
	block := "<memory>the backend pins jdk 21</memory>"
	for _, mode := range []memory.Delivery{memory.DeliveryPrompt, memory.DeliveryCached} {
		r := &Runner{cfg: RunnerConfig{
			SessionID:      "mode",
			MemoryContext:  block,
			MemoryDelivery: string(mode),
			MemoryEntries:  func() ([]memory.Entry, error) { return entries, nil },
		}}
		r.delivery = memory.ParseDelivery(string(mode))
		if r.delivery != mode {
			t.Fatalf("mode %q parsed as %q", mode, r.delivery)
		}
		if r.sidekick != nil {
			t.Errorf("mode %q built sidekick state, so a tool result can be annotated in a mode that has no notes", mode)
		}
		if got := r.memoryBlock(); !strings.Contains(got, "jdk 21") {
			t.Errorf("mode %q lost the memory block: %q", mode, got)
		}
		if got := r.memoryContext(); (mode == memory.DeliveryPrompt) != (got != "") {
			t.Errorf("mode %q put %q in the varying tail, which is the seam it must not invalidate", mode, got)
		}
		report := r.DeliveryReport()
		if report.PromptBlock != (mode == memory.DeliveryPrompt) {
			t.Errorf("mode %q reported prompt_block=%v", mode, report.PromptBlock)
		}
		if report.TargetedInjects != 0 || report.Deduped != 0 || report.BudgetWithheld != 0 {
			t.Errorf("mode %q recorded tool-result delivery: %+v", mode, report)
		}
	}
}

// The claim the whole mechanism rests on, at the seam production uses: the note is
// appended to the tool result, that string is written into the transcript, and the
// transcript is re-sent on every later request. Two reads of the same file in one
// run must leave one copy of the memory in the file, not two.
func TestOneCopyOfATargetedMemoryReachesTheStoredTranscript(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]any) {
			payload := map[string]any{"choices": []any{map[string]any{"delta": delta}}}
			encoded, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		if requests.Add(1) == 1 {
			calls := make([]any, 0, 2)
			for i, id := range []string{"call-1", "call-2"} {
				calls = append(calls, map[string]any{
					"index": i, "id": id, "type": "function",
					"function": map[string]any{"name": "read", "arguments": `{"path":"api/main.go"}`},
				})
			}
			send(map[string]any{"tool_calls": calls})
		} else {
			send(map[string]any{"content": "read it twice"})
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := New(RunnerConfig{
		SessionID:      "one-copy",
		SystemPrompt:   "You are Ceraa.",
		Model:          llm.ProviderConfig{BaseURL: server.URL, Model: "test", MaxRetries: 0},
		WorkDir:        dir,
		ToolOutputDir:  t.TempDir(),
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryEntries: func() ([]memory.Entry, error) {
			return []memory.Entry{{
				ID: "id-api", Content: "the backend pins jdk 21", Why: "gradle picked the wrong one",
				Seen: 8, SeenAt: time.Now(), Applies: filepath.Join(dir, "api"),
			}}, nil
		},
	}, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "read api/main.go twice"); err != nil {
		t.Fatal(err)
	}

	sess, err := store.Load("one-copy")
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, message := range sess.Messages {
		if message.Role != llm.RoleTool {
			continue
		}
		results++
		if got := strings.Count(message.ToolResult, "jdk 21"); got > 1 {
			t.Errorf("one tool result carries %d copies of the memory: %q", got, message.ToolResult)
		}
	}
	if results != 2 {
		t.Fatalf("the run produced %d tool results, so the test never drove two reads of the same file", results)
	}
	total := 0
	for _, message := range sess.Messages {
		if message.Role == llm.RoleTool {
			total += strings.Count(message.ToolResult, "jdk 21")
		}
	}
	if total != 1 {
		t.Errorf("the transcript carries %d copies of the memory; every one is re-sent on every later request", total)
	}
	if len(sess.MemoryDelivered) != 1 || sess.MemoryDelivered[0] != "id-api" {
		t.Errorf("the delivered set was not persisted for the next run: %v", sess.MemoryDelivered)
	}
}

// An operation with no path gets no targeted injection.
//
// A bash command has no single target, and extracting one means parsing a shell
// string, which is the opposite of the property this exists for: a rule that
// cannot be wrong.
func TestAnOperationWithNoPathIsNotAnnotated(t *testing.T) {
	entries := []memory.Entry{{
		Content: "JAVA_HOME is set here", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api",
	}}
	state := newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")

	got := state.annotate(sessionWithOp("a", "shell", ""), operation.ResultEvent{OpID: "a"})
	if strings.Contains(got, "JAVA_HOME") {
		t.Errorf("an operation with no path was annotated: %q", got)
	}
}

// An unreadable store must be silent rather than reported as an empty one, which
// would read as "there is nothing to remember" rather than "we could not look".
func TestAnUnreadableStoreDeliversNothing(t *testing.T) {
	state := newSidekickState(func() ([]memory.Entry, error) { return nil, os.ErrPermission }, "/srv")
	if got := state.annotate(sessionWithOp("a", "write", "/srv/a.txt"), operation.ResultEvent{OpID: "a"}); got != "" {
		t.Errorf("an unreadable store produced %q", got)
	}
}

// A shell command names its files as plain tokens, so the trigger has to see them.
//
// This is the coverage hole that made removing the prompt path premature. Eight
// tools carry an explicit path; bash does not, and in measured sessions bash is
// roughly five times the more common operation and does five times the file work.
//
// Run against a real directory, because the filter that does the work is a
// filesystem question - does this exist, or does it have a filename - and testing
// it against strings that cannot exist would be testing a different function.
func TestPathsAreExtractedFromACommand(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"main.go", "config.yaml", "old.txt", "new.txt", "Makefile",
		"src/a.go", "src/b.go", "out/log.txt", "bin/app", "api/build.gradle",
	} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		command string
		want    []string
	}{
		// The program, its subcommands, and its arguments are not targets. This is
		// the case containment alone got wrong: joined onto the root, `rm` and
		// `go` and `build` all resolve inside the workspace.
		{"rm main.go", []string{"main.go"}},
		{"sed -i s/a/b/ config.yaml", []string{"config.yaml"}},
		{"mv old.txt new.txt", []string{"new.txt", "old.txt"}},
		{"cat src/a.go && cat src/b.go", []string{"src/a.go", "src/b.go"}},
		// A redirect target arrives first in its segment, so it must not be
		// discarded as a program name.
		{"echo hi > out/log.txt", []string{"out/log.txt"}},
		{"cp bin/app api/build.gradle", []string{"api/build.gradle", "bin/app"}},
		// A globby build touches nothing this can name.
		{"go build ./...", nil},
		{"grep -rn needle src/", nil},
		// Outside the workspace, or escaping it, or not a path at all.
		{"cat /etc/passwd", nil},
		{"cat ../../etc/hosts", nil},
		{"curl -s https://example.com/a.json", nil},
		// An extensionless file that exists is still a target.
		{"make -f Makefile", []string{"Makefile"}},
	}
	for _, tc := range cases {
		got := pathsFromCommand(tc.command, root)
		want := make([]string, 0, len(tc.want))
		for _, w := range tc.want {
			want = append(want, filepath.Join(root, w))
		}
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q\n  got  %v\n  want %v", tc.command, got, want)
		}
	}
}

// The bound is what stops a build that names three hundred files from becoming a
// wall of annotations.
func TestExtractionIsBounded(t *testing.T) {
	root := t.TempDir()
	var names []string
	for i := 0; i < 40; i++ {
		name := filepath.Join(root, fmt.Sprintf("f%02d.go", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if got := pathsFromCommand("cat "+strings.Join(names, " "), root); len(got) != maxCommandTargets {
		t.Errorf("extraction returned %d targets, want the cap of %d", len(got), maxCommandTargets)
	}
}

// The same command must always produce the same list, or two identical operations
// deliver different annotations and the comparison stops meaning anything.
func TestExtractionIsDeterministic(t *testing.T) {
	command := "cat b.go a.go c.go"
	first := pathsFromCommand(command, "/srv")
	for i := 0; i < 5; i++ {
		got := pathsFromCommand(command, "/srv")
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("extraction reordered between runs: %v then %v", first, got)
			}
		}
	}
	if first[0] != "/srv/a.go" {
		t.Errorf("targets are not sorted: %v", first)
	}
}

// A command that rewrites three files surfaces what is known about all three, not
// only the first.
func TestAMultiFileCommandReachesEveryTarget(t *testing.T) {
	entries := []memory.Entry{
		{Content: "api needs JAVA_HOME", Seen: 8, SeenAt: time.Now(), Applies: "/srv/api/build.gradle"},
		{Content: "deploys run from ops", Seen: 8, SeenAt: time.Now(), Applies: "/srv/ops"},
		{Content: "web needs node 22", Seen: 8, SeenAt: time.Now(), Applies: "/srv/web/package.json"},
	}
	state := newSidekickState(func() ([]memory.Entry, error) { return entries, nil }, "/srv")

	sess := &sessionstore.Session{}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "shell",
		SpecArgs: map[string]any{"command": "python3 - <<'PY' && cp api/build.gradle api/bak && cat web/package.json"},
	})
	got := state.annotate(sess, operation.ResultEvent{OpID: "a"})
	for _, miss := range []string{"JAVA_HOME", "node 22"} {
		if !strings.Contains(got, miss) {
			t.Errorf("a shell rewrite of api/build.gradle and web/package.json missed %q: %q", miss, got)
		}
	}
	if strings.Contains(got, "deploys run from ops") {
		t.Errorf("surfaced a memory about a path the command never touched: %q", got)
	}
}

// The prompt must not steer the model toward the one tool the trigger cannot see.
// The wording that made bash the default for editing is the reason this was
// worth changing, and a test is the only thing that keeps it changed.
func TestBashIsNotTheDefaultForEditing(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "prompt", "system.txt"))
	if err != nil {
		t.Skipf("prompt not readable from this package: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "bash is the default for running, inspecting and editing") {
		t.Error("the prompt still makes bash the default for editing, which is the one tool the path trigger cannot see")
	}
	if !strings.Contains(text, "never with shell redirection or sed") {
		t.Error("the prompt does not forbid the model editing with shell redirection or sed")
	}
}

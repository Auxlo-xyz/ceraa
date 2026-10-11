package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// Memory.md is the append-only log that replaced the ranked store as the
// sidekick's source.
//
// The store re-rendered and re-ranked itself against a wall clock on every model
// request, and that volatility sat ahead of the cache seam: the bytes changed
// when nothing had been written, so the cached prefix was rebuilt on every turn.
// A log cannot do that. Nothing re-ranks, nothing decays, and only the tail is
// read, so the cost per turn is proportional to what is delivered rather than to
// everything ever learned.
//
// These tests pin the delivery side of that swap: the entry reaches the model on
// the tool result it is about, the session file still records the bare result,
// the delivered set still dedups, and an unreadable or empty log is a no-op
// rather than a failed turn.

// appendLog writes entries to the log in order, oldest first.
func appendLog(t *testing.T, file *memory.MemoryFile, entries ...string) {
	t.Helper()
	for i, entry := range entries {
		if err := file.Append(entry, fmt.Sprintf("sess-%d", i)); err != nil {
			t.Fatalf("append %q to %s: %v", entry, file.Path(), err)
		}
	}
}

// logMemoryFile is the log at path plus the reader production hands the runner.
func logMemoryFile(path string) func() *memory.MemoryFile {
	return func() *memory.MemoryFile { return memory.OpenMemoryFile(path) }
}

// logSidekick is a sidekick over a real log on disk, which is the shape
// production has: the file is the source and the reader opens it on every read.
func logSidekick(t *testing.T, tail int, entries ...string) (*sidekickState, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path), entries...)
	state := newSidekickState(nil, nil, "/srv")
	state.useMemoryFile(logMemoryFile(path), tail)
	return state, path
}

// readOn is the operation that reads a file under /srv/api, the path every
// fixture below is scoped to.
func readOn(path string) (*sessionstore.Session, operation.ResultEvent) {
	sess := sessionWithOp("a", "read", path)
	return sess, operation.ResultEvent{OpID: "a"}
}

// The seam, at the wire. Every unit test below calls annotate directly, which is
// the shape that passes while the model never sees the note: annotate is one term
// in a sum at the runner's result loop, and the sum can lose it. This drives a
// whole run and reads the tool-result body off the request.
func TestAMemoryFileEntryReachesTheModelOnTheToolResultItTouches(t *testing.T) {
	toolResults, _, runner := bondSeamRun(t, "memfile-seam", func(cfg *RunnerConfig) {
		// The log replaces the store, and a caller mid-migration configures both.
		cfg.MemoryEntries = nil
		file := memory.OpenMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
		appendLog(t, file,
			"GENERIC: an older standing note",
			"MEMFILE_SEAM: "+filepath.Join(cfg.WorkDir, "notes.txt")+" is generated, never hand-edit it",
		)
		cfg.MemoryFile = logMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
	})

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	carrying := 0
	for _, text := range toolResults {
		if !strings.Contains(text, "MEMFILE_SEAM") {
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
	if got := runner.systemPrompt(); strings.Contains(got, "MEMFILE_SEAM") {
		t.Error("the log entry is in the system prompt, so the cache prefix is volatile again")
	}
	if report := runner.DeliveryReport(); report.Mode != string(memory.DeliverySidekick) || report.TargetedInjects == 0 {
		t.Errorf("the run recorded no log delivery: %+v", report)
	}
}

// The annotated/persisted split, on the new source. The annotated text is what
// the model reads; the session file's operation record is what a human reads
// later, and it has to stay the bare result - a record that depends on which
// memories happened to be in the log at the time is not a record of anything.
func TestTheSessionOperationKeepsTheBareResultWhileTheModelSeesTheLogEntry(t *testing.T) {
	toolResults, sess, runner := bondSeamRun(t, "memfile-split", func(cfg *RunnerConfig) {
		cfg.MemoryEntries = nil
		file := memory.OpenMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
		appendLog(t, file,
			"GENERIC: an older standing note",
			"MEMFILE_SPLIT: "+filepath.Join(cfg.WorkDir, "notes.txt")+" is generated, never hand-edit it",
		)
		cfg.MemoryFile = logMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
	})

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	recorded := 0
	for _, op := range sess.Operations {
		if op.Output == "" {
			continue
		}
		recorded++
		if strings.Contains(op.Output, "MEMFILE_SPLIT") {
			t.Errorf("the session file's operation record carries the log entry: %q", op.Output)
		}
		if !strings.Contains(op.Output, "the file the agent read") {
			t.Errorf("the operation record lost the tool output: %q", op.Output)
		}
	}
	if recorded == 0 {
		t.Fatal("no operation was recorded, so the split was never exercised")
	}
	annotated := 0
	for _, msg := range sess.Messages {
		if msg.Role == "tool" && strings.Contains(msg.ToolResult, "MEMFILE_SPLIT") {
			annotated++
		}
	}
	if annotated != 1 {
		t.Errorf("the transcript carries the log entry %d times, want exactly the annotated copy", annotated)
	}
	// And the delivered set has to be persisted under an id the next run can
	// adopt, or the same entry is annotated again on the next Telegram message.
	if len(sess.MemoryDelivered) != 1 || !strings.HasPrefix(sess.MemoryDelivered[0], "memfile:") {
		t.Errorf("the delivered id was not persisted for the next run: %v", sess.MemoryDelivered)
	}
	if got := runner.DeliveryReport(); got.TargetedInjects == 0 {
		t.Errorf("the delivery report says the log delivered nothing: %+v", got)
	}
}

// An entry reaches the operation that touches the path it names, and only that
// one. A log entry is prose, so the scope it carries is the path it mentions, and
// a memory about the backend is not a fact about the web root.
func TestALogEntryReachesOnlyThePathItNames(t *testing.T) {
	state, _ := logSidekick(t, 0,
		"GENERIC: an older standing note",
		"MEMFILE_SCOPE: /srv/api pins jdk 21",
	)

	sess, result := readOn("/srv/api/main.go")
	if got := state.annotate(sess, result); !strings.Contains(got, "MEMFILE_SCOPE") {
		t.Errorf("the matching operation was not annotated: %q", got)
	}
	other := sessionWithOp("b", "read", "/srv/web/index.js")
	if got := state.annotate(other, operation.ResultEvent{OpID: "b"}); strings.Contains(got, "MEMFILE_SCOPE") {
		t.Errorf("an unrelated operation was annotated: %q", got)
	}
	// A bash command that names no file is not annotated with a scoped entry: the
	// entry says something about one path, and "no path" is not that path.
	shell := sessionWithOp("c", "shell", "")
	if got := state.annotate(shell, operation.ResultEvent{OpID: "c"}); strings.Contains(got, "MEMFILE_SCOPE") {
		t.Errorf("an operation with no path was annotated: %q", got)
	}
}

// The log's own order is the delivery order: oldest first, because nothing
// re-ranks. A ranking is what needed a clock, and a clock is what made the block
// volatile.
func TestLogEntriesAreDeliveredOldestFirstAndASecondOperationDoesNotRepeatThem(t *testing.T) {
	state, _ := logSidekick(t, 0,
		"MD_ONE: /srv/api is the backend",
		"MD_TWO: /srv/api pins jdk 21",
		"MD_THREE: /srv/api builds with gradle",
	)

	sess, result := readOn("/srv/api/main.go")
	first := state.annotate(sess, result)
	two, three := strings.Index(first, "MD_TWO"), strings.Index(first, "MD_THREE")
	if two < 0 || three < 0 {
		t.Fatalf("the first operation delivered no log entries: %q", first)
	}
	if two > three {
		t.Errorf("the newest entry was rendered before an older one: %q", first)
	}

	// The half that shipped. A second operation touching the same path used to be
	// annotated with the same text again, and that copy was written into the tool
	// result and re-sent on every request for the rest of the session.
	again := state.annotate(sessionWithOp("b", "read", "/srv/api/main.go"), operation.ResultEvent{OpID: "b"})
	if strings.Contains(again, "MD_TWO") || strings.Contains(again, "MD_THREE") {
		t.Errorf("a second matching operation repeated the entries: %q", again)
	}
	if report := state.deliveryReport(); report.Deduped == 0 {
		t.Errorf("the deduped delivery was not recorded: %+v", report)
	}
}

// A standing entry has no path, so it matches every operation and would be
// written into the transcript once per call. Once per run is the affordable
// answer, and once per session would fail in the other direction: the baseline
// has to survive exactly when a compacted transcript has dropped it.
func TestStandingLogEntriesFireOncePerRunAndRearmOnTheNext(t *testing.T) {
	state, _ := logSidekick(t, 0,
		"GENERIC: an older standing note",
		"STANDING_MD: the user prefers dark mode",
	)

	sess, result := readOn("/srv/web/index.js")
	first := state.annotate(sess, result)
	if !strings.Contains(first, "STANDING_MD") {
		t.Fatalf("the first call delivered nothing: %q", first)
	}
	second := state.annotate(sessionWithOp("b", "read", "/srv/web/other.js"), operation.ResultEvent{OpID: "b"})
	if strings.Contains(second, "STANDING_MD") {
		t.Errorf("the standing entry repeated on a later call: %q", second)
	}
	// A new runner over the same log, which is what the next Telegram message
	// builds: the standing set re-arms with it.
	next := newSidekickState(nil, nil, "/srv")
	next.useMemoryFile(logMemoryFile(pathOf(t, state)), 0)
	if got := next.annotate(sessionWithOp("c", "read", "/srv/web/index.js"), operation.ResultEvent{OpID: "c"}); !strings.Contains(got, "STANDING_MD") {
		t.Errorf("the standing entry was not re-armed on the next run: %q", got)
	}
}

// pathOf is the log the state reads, for a test that wants a second reader over
// the same file.
func pathOf(t *testing.T, state *sidekickState) string {
	t.Helper()
	if state == nil || state.memFile == nil {
		t.Fatal("no memory file to read")
	}
	return state.memFile().Path()
}

// The tail is bounded, and the bound is the point: a log that has grown for a
// month costs what is delivered, not what it holds. Reading it whole would put
// every entry ever learned in front of the selector on every turn, which is the
// store's cost with a different filename.
func TestTheTailBoundHoldsSoALongLogDoesNotFloodOneNote(t *testing.T) {
	var entries []string
	for i := 1; i <= 6; i++ {
		entries = append(entries, fmt.Sprintf("MD_CT%02d: /srv/api carries entry %02d", i, i))
	}
	state, _ := logSidekick(t, 2, entries...)

	sess, result := readOn("/srv/api/main.go")
	got := state.annotate(sess, result)
	for _, stale := range []string{"MD_CT01", "MD_CT02", "MD_CT03", "MD_CT04"} {
		if strings.Contains(got, stale) {
			t.Errorf("a tail of two delivered %q: %s", stale, got)
		}
	}
	if !strings.Contains(got, "MD_CT05") || !strings.Contains(got, "MD_CT06") {
		t.Errorf("the newest two entries were not delivered: %q", got)
	}
	if five, six := strings.Index(got, "MD_CT05"), strings.Index(got, "MD_CT06"); five > six {
		t.Errorf("the tail was delivered newest first: %q", got)
	}
}

// The default is what a run that delivers on every call can render: the general
// plus targeted limits of one note, times the number of notes a run may write.
// A caller that raises it is choosing a larger budget, not a different shape.
func TestTheDefaultTailIsTheRunBudgetInEntries(t *testing.T) {
	if defaultMemoryTail != (memory.SidekickGeneralLimit+memory.SidekickTargetLimit)*(memory.SidekickRunBytes/memory.SidekickBlockBytes) {
		t.Fatalf("defaultMemoryTail = %d, want the derived value", defaultMemoryTail)
	}
	state, _ := logSidekick(t, 0, "MD_DEFAULT: /srv/api is the backend", "a second entry")
	if state.tail != defaultMemoryTail {
		t.Errorf("an unset MemoryTail was normalised to %d, want %d", state.tail, defaultMemoryTail)
	}
}

// An unreadable log is a no-op, not a failed turn and not a panic. The file is an
// optimisation and the conversation is the source of truth, so a run that cannot
// read it delivers nothing.
func TestAnEmptyOrMissingMemoryFileDeliversNothing(t *testing.T) {
	states := map[string]*sidekickState{}
	// No file at all: a project with no memory yet is a state, not an error.
	missing := newSidekickState(nil, nil, "/srv")
	missing.useMemoryFile(logMemoryFile(filepath.Join(t.TempDir(), "absent", "Memory.md")), 0)
	states["missing"] = missing
	// A file that exists and is empty.
	empty := filepath.Join(t.TempDir(), "Memory.md")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	blank := newSidekickState(nil, nil, "/srv")
	blank.useMemoryFile(logMemoryFile(empty), 0)
	states["empty"] = blank
	// A reader that hands back nothing at all.
	none := newSidekickState(nil, nil, "/srv")
	none.useMemoryFile(func() *memory.MemoryFile { return nil }, 0)
	states["nil reader"] = none

	for name, state := range states {
		sess, result := readOn("/srv/api/main.go")
		if got := state.annotate(sess, result); got != "" {
			t.Errorf("%s: annotate returned %q", name, got)
		}
		if report := state.deliveryReport(); report != (memory.DeliveryReport{}) {
			t.Errorf("%s: the run recorded delivery it never made: %+v", name, report)
		}
	}
}

// The three delivered-set entry points, on the new source. They are the seams a
// compaction epoch and a per-message coordinator use, and the ids they carry are
// the log's own rather than a store's - which is the part that could silently
// have stopped working when the source changed.
func TestSeedForgetAndMarkStillBehaveWithTheLog(t *testing.T) {
	const body = "MD_EPOCH: /srv/api pins jdk 21"
	id := parseLogEntry(body).id
	if id == "" || !strings.HasPrefix(id, "memfile:") {
		t.Fatalf("the log entry has no identity to remember it by: %q", id)
	}

	// Mark: a caller carrying a set across an epoch boundary suppresses without
	// injecting.
	marked, _ := logSidekick(t, 0, "an older standing note", body)
	runner := &Runner{cfg: RunnerConfig{SessionID: "log-epoch"}, reg: tool.DefaultRegistry()}
	runner.sidekick = marked
	sess, result := readOn("/srv/api/main.go")
	runner.MarkDeliveredMemory(sess, id)
	if got := runner.sidekick.annotate(sess, result); strings.Contains(got, "MD_EPOCH") {
		t.Errorf("a memory marked as already delivered was injected again: %q", got)
	}
	if len(sess.MemoryDelivered) != 1 || sess.MemoryDelivered[0] != id {
		t.Errorf("the marked id was not carried onto the session: %v", sess.MemoryDelivered)
	}

	// Seed: a runner built for the next message starts from the session's own
	// record rather than from empty, or it re-fires everything the transcript
	// already carries.
	seeded, _ := logSidekick(t, 0, "an older standing note", body)
	runner.sidekick = seeded
	seededSession, seededResult := readOn("/srv/api/main.go")
	seededSession.MemoryDelivered = []string{id}
	runner.SeedDeliveredMemory(seededSession)
	if got := runner.sidekick.annotate(seededSession, seededResult); strings.Contains(got, "MD_EPOCH") {
		t.Errorf("the seed did not suppress an id the transcript already carries: %q", got)
	}

	// Forget: the compaction drops the tool result that carried the note, so
	// delivering it once more is not a repeat.
	delivered, _ := logSidekick(t, 0, "an older standing note", body)
	runner.sidekick = delivered
	forgotten, forgottenResult := readOn("/srv/api/main.go")
	if got := runner.sidekick.annotate(forgotten, forgottenResult); !strings.Contains(got, "MD_EPOCH") {
		t.Fatalf("the first epoch delivered nothing: %q", got)
	}
	runner.ForgetDeliveredMemory(forgotten)
	if len(forgotten.MemoryDelivered) != 0 {
		t.Errorf("the persisted half survived the epoch reset: %v", forgotten.MemoryDelivered)
	}
	if got := runner.sidekick.annotate(forgotten, forgottenResult); !strings.Contains(got, "MD_EPOCH") {
		t.Errorf("the entry did not come back after the transcript dropped it: %q", got)
	}
}

// The run ceiling is a byte ceiling and the tail is a count, so they are separate
// bounds and the ceiling still has to hold: past it an operation gets nothing
// rather than a fraction of a note, and what was withheld is recorded so a silent
// stop is not mistaken for a log with nothing to say.
func TestTheRunCeilingStillStopsLogDeliveryRatherThanTrimmingIt(t *testing.T) {
	var entries []string
	for i := 0; i < 40; i++ {
		entries = append(entries, fmt.Sprintf("MD_CEIL%02d: /srv/api carries %s", i, strings.Repeat("x", 90)))
	}
	state, _ := logSidekick(t, 64, entries...)

	delivered, spent := 0, 0
	for i := 0; i < 40; i++ {
		path := fmt.Sprintf("/srv/api/file%02d.go", i)
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
	if delivered == 40 {
		t.Error("the run ceiling never engaged, so the test is not exercising it")
	}
	if report := state.deliveryReport(); report.BudgetWithheld == 0 {
		t.Errorf("the withheld entries were not recorded: %+v", report)
	}
}

// The other two modes are untouched by any of this: the log is a sidekick source
// and a runner in prompt or cached mode must not build one from it.
func TestTheLogDoesNotReachAPromptOrCachedRun(t *testing.T) {
	for _, mode := range []memory.Delivery{memory.DeliveryPrompt, memory.DeliveryCached} {
		runner := New(RunnerConfig{
			SessionID:      "mode",
			MemoryContext:  "<memory>block</memory>",
			MemoryDelivery: string(mode),
			MemoryFile:     logMemoryFile(filepath.Join(t.TempDir(), "Memory.md")),
		}, nil, tool.NewRegistry())
		if runner.sidekick != nil {
			t.Errorf("mode %q built sidekick state from a log, so a tool result can be annotated in a mode that has no notes", mode)
		}
		if got := runner.memoryContext(); (mode == memory.DeliveryPrompt) != (got != "") {
			t.Errorf("mode %q put %q in the varying tail", mode, got)
		}
	}
}

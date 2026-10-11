package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/memory"
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
// These tests pin the reader side of that swap: the tail reads oldest first,
// the bound holds, an unreadable or empty log reads nothing, the seam run
// carries the entry exactly once on the newest user message, and the session
// file still records bare results.

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
	state := newSidekickState("/srv")
	state.useMemoryFile(logMemoryFile(path), tail)
	return state, path
}

// The seam, at the wire. The tail line rides the newest user message, so the
// wire tests read user bodies off the request: a unit test on the reader is
// the shape that passes while the model never sees the line.
func TestAMemoryFileEntryReachesTheModelOnceViaTheTailLine(t *testing.T) {
	toolResults, userTexts, _, runner := memoryLogSeamRun(t, "memfile-seam", func(cfg *RunnerConfig) {
		// The log replaces the store, and a caller mid-migration configures both.
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
	// Tool results carry no memory: the tail line is the single delivery.
	for _, text := range toolResults {
		if strings.Contains(text, "MEMFILE_SEAM") || strings.Contains(text, "<memory>") {
			t.Errorf("a tool result carries memory text: %q", text)
		}
	}
	carrying := 0
	for _, text := range userTexts {
		if !strings.Contains(text, "MEMFILE_SEAM") {
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
	if got := runner.systemPrompt(); strings.Contains(got, "MEMFILE_SEAM") {
		t.Error("the log entry is in the system prompt, so the cache prefix is volatile again")
	}
}

// The stored split, on the new source. The session file's operation record is
// what a human reads later, and it has to stay the bare result - a record that
// depends on which memories happened to be in the log at the time is not a
// record of anything. The tail line lives on the request, never in storage.
func TestTheSessionOperationKeepsTheBareResultWhileTheTailCarriesTheEntry(t *testing.T) {
	_, _, sess, _ := memoryLogSeamRun(t, "memfile-split", func(cfg *RunnerConfig) {
		file := memory.OpenMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
		appendLog(t, file,
			"GENERIC: an older standing note",
			"MEMFILE_SPLIT: "+filepath.Join(cfg.WorkDir, "notes.txt")+" is generated, never hand-edit it",
		)
		cfg.MemoryFile = logMemoryFile(filepath.Join(cfg.WorkDir, "Memory.md"))
	})

	recorded := 0
	for _, op := range sess.Operations {
		if op.Output == "" {
			continue
		}
		recorded++
		if strings.Contains(op.Output, "MEMFILE_SPLIT") || strings.Contains(op.Output, "<memory>") {
			t.Errorf("the session file's operation record carries the log entry: %q", op.Output)
		}
		if !strings.Contains(op.Output, "the file the agent read") {
			t.Errorf("the operation record lost the tool output: %q", op.Output)
		}
	}
	if recorded == 0 {
		t.Fatal("no operation was recorded, so the split was never exercised")
	}
	for _, msg := range sess.Messages {
		if strings.Contains(msg.Content, "<memory>") || strings.Contains(msg.ToolResult, "<memory>") {
			t.Errorf("the stored transcript carries the tail line, which must live on the request only: %+v", msg)
		}
	}
}

// The tail reads oldest first and stops at the bound. The bound is the point:
// a log that has grown for a month costs what is delivered, not what it
// holds.
func TestTheTailReadsOldestFirstAndStopsAtTheBound(t *testing.T) {
	var entries []string
	for i := 1; i <= 6; i++ {
		entries = append(entries, fmt.Sprintf("MD_CT%02d: /srv/api carries entry %02d", i, i))
	}
	state, _ := logSidekick(t, 2, entries...)

	tail := state.memTail()
	if len(tail) != 2 {
		t.Fatalf("a tail of two read %d entries: %q", len(tail), tail)
	}
	if !strings.Contains(tail[0], "MD_CT05") || !strings.Contains(tail[1], "MD_CT06") {
		t.Errorf("the tail is not the newest two oldest-first: %q", tail)
	}
}

// The default is the derived run budget in entries. A caller that raises it is
// choosing a larger budget, not a different shape.
func TestTheDefaultTailIsTheRunBudgetInEntries(t *testing.T) {
	if defaultMemoryTail != (memory.SidekickGeneralLimit+memory.SidekickTargetLimit)*(memory.SidekickRunBytes/memory.SidekickBlockBytes) {
		t.Fatalf("defaultMemoryTail = %d, want the derived value", defaultMemoryTail)
	}
	state, _ := logSidekick(t, 0, "MD_DEFAULT: /srv/api is the backend", "a second entry")
	if state.tail != defaultMemoryTail {
		t.Errorf("an unset MemoryTail was normalised to %d, want %d", state.tail, defaultMemoryTail)
	}
}

// An unreadable log reads nothing, not a failed turn and not a panic. The file
// is an optimisation and the conversation is the source of truth.
func TestAnEmptyOrMissingMemoryFileReadsNothing(t *testing.T) {
	states := map[string]*sidekickState{}
	// No file at all: a project with no memory yet is a state, not an error.
	missing := newSidekickState("/srv")
	missing.useMemoryFile(logMemoryFile(filepath.Join(t.TempDir(), "absent", "Memory.md")), 0)
	states["missing"] = missing
	// A file that exists and is empty.
	empty := filepath.Join(t.TempDir(), "Memory.md")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	blank := newSidekickState("/srv")
	blank.useMemoryFile(logMemoryFile(empty), 0)
	states["empty"] = blank
	// A reader that hands back nothing at all.
	none := newSidekickState("/srv")
	none.useMemoryFile(func() *memory.MemoryFile { return nil }, 0)
	states["nil reader"] = none

	for name, state := range states {
		if tail := state.memTail(); tail != nil {
			t.Errorf("%s: memTail read %q", name, tail)
		}
	}
	// And the runner built over one renders no tail line.
	runner := New(RunnerConfig{
		SessionID:      "mode-empty",
		SystemPrompt:   "STATIC",
		WorkDir:        t.TempDir(),
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryFile:     logMemoryFile(filepath.Join(t.TempDir(), "absent", "Memory.md")),
	}, nil, tool.NewRegistry())
	if tail := runner.memoryTailLine(); tail != "" {
		t.Errorf("an unreadable log rendered %q", tail)
	}
}

// The other two modes attach nothing anywhere: no sidekick state is built, the
// system prompt stays clean, and the built messages carry no tail line.
func TestTheLogDoesNotReachAPromptOrCachedRun(t *testing.T) {
	for _, mode := range []memory.Delivery{memory.DeliveryPrompt, memory.DeliveryCached} {
		dir := t.TempDir()
		path := filepath.Join(dir, "Memory.md")
		appendLog(t, memory.OpenMemoryFile(path), "MODE_CHECK: /srv/api is the backend")
		runner := New(RunnerConfig{
			SessionID:      "mode",
			SystemPrompt:   "STATIC",
			WorkDir:        dir,
			MemoryDelivery: string(mode),
			MemoryFile:     logMemoryFile(path),
		}, nil, tool.NewRegistry())
		if runner.sidekick != nil {
			t.Errorf("mode %q built sidekick state from a log, so memory has a path to the model in a mode that delivers nothing", mode)
		}
		if got := runner.systemPrompt(); strings.Contains(got, "<memory>") {
			t.Errorf("mode %q carries a memory block in the system prompt: %q", mode, got)
		}
		sess := &sessionstore.Session{
			ID:       "mode",
			Messages: []sessionstore.Message{{Role: llm.RoleUser, Content: "hello"}},
		}
		for _, msg := range runner.messages(sess, nil) {
			if strings.Contains(msg.Text, "<memory>") || strings.Contains(msg.Text, "MODE_CHECK") {
				t.Errorf("mode %q attached memory to the request: %q", mode, msg.Text)
			}
		}
	}
}

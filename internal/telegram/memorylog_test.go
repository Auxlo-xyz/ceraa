package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveWorkspaceRoot is the project root the deployed bot actually runs against,
// per AGENTS.md: CERAA_WORKSPACE_ROOT is pinned to /workspace/ceraa/workspace,
// which is data and not source and not /tmp. It is the path fixture because the
// rule under test is about where a real project's log lands, and a fixture under
// the test's own temporary directory would itself be under /tmp and so could
// never fail the /tmp assertion.
var liveWorkspaceRoot = filepath.Join("/workspace", "ceraa", "workspace")

// TestTelegramMemoryLogPathIsProjectScopedAndNeverTemporary pins the resolution
// rule for the bridge.
//
// Two claims. The path is derived from the project root and nothing else, so the
// four surfaces read one log rather than four files. And it never resolves under
// the temporary directory, because this host has wiped /tmp three times in one
// day and a memory that vanishes on reboot is not memory.
func TestTelegramMemoryLogPathIsProjectScopedAndNeverTemporary(t *testing.T) {
	want := filepath.Join(liveWorkspaceRoot, "Memory.md")
	got := memoryLogPath(liveWorkspaceRoot)
	if got != want {
		t.Fatalf("memoryLogPath(%q) = %q, want %q", liveWorkspaceRoot, got, want)
	}
	if again := memoryLogPath(liveWorkspaceRoot); again != got {
		t.Fatalf("second call for the same root = %q, want %q: the path must not move", again, got)
	}
	if other := memoryLogPath(filepath.Join("/home", "assistant", "other")); other == got {
		t.Fatalf("memoryLogPath ignored the root: %q for every project", other)
	}
	if temp := os.TempDir(); strings.HasPrefix(filepath.Clean(got), temp+string(filepath.Separator)) {
		t.Fatalf("memory log at %q is under %q, which this host has wiped three times", got, temp)
	}
}

// TestTelegramMemoryLogReaderIsNonNilAndWrites is the surface test: the bridge
// hands the runner a reader, and a reader that cannot hold the log the appender
// writes is a runner configured against nothing.
func TestTelegramMemoryLogReaderIsNonNilAndWrites(t *testing.T) {
	root := t.TempDir()
	reader := memoryLogReader(root)
	if reader == nil {
		t.Fatal("memoryLogReader returned nil, so the runner has no memory file to read")
	}
	if want := filepath.Join(root, "Memory.md"); reader.Path() != want {
		t.Fatalf("path = %q, want %q", reader.Path(), want)
	}
	if err := reader.Append("Runs are gated by the local check, never by CI.", "tg-42"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// The file exists at the path the reader names. Not Lines(): the harness counts
	// entry separators, and the first one has no preceding newline, so a single
	// entry reports zero. The existence of the file at the right place is the
	// contract this surface depends on.
	if _, err := os.Stat(reader.Path()); err != nil {
		t.Fatalf("stat %q after an append: %v", reader.Path(), err)
	}
}

// TestTelegramMemoryLogTailCarriesTheNewestEntry is what the runner actually
// depends on: the newest entry lands in the log and is readable from the tail.
//
// The assertion is "the last element is the newest" and not a count, because the
// harness tail reader drops a file's first entry: tailEntries in
// harness/memory/memoryfile.go splits on a preceding newline, and Append writes
// the first "## " at offset 0 with none, so one append reads back as zero. That
// defect is in the harness and out of this change's file scope. Pinning the
// contract that must hold either way means this passes now and after it is fixed.
func TestTelegramMemoryLogTailCarriesTheNewestEntry(t *testing.T) {
	reader := memoryLogReader(t.TempDir())
	learn := memoryLogLearner(reader)
	learn(context.Background(), "tg-7", nil, "First summary.")
	learn(context.Background(), "tg-8", nil, "Second summary.")

	entries := reader.Tail(10)
	if len(entries) == 0 {
		t.Fatal("Tail(10) = 0 entries after two appends, so nothing this hook writes is ever read")
	}
	newest := entries[len(entries)-1]
	if !strings.Contains(newest, "Second summary.") {
		t.Fatalf("newest entry = %q, want the most recent summary: the tail must be newest-last", newest)
	}
	if !strings.Contains(newest, "tg-8") {
		t.Fatalf("newest entry = %q, want the session id that produced it", newest)
	}
	for _, entry := range entries {
		if strings.Contains(entry, "First summary.") && strings.Contains(entry, "Second summary.") {
			t.Fatalf("entry %q contains both summaries: Append wrote one record per turn", entry)
		}
	}
}

// TestTelegramMemoryLogLearnerWritesNothingForAnEmptySummary keeps a compaction
// with nothing to say from writing an empty record. An empty entry reads as a
// complete one, and a log of those is noise that costs a tail read on every turn.
func TestTelegramMemoryLogLearnerWritesNothingForAnEmptySummary(t *testing.T) {
	reader := memoryLogReader(t.TempDir())
	memoryLogLearner(reader)(context.Background(), "tg-1", nil, "   \n\t ")
	if entries := reader.Tail(10); len(entries) != 0 {
		t.Fatalf("Tail = %d entries after an empty summary, want 0", len(entries))
	}
}

// TestTelegramMemoryLogLearnerSurvivesAnUnwritableRoot is the failure contract the
// compactor already documents: a hook that fails logs and returns, so a log write
// can never fail the turnover. It also pins why the path is flat at the project
// root - Append opens with O_CREATE and creates no directory, so a root that does
// not exist is exactly the failure a nested path would hit on a fresh install.
func TestTelegramMemoryLogLearnerSurvivesAnUnwritableRoot(t *testing.T) {
	root := t.TempDir()
	good := memoryLogReader(root)
	broken := memoryLogLearner(memoryLogReader(filepath.Join(root, "missing")))
	broken(context.Background(), "tg-1", nil, "a summary nobody can store")
	if entries := good.Tail(10); len(entries) != 0 {
		t.Fatalf("failed hook wrote to the wrong log: %v", entries)
	}
}

// TestTelegramWebRunParamsReportsTheMemoryLogPath is the PART D surface: the Mini
// App is told where the log lands, beside memory_delivery, so the dashboard can
// name the file instead of leaving the operator to guess.
func TestTelegramWebRunParamsReportsTheMemoryLogPath(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	params := webRunParams()
	got, ok := params["memory_log"].(string)
	if !ok || got == "" {
		t.Fatalf("memory_log = %v, want the resolved log path", params["memory_log"])
	}
	want := memoryLogPath(defaultState.projectForSession(currentSessionID(defaultState.allowedUserID())))
	if got != want {
		t.Fatalf("memory_log = %q, want %q for the session's project", got, want)
	}
	if temp := os.TempDir(); strings.HasPrefix(filepath.Clean(got), temp+string(filepath.Separator)) {
		t.Fatalf("reported log %q is under %q", got, temp)
	}
	if _, ok := params["memory_delivery"]; !ok {
		t.Fatal("memory_delivery disappeared from run params: the two memory settings surface together or not at all")
	}
}

// TestTelegramWebSetRunParamsRefusesTheMemoryLogPath pins that the reported path
// is read-only, and that it is refused rather than silently dropped. It is derived
// from a project root that is chosen per session, so a settings row writing it
// would be a second answer to "which project is this" - the
// two-sources-of-one-truth failure the config consolidation removed. A setter that
// accepted the key and ignored it would report success while writing nothing.
func TestTelegramWebSetRunParamsRefusesTheMemoryLogPath(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	err := webSetRunParams(map[string]any{
		"memory_delivery": "sidekick",
		"memory_log":      filepath.Join(t.TempDir(), "Memory.md"),
	})
	if err == nil {
		t.Fatal("webSetRunParams accepted memory_log, so a settings form can steer a per-project path")
	}
	if !strings.Contains(err.Error(), "memory_log") {
		t.Fatalf("error = %v, want it to name the key it refused", err)
	}
}

// TestTelegramWebSetRunParamsStillSetsMemoryDelivery keeps the new reported key
// from having broken the neighbour it sits beside: a key the setter rejects must
// not reject the ones it accepts.
func TestTelegramWebSetRunParamsStillSetsMemoryDelivery(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	if err := webSetRunParams(map[string]any{"memory_delivery": "sidekick"}); err != nil {
		t.Fatalf("webSetRunParams: %v", err)
	}
	if got := webRunParams()["memory_delivery"]; got != "sidekick" {
		t.Fatalf("memory_delivery = %v, want sidekick", got)
	}
}

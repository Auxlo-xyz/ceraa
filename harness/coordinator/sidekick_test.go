package coordinator

import (
	"os"
	"strings"
	"testing"
	"time"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
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

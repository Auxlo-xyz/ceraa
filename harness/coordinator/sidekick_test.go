package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

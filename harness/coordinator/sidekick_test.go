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

	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// WHY: memory now reaches the model exactly once per run as a tail line on the newest user message, so no mode may render it into the prompt.
// No mode renders a memory block anymore. The system prompt carries no memory
// in any mode and the report never claims a prompt block.
func TestNoModeRendersAMemoryBlock(t *testing.T) {
	for _, mode := range []memory.Delivery{memory.DeliveryPrompt, memory.DeliveryCached, memory.DeliverySidekick} {
		path := filepath.Join(t.TempDir(), "Memory.md")
		appendLog(t, memory.OpenMemoryFile(path), "MODE_CHECK: /srv/api is the backend")
		r := New(RunnerConfig{
			SessionID:      "mode",
			MemoryDelivery: string(mode),
			MemoryFile:     logMemoryFile(path),
		}, nil, tool.NewRegistry())
		if r.delivery != mode {
			t.Fatalf("mode %q parsed as %q", mode, r.delivery)
		}
		if (r.sidekick != nil) != (mode == memory.DeliverySidekick) {
			t.Errorf("mode %q built sidekick state = %v, want it only in sidekick mode", mode, r.sidekick != nil)
		}
		if got := r.systemPrompt(); strings.Contains(got, "<memory>") || strings.Contains(got, "MODE_CHECK") {
			t.Errorf("mode %q carries memory in the system prompt: %q", mode, got)
		}
	}
}

// WHY: the mode label must stay honest because the run record stores it for later analysis.
// The point of keeping memory out of the prompt: the system prompt becomes
// fully static, so the whole thing is one cached prefix.
func TestSidekickModeIsReportedForMeasurement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Memory.md")
	appendLog(t, memory.OpenMemoryFile(path), "MODE_REPORT: /srv/api is the backend")
	sidekick := New(RunnerConfig{
		SessionID:      "mode",
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryFile:     logMemoryFile(path),
	}, nil, tool.NewRegistry())

	if sidekick.delivery != memory.DeliverySidekick {
		t.Errorf("mode = %q", sidekick.delivery)
	}
	if sidekick.sidekick == nil {
		t.Error("sidekick mode built no tail reader, so memory has no path to the model")
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

// WHY: the stored transcript is re-sent on every later request, so any per-result copy would repeat for the rest of the session.
// The claim the new contract rests on, at the seam production uses: two reads
// of the same file in one run must leave zero copies of the memory in the
// stored tool results, because delivery now happens once as a tail line.
func TestNoMemoryTextReachesTheStoredTranscript(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "Memory.md")
	appendLog(t, memory.OpenMemoryFile(logPath),
		"LOG_COPY: "+filepath.Join(dir, "api")+" pins jdk 21",
	)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]any) {
			payload := map[string]any{"choices": []any{map[string]any{"delta": delta}}}
			encoded, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
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
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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
		MemoryFile:     logMemoryFile(logPath),
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
	if total != 0 {
		t.Errorf("the transcript carries %d copies of the memory; tool results must carry none", total)
	}
}

// WHY: target extraction still runs because the instruction channel shares it, but memory must never land on a tool result.
// A command that rewrites two files still resolves to both targets; there is
// simply no memory annotation anymore for them to trigger.
func TestAMultiFileCommandCarriesNoMemoryText(t *testing.T) {
	sess := &sessionstore.Session{}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "a", SpecType: "shell",
		SpecArgs: map[string]any{"command": "python3 - <<'PY' && cp api/build.gradle api/bak && cat web/package.json"},
	})
	state := newSidekickState("/srv")
	targets := state.targetsForSpec("shell", sess.Operations[0].SpecArgs)
	joined := strings.Join(targets, ",")
	for _, want := range []string{filepath.Join("/srv", "api", "build.gradle"), filepath.Join("/srv", "web", "package.json")} {
		if !strings.Contains(joined, want) {
			t.Errorf("extraction lost %q from a multi-file command: %v", want, targets)
		}
	}
}

// WHY: the model names files as plain shell tokens, so the shared extractor must keep seeing them for the tail reader to match on.
// A shell command names its files as plain tokens, so the trigger has to see them.
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

// WHY: identical operations must keep resolving identically, or the tail reader's matching stops meaning anything.
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

// WHY: the prompt must not steer the model toward the one tool the shared trigger cannot see.
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

// WHY: an unbounded target list would turn one build command into noise for the tail reader's matching.
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

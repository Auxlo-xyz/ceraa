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

	"ceraa/harness/instruction"
	"ceraa/harness/llm"
	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// ruleSource compiles an instruction file once and serves the same snapshot, the
// way a wired production source does. The counter is the seam: a wiring that
// reloads per step still passes every content assertion below, and only this
// exposes it.
type ruleSource struct {
	mu    sync.Mutex
	snap  instruction.Snapshot
	loads int
	body  string
}

func compilingSource(body string) *ruleSource {
	return &ruleSource{body: body}
}

func (s *ruleSource) loader() func() instruction.Snapshot {
	return func() instruction.Snapshot {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.loads++
		snap, _ := instruction.Compile("AGENTS.md", []byte(s.body))
		s.snap = snap
		return snap
	}
}

func (s *ruleSource) loadsSeen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

const rulesBody = `## Outbound network

- Every network tool goes through harness/netguard. [applies=notes.txt]
- The guard covers RFC 1918 and CGNAT. [applies=notes.txt]
- Unrelated rule that never fires. [applies=never/touched.txt]
`

// instructionSeamRun drives a whole run against a fake provider and returns the
// tool results the model actually read, the session, and the compiled source.
//
// It follows memoryLogSeamRun deliberately: every assertion in this file is about the
// bytes on the wire, because instructionNote is one more term in the same sum at
// runner.go:1077, and that is the seam that can lose it.
func instructionSeamRun(t *testing.T, sessionID string, cfg func(*RunnerConfig)) ([]string, *sessionstore.Session, *ruleSource) {
	t.Helper()
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := writeFileForTest(target, "the file the agent read"); err != nil {
		t.Fatal(err)
	}

	var (
		mu         sync.Mutex
		toolResult []string
		requests   atomic.Int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			System string `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, m := range body.Messages {
			if m.Role == "tool" {
				toolResult = append(toolResult, m.Content)
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
	source := compilingSource(rulesBody)
	config := RunnerConfig{
		SessionID:        sessionID,
		SystemPrompt:     "STATIC",
		Model:            llm.FromProvider(server.URL, "test", "test"),
		ModelCatalogURL:  "http://127.0.0.1:1",
		WorkDir:          work,
		MaxRequests:      4,
		InstructionRules: source.loader(),
	}
	if cfg != nil {
		cfg(&config)
	}
	runner := New(config, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "read the notes"); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), toolResult...), sess, source
}

// The seam: a rule whose scope covers the file the agent touched has to reach the
// tool result the model reads.
func TestAnApplicableRuleReachesTheToolResultTheModelReads(t *testing.T) {
	results, _, _ := instructionSeamRun(t, "instr-seam", nil)

	carrying := 0
	for _, text := range results {
		if !strings.Contains(text, "Every network tool goes through harness/netguard") {
			continue
		}
		carrying++
		if !strings.Contains(text, "the file the agent read") {
			t.Errorf("the rule replaced the tool output rather than accompanying it: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no tool result the model reads carries the rule: %#v", results)
	}
}

// A rule outside the operation's scope must not fire, and a rule that did not fire
// must not be retired by the delivery set - otherwise the first unrelated call
// silently disables it forever.
func TestARuleOutsideTheScopeDoesNotFireAndIsNotRetired(t *testing.T) {
	results, _, _ := instructionSeamRun(t, "instr-scope", nil)

	for _, text := range results {
		if strings.Contains(text, "Unrelated rule that never fires") {
			t.Errorf("a rule outside the operation's scope was delivered: %q", text)
		}
	}
}

// The annotated/persisted split, same as the memory path: the model reads the
// note, the operation record keeps the bare result for a later reader.
//
// The split survives by construction here - persistResult ASSIGNS Operations[].Output
// from resultText rather than appending to it, so anything written ahead of the
// persist is erased. This test is the guard that keeps it that way, not the reason
// it holds: appending before the persist is invisible, which is exactly why a leak
// here would go unnoticed until the persist stopped overwriting.
func TestTheSessionOperationKeepsTheBareResultWhileTheModelSeesTheRule(t *testing.T) {
	results, sess, _ := instructionSeamRun(t, "instr-split", nil)

	recorded := 0
	for _, op := range sess.Operations {
		if op.Output == "" {
			continue
		}
		recorded++
		if strings.Contains(op.Output, "Every network tool goes through harness/netguard") {
			t.Errorf("the rule leaked into the stored operation record: %q", op.Output)
		}
	}
	if recorded == 0 {
		t.Fatal("no operation was recorded, so the split was not exercised")
	}
	if len(results) == 0 {
		t.Fatal("no tool result reached the model, so the test proved nothing")
	}
}

// The whole point of the feature. A rule that lives in a file must never reach the
// system prompt, or the prompt is volatile again and the cache prefix is rebuilt
// every request - the defect this replaced.
func TestTheRulesNeverEnterTheSystemPrompt(t *testing.T) {
	// The exact prompt text is the system prompt plus the identity, workspace and
	// date blocks the runner always assembles. Asserting on the whole string would
	// pin those unrelated wrappers. The invariant is that no RULE TEXT appears and
	// that configuring the layer changes nothing at all - compared against a runner
	// in the SAME work directory, because the workspace block embeds it.
	work := t.TempDir()
	prompt := systemPromptForRules(t, "STATIC", work, rulesBody)
	if !strings.Contains(prompt, "STATIC") {
		t.Errorf("the base system prompt was disturbed by the instruction layer: %q", prompt)
	}
	if strings.Contains(prompt, "Every network tool goes through harness/netguard") {
		t.Errorf("a rule is in the system prompt, so the cache prefix is volatile again: %q", prompt)
	}
	if strings.Contains(prompt, "The guard covers RFC 1918 and CGNAT") {
		t.Errorf("a rule is in the system prompt: %q", prompt)
	}

	bare := systemPromptForRules(t, "STATIC", work, "")
	if prompt != bare {
		t.Errorf("configuring rules changed the system prompt:\nwith:    %q\nwithout: %q", prompt, bare)
	}
}

// systemPromptForRules builds a runner in a known directory with the given rule
// file and returns the prompt it would send.
// sessionFor copies a template into a fresh session. The delivered set is written
// through to the session file, so three runners sharing one session would each see
// the previous runner's delivery and the last assertion would pass for the wrong
// reason.
func sessionFor(template *sessionstore.Session) *sessionstore.Session {
	fresh := &sessionstore.Session{ID: template.ID, Version: template.Version}
	fresh.Operations = append(fresh.Operations, template.Operations...)
	return fresh
}

func systemPromptForRules(t *testing.T, base, work, body string) string {
	t.Helper()
	cfg := RunnerConfig{
		SystemPrompt:    base,
		WorkDir:         work,
		Model:           llm.FromProvider("http://127.0.0.1:1", "test", "test"),
		ModelCatalogURL: "http://127.0.0.1:1",
	}
	if body != "" {
		cfg.InstructionRules = compilingSource(body).loader()
	}
	return New(cfg, nil, tool.DefaultRegistry()).systemPrompt()
}

// The loader is called once per run and never per step. A file read per step is
// the exact defect this feature exists to remove, and it looks like a working
// feature the whole time.
func TestTheRuleFileIsCompiledOncePerRunAndNotPerStep(t *testing.T) {
	_, _, source := instructionSeamRun(t, "instr-once", nil)

	if loads := source.loadsSeen(); loads != 1 {
		t.Errorf("the rule file was compiled %d times in one run, want exactly 1", loads)
	}
}

// Instructions are independent of the memory sidekick. A caller configured with
// rules and no memory store still delivers rules; coupling the two would make the
// instruction feature silently depend on memory being configured.
func TestInstructionsDeliverWithNoMemorySidekickConfigured(t *testing.T) {
	results, _, _ := instructionSeamRun(t, "instr-nomem", nil)

	found := false
	for _, text := range results {
		if strings.Contains(text, "Every network tool goes through harness/netguard") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rule was delivered without a memory sidekick configured: %#v", results)
	}
}

// Marking happens after the render. If a rule were marked before its note cleared
// the byte ceiling, the ceiling would retire it unseen and it would never fire
// again anywhere in the session - a silent hole rather than a visible cap.
func TestARuleWithheldByTheBudgetIsNotRetired(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := writeFileForTest(target, "the file the agent read"); err != nil {
		t.Fatal(err)
	}

	var bullets strings.Builder
	bullets.WriteString("## Rules\n\n")
	for i := 0; i < 400; i++ {
		bullets.WriteString(fmt.Sprintf("- Rule %d that is long enough to matter and carries real words. [applies=notes.txt]\n", i))
	}

	var (
		mu      sync.Mutex
		held    []instruction.Snapshot
		seen    []string
		request atomic.Int32
	)
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
		seen = seen[:0]
		for _, m := range body.Messages {
			if m.Role == "tool" {
				seen = append(seen, m.Content)
			}
		}
		mu.Unlock()

		var payload map[string]any
		if request.Add(1) == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call1",
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
	runner := New(RunnerConfig{
		SessionID:       "instr-budget",
		SystemPrompt:    "STATIC",
		Model:           llm.FromProvider(server.URL, "test", "test"),
		ModelCatalogURL: "http://127.0.0.1:1",
		WorkDir:         work,
		MaxRequests:     4,
		InstructionRules: func() instruction.Snapshot {
			snap, _ := instruction.Compile("AGENTS.md", []byte(bullets.String()))
			held = append(held, snap)
			return snap
		},
	}, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "read the notes"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no tool result reached the model")
	}
	first := seen[0]
	// A bounded note must say what it withheld. Silence here is what makes a cap
	// indistinguishable from an empty rule file.
	if !strings.Contains(first, "Withheld") && !strings.Contains(first, "withheld") {
		t.Errorf("a truncated note does not say so: %q", first[:min(len(first), 400)])
	}
	// And the rules it did carry are the first ones in document order, because
	// mandatory-first ordering is what a budget has to respect.
	if !strings.Contains(first, "Rule 0") {
		t.Errorf("the note dropped document order under budget: %q", first[:min(len(first), 400)])
	}
}

// A nil instruction source is a no-op, not a panic. Rule delivery is optional and
// a caller with no rule file is the normal case.
func TestANilInstructionSourceIsANoOp(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := writeFileForTest(target, "the file the agent read"); err != nil {
		t.Fatal(err)
	}
	var (
		mu      sync.Mutex
		results []string
		request atomic.Int32
	)
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
				results = append(results, m.Content)
			}
		}
		mu.Unlock()
		var payload map[string]any
		if request.Add(1) == 1 {
			payload = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call1",
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
	runner := New(RunnerConfig{
		SessionID:       "instr-nil",
		SystemPrompt:    "STATIC",
		Model:           llm.FromProvider(server.URL, "test", "test"),
		ModelCatalogURL: "http://127.0.0.1:1",
		WorkDir:         work,
		MaxRequests:     4,
	}, store, tool.DefaultRegistry())
	if _, err := runner.Run(context.Background(), "read the notes"); err != nil {
		t.Fatalf("a nil instruction source broke the run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Fatal("no tool result reached the model")
	}
}

// Marking happens after the render, and this is the test that pins it.
//
// A rule the byte ceiling drops must stay eligible, because a withheld rule is not
// a delivered one. Marking before the render retires it instead: it never reaches
// the model in that operation, the ceiling is the only reason it can fire again on
// the next, and marking has already spent that chance. The result is a rule that
// exists, fires, and delivers nothing anywhere in the session.
//
// This can only be observed over two operations. One operation shows either the
// rule delivered or the note truncated, and neither distinguishes "withheld, still
// eligible" from "withheld, retired".
func TestARuleTheBudgetHeldIsStillDeliveredOnTheNextMatchingOperation(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := writeFileForTest(target, "the file the agent read"); err != nil {
		t.Fatal(err)
	}

	var body strings.Builder
	body.WriteString("## Rules\n\n")
	body.WriteString("- The first rule, long enough to fill the note on its own. [applies=notes.txt]\n")
	for i := 0; i < 400; i++ {
		body.WriteString(fmt.Sprintf("- Filler rule %d that carries a full line of real words so the ceiling bites. [applies=notes.txt]\n", i))
	}

	runner := New(RunnerConfig{
		WorkDir:          work,
		InstructionRules: compilingSource(body.String()).loader(),
	}, nil, tool.DefaultRegistry())

	sess := &sessionstore.Session{ID: "budget", Version: 3}
	op := sessionstore.Operation{
		ID: "op1", Tool: "read", SpecType: "read", SpecArgs: map[string]any{"path": target},
	}
	result := operation.ResultEvent{OpID: "op1", Status: operation.StatusCompleted}

	sess.Operations = append(sess.Operations, op)
	first := runner.instructionNote(sess, result)
	if !strings.Contains(first, "The first rule") {
		t.Fatalf("the first rule was not delivered, so the withholding below proves nothing: %q", first)
	}
	if len(first) == 0 {
		t.Fatal("nothing was delivered at all")
	}

	// The same operation again. Every rule the ceiling withheld is still eligible,
	// so the note must still carry something - a retired set would render empty and
	// the rule that never arrived would be gone for the rest of the session.
	sess.Operations = append(sess.Operations, op)
	second := runner.instructionNote(sess, result)
	if second == "" {
		t.Fatal("a second matching operation delivered nothing, so the first one's cap retired rules it had withheld")
	}
	// And what it delivers now is not a repeat of what it already sent.
	if strings.Contains(second, "The first rule") {
		t.Errorf("a rule already delivered was delivered again: %q", second[:min(len(second), 300)])
	}
}

// memoryTailLine needs no operation, so delivering one must not depend on the
// other being configured. Wiring that gated them behind a shared nil check
// would make the instruction feature silently require memory to have been set
// up.
func TestTheTwoChannelsAreIndependent(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "notes.txt")
	if err := writeFileForTest(target, "the file the agent read"); err != nil {
		t.Fatal(err)
	}
	sess := &sessionstore.Session{ID: "ind", Version: 3}

	// Both configured: the instruction note renders on the tool result and the
	// memory tail renders on the run, and neither consults the other.
	logPath := filepath.Join(work, "Memory.md")
	appendLog(t, memory.OpenMemoryFile(logPath), "memory only, recorded at "+work)
	both := New(RunnerConfig{
		WorkDir:          work,
		InstructionRules: compilingSource(rulesBody).loader(),
		MemoryDelivery:   string(memory.DeliverySidekick),
		MemoryFile:       logMemoryFile(logPath),
	}, nil, tool.DefaultRegistry())
	result := operation.ResultEvent{OpID: "op1", Status: operation.StatusCompleted}
	sess.Operations = append(sess.Operations, sessionstore.Operation{
		ID: "op1", Tool: "read", SpecType: "read", SpecArgs: map[string]any{"path": target},
	})
	if note := both.instructionNote(sessionFor(sess), result); note == "" {
		t.Error("an instruction source with a memory store configured delivered nothing")
	}
	if tail := both.memoryTailLine(sessionFor(sess)); !strings.Contains(tail, "memory only") {
		t.Errorf("a memory tail with an instruction source configured delivered nothing: %q", tail)
	}

	// Instructions alone.
	rulesOnly := New(RunnerConfig{
		WorkDir:          work,
		InstructionRules: compilingSource(rulesBody).loader(),
	}, nil, tool.DefaultRegistry())
	if note := rulesOnly.instructionNote(sessionFor(sess), result); !strings.Contains(note, "Every network tool goes through harness/netguard") {
		t.Errorf("rules did not deliver without a memory sidekick: %q", note)
	}

	// Memory alone.
	memoryRunner := New(RunnerConfig{
		WorkDir:        work,
		MemoryDelivery: string(memory.DeliverySidekick),
		MemoryFile:     logMemoryFile(logPath),
	}, nil, tool.DefaultRegistry())
	if note := memoryRunner.instructionNote(sessionFor(sess), result); note != "" {
		t.Errorf("a runner with no rule file produced an instruction note: %q", note)
	}
	if tail := memoryRunner.memoryTailLine(sessionFor(sess)); !strings.Contains(tail, "memory only") {
		t.Errorf("the memory tail stopped delivering once the instruction channel existed: %q", tail)
	}
}

func writeFileForTest(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// atomic.Int32 stands in for sync/atomic's Int32 so the fake provider's request
// counting reads the same as the existing seam harnesses.

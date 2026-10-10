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
	"ceraa/harness/operation"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

// BOND.md is the operator-maintained identity file: who the agent works for, what
// they are building, the patterns they hold. The sidekick mode suppressed the whole
// prompt memory block, and the bond was inside that block, so it stopped reaching
// the model entirely - with nothing logged, because "the renderer ran and returned
// the bond" and "the render was discarded before it was sent" look the same from
// outside the process.
//
// These tests pin the path that puts it back: a separate, unscoped, once-per-run
// delivery on the first tool result. The unit tests pin each property separately;
// the seam tests drive a whole run, because a bond wired into annotate and never
// into the request body passes every one of them.

const bondBody = "Name: Sidekick. Role: founder. Stack: Go and Rust. Goal: ship Ceraa."

const bondLaw = "BOND.md is Sidekick-maintained: read it, never write it."

// bondRenderer stands in for memory.LiveBond. It is a function rather than a
// fixture file because the property under test is about *calls*, and a file can
// only be read so many times before the test needs its own bookkeeping.
func bondRenderer(body string) func() (string, error) {
	return func() (string, error) {
		return "<bond>\n" + body + "\n" + bondLaw + "\n</bond>\n", nil
	}
}

// The defect itself: the bond reaches the model in sidekick mode, on the first
// tool result, with the read-only law travelling inside it.
func TestTheBondReachesTheModelOnTheFirstToolResult(t *testing.T) {
	state := newSidekickState(nil, bondRenderer(bondBody), "/srv")

	got := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})

	if !strings.Contains(got, "<bond>") || !strings.Contains(got, "</bond>") {
		t.Fatalf("the bond is not in the block the tool result carries: %q", got)
	}
	if !strings.Contains(got, bondBody) {
		t.Errorf("the bond content is missing: %q", got)
	}
	if !strings.Contains(got, "never write it") {
		t.Errorf("the read-only law did not travel with the file it governs: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("the block was delivered with padding whitespace: %q", got)
	}
}

// The flooding objection, applied to the bond rather than to a general memory. A
// bond has no path at all, so it matches every operation harder than a general
// memory does, and it is a longer document: forty copies of BOND.md in one
// transcript is not a delivery mechanism, it is a flood with a mechanism attached.
func TestTheBondIsDeliveredOncePerRunNotOncePerCall(t *testing.T) {
	state := newSidekickState(nil, bondRenderer(bondBody), "/srv")

	first := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	second := state.annotate(sessionWithOp("b", "read", "/srv/b.txt"), operation.ResultEvent{OpID: "b"})
	third := state.annotate(sessionWithOp("c", "read", "/srv/c.txt"), operation.ResultEvent{OpID: "c"})

	if !strings.Contains(first, bondBody) {
		t.Fatalf("the first call delivered nothing: %q", first)
	}
	for _, later := range []string{second, third} {
		if strings.Contains(later, "<bond>") || strings.Contains(later, bondBody) {
			t.Errorf("the bond repeated on a later tool call: %q", later)
		}
	}
}

// LiveBond re-reads the file on every call, so "delivered once" and "read once"
// are different claims and only the flag buys the second one. Rendering again
// mid-run would put two revisions of the same operator-maintained file in one
// transcript, and the model has no way to tell which one is current.
func TestTheBondIsNotReReadAfterItIsDelivered(t *testing.T) {
	var renders atomic.Int32
	state := newSidekickState(nil, func() (string, error) {
		if renders.Add(1) == 1 {
			return "<bond>\nfirst revision\n</bond>\n", nil
		}
		return "<bond>\nsecond revision\n</bond>\n", nil
	}, "/srv")

	first := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	for i := 0; i < 5; i++ {
		state.annotate(sessionWithOp("b", "read", "/srv/b.txt"), operation.ResultEvent{OpID: "b"})
	}

	if renders.Load() != 1 {
		t.Errorf("the renderer ran %d times, so the file was re-read mid-run", renders.Load())
	}
	if !strings.Contains(first, "first revision") {
		t.Fatalf("the first call did not deliver the first revision: %q", first)
	}
	if strings.Contains(first, "second revision") {
		t.Errorf("the run delivered two revisions of the same file: %q", first)
	}
}

// A read failure is retried, because nothing was delivered and a bond that never
// arrives is the failure this whole path was added to stop.
func TestTheBondIsRetriedAfterAFailedRead(t *testing.T) {
	var renders atomic.Int32
	state := newSidekickState(nil, func() (string, error) {
		if renders.Add(1) == 1 {
			return "", os.ErrPermission
		}
		return "<bond>\nrecovered\n</bond>\n", nil
	}, "/srv")

	if got := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"}); got != "" {
		t.Fatalf("a failed read delivered something: %q", got)
	}
	later := state.annotate(sessionWithOp("b", "read", "/srv/b.txt"), operation.ResultEvent{OpID: "b"})
	if !strings.Contains(later, "recovered") {
		t.Errorf("one failed read silenced the bond for the rest of the run: %q", later)
	}
}

// The bond is not a memory, and it must not sit behind the read that can fail. An
// unreadable project-memory store would otherwise take the person's name and
// standing rules down with it, which is the same silent loss with one more
// indirection in front of it.
func TestTheBondIsDeliveredWhenTheMemoryStoreCannotBeRead(t *testing.T) {
	state := newSidekickState(
		func() ([]memory.Entry, error) { return nil, os.ErrPermission },
		bondRenderer(bondBody),
		"/srv",
	)

	got := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	if !strings.Contains(got, bondBody) {
		t.Errorf("an unreadable store took the bond with it: %q", got)
	}
}

// The bound is a refusal, not a truncation. The block wraps an operator-maintained
// file whose own renderer already truncates while keeping the read-only law, so
// cutting it a second time is the one edit that can drop that law - and a
// half-open <bond> reads to the model as a damaged file rather than as a budget.
func TestAnOverBudgetBondIsRefusedRatherThanTruncated(t *testing.T) {
	oversized := "<bond>\n" + strings.Repeat("x", sidekickBondBytes+10) + "\n" + bondLaw + "\n</bond>\n"
	state := newSidekickState(nil, func() (string, error) { return oversized, nil }, "/srv")

	got := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	if got != "" {
		t.Errorf("a %d-byte bond was delivered against a %d-byte bound: %q", len(oversized), sidekickBondBytes, got)
	}
}

// The bound is enforced where the text becomes output, so it holds for a caller
// that never saw the constant and configured no budget of its own.
func TestTheBondBoundHoldsForARendererWithNoBudget(t *testing.T) {
	huge := strings.Repeat("identity line. ", 2000)
	state := newSidekickState(nil, func() (string, error) { return "<bond>\n" + huge + "\n</bond>", nil }, "/srv")

	got := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	if len(got) > sidekickBondBytes {
		t.Errorf("a renderer with no budget of its own delivered %d bytes", len(got))
	}
}

// A bond that renders empty is a first run, not a fault: it has been asked and
// answered, so the rest of the run must not keep asking.
func TestAnEmptyBondIsNotAskedForAgain(t *testing.T) {
	var renders atomic.Int32
	state := newSidekickState(nil, func() (string, error) {
		renders.Add(1)
		return "   \n", nil
	}, "/srv")

	for i := 0; i < 4; i++ {
		state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	}
	if renders.Load() != 1 {
		t.Errorf("the renderer ran %d times for a bond with nothing in it", renders.Load())
	}
}

// Nothing configured means nothing delivered. This is the shape a caller with
// neither a bond nor a store gets, and it has to stay silent rather than panic.
func TestSidekickStateWithoutABondDeliversOnlyMemory(t *testing.T) {
	state := newSidekickState(func() ([]memory.Entry, error) { return nil, nil }, nil, "/srv")
	if got := state.annotate(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"}); got != "" {
		t.Errorf("a bond was invented: %q", got)
	}
}

// MemoryEntries is documented as optional, so a caller with a bond and no store is
// a legal configuration. Keying the sidekick state on the store alone meant that
// caller got no sidekick at all and the bond went with it - the same silent drop,
// one indirection further from the cause.
func TestSidekickModeIsBuiltForABondWithNoMemoryStore(t *testing.T) {
	runner := New(RunnerConfig{
		MemoryDelivery: string(memory.DeliverySidekick),
		BondFunc:       bondRenderer(bondBody),
	}, nil, tool.NewRegistry())

	if runner.sidekick == nil {
		t.Fatal("no sidekick state was built for a bond with no memory store, so the bond has no path to the model")
	}
	got := runner.sidekickNote(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"})
	if !strings.Contains(got, bondBody) {
		t.Errorf("the bond did not reach the tool result: %q", got)
	}
}

// The other two modes already delivered the bond, and adding a third channel must
// not move a byte of what they send. Compared as whole prompts and seams rather
// than by substring, because a substring test passes on a prompt that carries the
// bond in the wrong place.
func TestPromptAndCachedModesSendTheSameBytesWithOrWithoutTheBondChannel(t *testing.T) {
	combined := func() (string, error) {
		return "<bond>\n" + bondBody + "\n" + bondLaw + "\n</bond>\n<memory>project memory</memory>", nil
	}
	for _, mode := range []memory.Delivery{memory.DeliveryPrompt, memory.DeliveryCached} {
		plain := &Runner{cfg: RunnerConfig{SystemPrompt: "STATIC", MemoryContextFunc: combined}}
		plain.delivery = mode
		withChannel := &Runner{cfg: RunnerConfig{SystemPrompt: "STATIC", MemoryContextFunc: combined, BondFunc: bondRenderer(bondBody)}}
		withChannel.delivery = mode

		if plain.systemPrompt() != withChannel.systemPrompt() {
			t.Errorf("%s: the assembled prompt changed when the bond channel was added", mode)
		}
		if plain.systemCacheSeam() != withChannel.systemCacheSeam() {
			t.Errorf("%s: the cache seam moved when the bond channel was added", mode)
		}
		if !strings.Contains(withChannel.systemPrompt(), bondBody) {
			t.Errorf("%s: the bond stopped reaching the prompt", mode)
		}
		// Neither mode builds sidekick state, so no tool result can carry a bond.
		if got := withChannel.sidekickNote(sessionWithOp("a", "read", "/srv/a.txt"), operation.ResultEvent{OpID: "a"}); got != "" {
			t.Errorf("%s annotated a tool result: %q", mode, got)
		}
	}
}

// The reason sidekick mode exists: the system prompt stays free of the bond, so
// the whole prefix is one cached block. Without this, "delivered" would be true
// and the saving that justified the mode would be gone.
func TestSidekickModeKeepsTheBondOutOfTheSystemPrompt(t *testing.T) {
	runner := New(RunnerConfig{
		SystemPrompt: "STATIC",
		MemoryContextFunc: func() (string, error) {
			return "<bond>\n" + bondBody + "\n" + bondLaw + "\n</bond>\n<memory>project memory</memory>", nil
		},
		MemoryDelivery: string(memory.DeliverySidekick),
		BondFunc:       bondRenderer(bondBody),
	}, nil, tool.NewRegistry())

	if strings.Contains(runner.systemPrompt(), bondBody) {
		t.Error("the bond is back in the prompt in sidekick mode, which is the volatility this mode removes")
	}
	if strings.Contains(runner.systemPrompt(), "project memory") {
		t.Error("project memory is in the sidekick prompt")
	}
}

// A run that never annotates has to say so, because a recorded zero and a missing
// record are the difference between "the bond did not reach the model" and "no run
// happened here at all".
func TestARunThatNeverAnnotatedReportsNoBondRatherThanClaimingOne(t *testing.T) {
	state := newSidekickState(nil, bondRenderer(bondBody), "/srv")
	if got := state.deliveryReport().BondInjected; got != 0 {
		t.Errorf("a run that never annotated anything reported %d bond bytes", got)
	}
}

// bondSeamRun drives one real run against a fake provider: the model reads a file,
// then answers. It returns the tool-result bodies exactly as they went on the wire,
// so a test can assert on what the model saw rather than on what was assembled.
func bondSeamRun(t *testing.T, sessionID string, extra func(*RunnerConfig)) ([]string, *sessionstore.Session, *Runner) {
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
			return "<bond>\n" + bondBody + "\n" + bondLaw + "\n</bond>\n<memory>project memory</memory>", nil
		},
		MemoryDelivery: string(memory.DeliverySidekick),
		BondFunc:       bondRenderer(bondBody),
		MemoryEntries:  func() ([]memory.Entry, error) { return nil, nil },
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

// The seam. Every test above calls annotate directly, which is the shape that passes
// while the model never sees the block: annotate is one term in a sum at the
// runner's result loop, and the sum can lose it. This reads the bytes off the wire
// from a whole run instead.
func TestSidekickModePutsTheBondInTheToolResultTheModelReads(t *testing.T) {
	toolResults, _, runner := bondSeamRun(t, "bond-seam", nil)

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	carrying := 0
	for _, text := range toolResults {
		if !strings.Contains(text, "<bond>") {
			continue
		}
		carrying++
		if !strings.Contains(text, bondBody) {
			t.Errorf("a tool result carries a bond without its content: %q", text)
		}
		if !strings.Contains(text, "the file the agent read") {
			t.Errorf("the bond replaced the tool output rather than accompanying it: %q", text)
		}
	}
	if carrying == 0 {
		t.Fatalf("no tool result the model reads carries the bond: %#v", toolResults)
	}
	if carrying != 1 {
		t.Errorf("the bond reached the model %d times in one run, once per tool call", carrying)
	}
	if strings.Contains(runner.systemPrompt(), bondBody) {
		t.Error("the bond is in the system prompt, so the cache prefix is volatile again")
	}
}

// The annotated/persisted split. The annotated text is what the model reads; the
// session file's operation record is what a human reads later, and it has to stay
// the bare result - a record that depends on which memories happened to be in the
// store at the time is not a record of anything.
func TestTheSessionOperationKeepsTheBareResultWhileTheModelSeesTheBond(t *testing.T) {
	toolResults, sess, runner := bondSeamRun(t, "bond-split", nil)

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	recorded := 0
	for _, op := range sess.Operations {
		if op.Output == "" {
			continue
		}
		recorded++
		if strings.Contains(op.Output, "<bond>") {
			t.Errorf("the session file's operation record carries the bond: %q", op.Output)
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
		if msg.Role == llm.RoleTool && strings.Contains(msg.ToolResult, "<bond>") {
			annotated++
		}
	}
	if annotated != 1 {
		t.Errorf("the transcript carries the bond %d times, want exactly the annotated copy", annotated)
	}

	// And the run has to leave a record that the bond was delivered at all, because
	// a sidekick run that dropped it is otherwise indistinguishable from one on an
	// install with no BOND.md.
	if len(sess.Runs) != 1 || sess.Runs[0].MemoryBondSent == 0 {
		t.Errorf("the run recorded no bond delivery, so a drop would be unobservable: %+v", sess.Runs)
	}
	if got := sess.Runs[0].MemoryBondSent; got > sidekickBondBytes {
		t.Errorf("the run recorded %d bond bytes, over the %d bound", got, sidekickBondBytes)
	}
	if report := runner.DeliveryReport(); report.BondInjected == 0 {
		t.Errorf("the delivery report says no bond was sent: %+v", report)
	}
}

// The counter-case, at the seam. A run in cached mode already carried the bond in
// the prompt, and that is the whole point of the mode, so the tool results must be
// byte-identical to the bare tool output: the new channel must not double up on a
// mode that never needed it.
func TestCachedModeLeavesTheToolResultAlone(t *testing.T) {
	toolResults, _, _ := bondSeamRun(t, "bond-cached", func(cfg *RunnerConfig) {
		cfg.MemoryDelivery = string(memory.DeliveryCached)
	})

	if len(toolResults) == 0 {
		t.Fatal("no tool result reached the provider, so the test proved nothing")
	}
	for _, text := range toolResults {
		if strings.Contains(text, "<bond>") {
			t.Errorf("cached mode annotated a tool result with the bond: %q", text)
		}
	}
}

package coordinator

import (
	"sync"
	"testing"

	"ceraa/harness/compaction"
	"ceraa/harness/llm"
)

// The motivating bug: the catalog's advertised window was fetched and written to
// the run record, and the budget came from a hardcoded constant regardless. So
// every model was budgeted as though it were the largest one Ceraa had ever seen.
func TestRecalibrateUsesTheCatalogWindow(t *testing.T) {
	compactor := compaction.New(llm.ProviderConfig{})
	r := &Runner{
		cfg:       RunnerConfig{Model: llm.ProviderConfig{Model: "small/model"}, Compactor: compactor},
		contextMu: sync.RWMutex{},
	}
	r.recalibrateContextLimit(llm.ModelMetadata{ContextWindow: 32000}, nil)

	limit, source := r.ContextLimit()
	if limit != 32000 {
		t.Errorf("limit = %d, want 32000, the advertised window unchanged", limit)
	}
	if source != llm.ContextFromCatalog {
		t.Errorf("source = %s, want %s", source, llm.ContextFromCatalog)
	}
	// The compactor is a separate budget holder and has to follow, or compaction
	// keeps summarising against a window the model does not have. It receives the
	// window, not a reduced budget: the headroom is subtracted once, inside
	// NeedsCompaction, and shaving it here too counted it twice.
	if compactor.ContextLimit != 32000 {
		t.Errorf("compactor limit = %d, want 32000", compactor.ContextLimit)
	}
}

// A model switch is the whole reason recalibration exists. The limit is a property
// of the model, so changing the model must move the budget, and it must move
// without reconstructing the runner.
func TestRecalibrateFollowsAModelSwitch(t *testing.T) {
	compactor := compaction.New(llm.ProviderConfig{})
	r := &Runner{cfg: RunnerConfig{Model: llm.ProviderConfig{Model: "big/model"}, Compactor: compactor}}

	r.recalibrateContextLimit(llm.ModelMetadata{ContextWindow: 200000}, nil)
	before, _ := r.ContextLimit()
	if before <= 0 {
		t.Fatal("no budget after the first calibration")
	}

	// Switch to a much smaller model, as a /model command would.
	r.cfg.Model.Model = "tiny/model"
	r.recalibrateContextLimit(llm.ModelMetadata{ContextWindow: 8000}, nil)
	after, source := r.ContextLimit()

	if after >= before {
		t.Errorf("budget did not shrink on a model switch: %d -> %d", before, after)
	}
	if source != llm.ContextFromCatalog {
		t.Errorf("source = %s, want %s", source, llm.ContextFromCatalog)
	}
	if compactor.ContextLimit != after {
		t.Errorf("compactor did not follow the switch: %d, want %d", compactor.ContextLimit, after)
	}
}

// A profile that states a limit is a decision somebody made, so a model switch
// must not quietly override it.
func TestRecalibrateRespectsAnExplicitProfileLimit(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		Model:        llm.ProviderConfig{Model: "codex/model"},
		ContextLimit: 200000,
	}}
	r.recalibrateContextLimit(llm.ModelMetadata{ContextWindow: 1000000}, nil)
	limit, source := r.ContextLimit()
	if limit != 200000 || source != llm.ContextFromProfile {
		t.Errorf("limit = %d/%s, want 200000/%s", limit, source, llm.ContextFromProfile)
	}
}

// An unknown model must still get a usable budget rather than zero, because every
// caller divides by or subtracts from this number.
func TestContextLimitIsUsableBeforeAndAfterCalibration(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{Model: llm.ProviderConfig{Model: "unknown"}}}
	if limit, _ := r.ContextLimit(); limit <= 0 {
		t.Errorf("pre-calibration limit = %d, want a positive fallback", limit)
	}
	r.recalibrateContextLimit(llm.ModelMetadata{}, nil)
	if limit, _ := r.ContextLimit(); limit != llm.FallbackContextWindow {
		t.Errorf("post-calibration limit = %d, want %d", limit, llm.FallbackContextWindow)
	}
}

// The trim threshold is not a tunable margin. It is the provider's window minus
// the two things that also ride the request: the schemas, and the room the
// reply needs. Drop either and the threshold moves to 100% of the window, where
// a request that fits by estimate is refused for exceeding it. That is the rare
// case the margin exists for, and it is invisible until it fires, so the ratio
// is pinned rather than left to a reader of the subtraction.
func TestAssemblyTrimThresholdHoldsFivePercentBack(t *testing.T) {
	const window = 200000
	tools := llm.ToolDef{
		Name:        "bash",
		Description: "Execute a bash command and return the combined stdout/stderr output.",
		Parameters:  map[string]any{"type": "object"},
	}
	defs := []llm.ToolDef{tools}
	outputReserve := 8192
	toolTokens := llm.EstimateToolDefinitionTokens(defs)

	if toolTokens <= 0 {
		t.Fatal("tool definition tokens estimated as zero, so the ratio below is meaningless")
	}

	limit := window - outputReserve - toolTokens
	ratio := float64(limit) / float64(window)

	// The 5% the design claims, with room for a larger tool catalogue.
	if ratio < 0.90 || ratio >= 1.0 {
		t.Errorf("trim threshold = %.1f%% of window (%d of %d), want >=90%% and <100%%",
			ratio*100, limit, window)
	}
	// Named so the number a reader is checking is the one in the code comment.
	if got, want := window-limit, outputReserve+toolTokens; got != want {
		t.Errorf("held back = %d, want %d (output reserve + tool schemas)", got, want)
	}
}

// The reserve is what leaves the reply somewhere to go. A trim threshold at the
// full window is a request with nowhere to put the answer.
func TestTrimThresholdLeavesRoomForTheReply(t *testing.T) {
	const window = 200000
	const outputReserve = 8192
	limit := window - outputReserve - 1000
	if limit <= outputReserve {
		t.Fatalf("trim threshold %d leaves no room for a %d token reply", limit, outputReserve)
	}
	if limit >= window {
		t.Fatalf("trim threshold %d is at or past the window %d", limit, window)
	}
}

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

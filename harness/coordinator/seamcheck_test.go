package coordinator

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
)

// The seam must land exactly on the boundary between the stable prefix and the
// volatile tail. Off by even one byte and the stable block either loses its
// trailing newline or absorbs the date, so the whole prefix is re-read whenever
// the date rolls over.
func TestSystemCacheSeamLandsOnTheStableBoundary(t *testing.T) {
	static := strings.Repeat("instruction line. ", 400)
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt:      static,
		MemoryContextFunc: func() (string, error) { return "<memory>note</memory>", nil },
	}}

	full := r.systemPrompt()
	seam := r.systemCacheSeam()
	if seam <= 0 || seam >= len(full) {
		t.Fatalf("seam %d is outside the prompt (%d bytes)", seam, len(full))
	}

	stable := full[:seam]
	volatile := full[seam:]

	// The date is the first volatile part, so it must be entirely in the tail.
	if strings.Contains(stable, "<current-date>") {
		t.Error("the date is inside the cached block, so a daily rollover re-reads the whole prefix")
	}
	if !strings.Contains(volatile, "<current-date>") {
		t.Error("the date is missing from the tail; the seam is past it")
	}
	// Memory is per-turn and must not be cached either.
	if strings.Contains(stable, "<memory>") {
		t.Error("live memory is inside the cached block, so every memory edit re-reads the whole prefix")
	}
	// The stable half must be the static prompt, unmodified.
	if !strings.Contains(stable, "instruction line.") {
		t.Error("the static prompt is not in the cached block")
	}
	// The block must be newline-terminated, or appending invalidates it.
	if !strings.HasSuffix(stable, "\n") {
		t.Errorf("the cached block is not newline-terminated: %q", stable[max(0, len(stable)-20):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// The identity block is optional, and when it is absent the date moves from index
// 2 to index 1. An index-based rule then treats the date as cacheable, so a daily
// rollover re-reads the entire prefix and the seam looks fine on every other day.
func TestSeamIsCorrectWithoutTheIdentityBlock(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt: strings.Repeat("instruction line. ", 400),
		// no ResolveModel, so modelIdentity is empty and no identity block is added
	}}

	full := r.systemPrompt()
	stable, volatile := full[:r.systemCacheSeam()], full[r.systemCacheSeam():]
	if strings.Contains(stable, "<current-date>") {
		t.Error("with no identity block the date fell inside the cached prefix")
	}
	if !strings.Contains(volatile, "<current-date>") {
		t.Error("the date is not in the tail")
	}
}

// The seam must not depend on whether the identity block resolved. When it does
// resolve, everything before the date is still static prompt plus identity, and
// both shapes must leave the date out of the cached region.
func TestSeamDoesNotDependOnIdentityResolution(t *testing.T) {
	static := strings.Repeat("instruction line. ", 400)
	without := &Runner{cfg: RunnerConfig{SystemPrompt: static}}
	with := &Runner{cfg: RunnerConfig{
		SystemPrompt: static,
		Model:        llm.ProviderConfig{Model: "gpt-5.6-luna", BaseURL: "https://example.invalid"},
	}}
	if without.modelIdentity() != "" {
		t.Fatal("fixture: the no-identity case resolved an identity anyway")
	}
	if with.modelIdentity() == "" {
		t.Fatal("fixture: the with-identity case resolved nothing")
	}
	for name, r := range map[string]*Runner{"without identity": without, "with identity": with} {
		full := r.systemPrompt()
		seam := r.systemCacheSeam()
		if strings.Contains(full[:seam], "<current-date>") {
			t.Errorf("%s: the date is inside the cached prefix", name)
		}
		if !strings.Contains(full[seam:], "<current-date>") {
			t.Errorf("%s: the date is missing from the tail", name)
		}
	}
}

package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"ceraa/internal/config"
	"ceraa/internal/telegram/keyboard"
	"ceraa/internal/telegram/menus"
)

// The variant menu was a compaction tuning knob presented as a writing style: its
// entire effect was halving the retained history and starting compaction earlier,
// and it offered no direction for keeping more. It is removed rather than renamed,
// because there is no third setting worth having and the honest place for a
// compaction budget is the compaction menu.
func TestVariantIsGoneFromTheHub(t *testing.T) {
	for _, name := range menus.All {
		if name == "variant" {
			t.Fatal("the hub still offers a variant entry")
		}
	}
	callbacks := callbacksIn(t, menus.Root())
	if hasCallback(callbacks, "menu:variant") {
		t.Error("the compact root menu still offers a variant entry")
	}
	if hasCallback(callbacks, "💭") {
		t.Error("a variant shortcut is still in the root menu")
	}
}

// The keyboard callback is a separate surface from the menus, so it needs its own
// assertion.
func TestVariantIsGoneFromTheReplyKeyboard(t *testing.T) {
	raw, _ := json.Marshal(keyboard.Main())
	if strings.Contains(string(raw), "Variant") {
		t.Errorf("the reply keyboard still mentions the variant: %s", raw)
	}
	// Thinking took the slot, so the keyboard is not left with a hole.
	if !strings.Contains(string(raw), "Thinking") {
		t.Errorf("the reply keyboard lost its other shortcut: %s", raw)
	}
}

// A stale callback in an old chat must not silently do something, and must not
// panic either.
func TestStaleVariantCallbackIsInert(t *testing.T) {
	cap := beginCapture(t)
	tap(t, "select_variant:compact")
	// No state change, and certainly no error text claiming it worked.
	if strings.Contains(cap.text(), "Variant set") {
		t.Errorf("a stale variant callback still reported a change: %q", cap.text())
	}
}

// The status report listed the variant, so it has to stop.
func TestStatusReportHasNoVariantLine(t *testing.T) {
	isolateState(t)
	text, ok := menuReportText("status", 1)
	if !ok {
		t.Fatal("no status report")
	}
	if strings.Contains(text, "Variant") {
		t.Errorf("the status report still mentions the variant:\n%s", text)
	}
}

// The behaviour it used to change must be gone too, not just the menu. A retained
// 4000/30000 override would quietly change how much history Ceraa keeps for
// anyone whose state file still carries the old value.
func TestCompactorNoLongerReadsAVariant(t *testing.T) {
	isolateState(t)
	// A stale state file may still carry variant=compact; the builder must ignore
	// it rather than read a field that no longer exists.
	compactor := newCompactor(config.DefaultConfig(), "codex", "sess")
	if compactor.Settings.Keep.Tokens == 4000 {
		t.Error("the compactor still applies the old compact-variant keep budget")
	}
	if compactor.Settings.Buffer == 30000 {
		t.Error("the compactor still applies the old compact-variant buffer")
	}
}

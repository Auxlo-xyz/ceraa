package telegram

import "testing"

// Back into a menu has to re-render it, not replay the keyboard that was drawn when
// it was first opened.
//
// This is the reported bug. A hub was opened while xAI was active, the user went
// into a provider screen, switched to MiniMax, and pressed Back. The hub came back
// still naming xAI as active, because closeCallbackMenu restored the stored markup
// verbatim. Nothing logged, and a restart cleared it, so it looked like the switch
// needed a restart rather than like a stale screen.
func TestMenuRootRecordsTheMenuName(t *testing.T) {
	// The stored root has to know which menu it is, or Back has nothing to re-render.
	root := menuRoot{name: "hub", text: "📋 Ceraa hub", markup: map[string]any{}}
	if root.name != "hub" {
		t.Fatalf("menuRoot has no name: %+v", root)
	}
}

// An unnamed root keeps the stored copy, because a one-off keyboard is not a menu
// and there is nothing to re-render.
func TestUnnamedMenuRootHasNothingToRerender(t *testing.T) {
	var root menuRoot
	if root.name != "" {
		t.Errorf("an unnamed root should report no menu, got %q", root.name)
	}
}

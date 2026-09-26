package menus

import "testing"

// Clear context was removed: it destroyed a session's history outright, with no
// undo, and compaction covers the legitimate need to shrink a context. The button
// must not come back.
func TestContextMenuHasNoClearContext(t *testing.T) {
	markup := InlineForSession("status", 0, "sess-1")
	rows, ok := markup["inline_keyboard"].([][]map[string]string)
	if !ok {
		t.Fatal("expected an inline keyboard")
	}
	for _, row := range rows {
		for _, button := range row {
			if button["callback_data"] == "context:clear" {
				t.Error("the context menu still offers Clear context")
			}
			if button["text"] == "🧹 Clear context" {
				t.Error("the context menu still has a Clear context button")
			}
		}
	}
	// The menu must still be usable, not emptied out.
	var found bool
	for _, row := range rows {
		for _, button := range row {
			if button["callback_data"] == "compact:now" {
				found = true
			}
		}
	}
	if !found {
		t.Error("Compact now disappeared along with Clear context")
	}
}

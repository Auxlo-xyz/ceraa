package tool

import (
	"slices"
	"testing"
)

// Pinned so adding a tool forces a decision about whether repeating it after an
// unknown outcome is safe. The failure mode of forgetting is the cautious one,
// but forgetting silently is still how a mutating tool ends up on a list.
func TestReadOnlySetIsPinnedAndEveryNameIsARealTool(t *testing.T) {
	want := []string{
		"fts_search", "get_goal", "list_secrets", "question",
		"read", "view_image", "webfetch", "websearch",
	}
	got := ReadOnlyNames()
	if !slices.Equal(got, want) {
		t.Errorf("read-only set is %v, want %v", got, want)
	}

	registered := map[string]bool{}
	for _, tool := range DefaultRegistry().All() {
		registered[tool.Name()] = true
	}
	for _, name := range got {
		if !registered[name] {
			t.Errorf("%q is classified read-only but is not a registered tool", name)
		}
	}
}

// browser is the one that looks harmless and is not: it can click and type,
// which changes state on somebody else's server.
//
// script is in the same shape: its body is only JavaScript, but it can reach
// write and bash, so repeating one after an unknown outcome is not safe.
func TestBrowserIsNotReadOnly(t *testing.T) {
	if ReadOnly("browser") {
		t.Error("browser is classified read-only; it can click and type")
	}
	for _, name := range []string{"bash", "write", "edit_line", "batch_edit_file",
		"run_command_with_secret", "todowrite", "task", "browser", "script"} {
		if ReadOnly(name) {
			t.Errorf("%q is classified read-only but can change things", name)
		}
	}
}

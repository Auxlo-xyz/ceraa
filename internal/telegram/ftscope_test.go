package telegram

import (
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/search"
)

// A real session scoped two searches to a subdirectory and got back matches from
// soraacode, the Python 3.14 standard library and a tcl9 tarball. The path the
// model chose was being replaced with the project root before the tool saw it, so
// the tool could not have honoured the scope even after the search learned to
// filter by it.
//
// The parent runner and the subagent factory each had their own copy of this
// logic, which is why both were wrong; they now share one function.
func TestSearchScopeDefaultsWithoutOverridingACaller(t *testing.T) {
	for _, tc := range []struct {
		name  string
		given any
		want  string
	}{
		{"a caller's scope survives", "/workspace/auxlo-neo", "/workspace/auxlo-neo"},
		{"a relative scope survives", "src/worker", "src/worker"},
		{"absent defaults to the project root", nil, "/workspace"},
		{"empty defaults to the project root", "", "/workspace"},
		{"whitespace defaults to the project root", "   ", "/workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := operation.Spec{Type: "fts_search", Args: map[string]any{"query": "AuxloNeo"}}
			if tc.given != nil {
				spec.Args["path"] = tc.given
			}
			applySearchScope(&spec, "/workspace", "/workspace")
			if got, _ := spec.Args["path"].(string); got != tc.want {
				t.Errorf("path = %q, want %q", got, tc.want)
			}
			if db, _ := spec.Args["db_path"].(string); db == "" {
				t.Error("db_path was not set, so the search has no index to read")
			}
		})
	}
}

// The index is a host concern, so unlike the scope it is always replaced.
func TestSearchScopeAlwaysPointsAtTheProjectIndex(t *testing.T) {
	spec := operation.Spec{Type: "fts_search", Args: map[string]any{
		"query":   "q",
		"db_path": "/somewhere/else.db",
	}}
	applySearchScope(&spec, "/workspace/auxlo-neo", "/workspace")
	want := search.ProjectIndexPath("/workspace", "/workspace/auxlo-neo")
	if got, _ := spec.Args["db_path"].(string); got != want {
		t.Errorf("db_path = %q, want the project index %q", got, want)
	}
}

// Every other tool goes through this call, so it has to be inert for them.
func TestSearchScopeLeavesOtherToolsAlone(t *testing.T) {
	for _, specType := range []string{"read", "write", "edit_line", "batch_edit_file", "bash"} {
		spec := operation.Spec{Type: specType, Args: map[string]any{"path": "/tmp/keep-me"}}
		applySearchScope(&spec, "/workspace", "/workspace")
		if got, _ := spec.Args["path"].(string); got != "/tmp/keep-me" {
			t.Errorf("%s had its path rewritten to %q", specType, got)
		}
		if _, added := spec.Args["db_path"]; added {
			t.Errorf("%s was given a db_path", specType)
		}
	}
}

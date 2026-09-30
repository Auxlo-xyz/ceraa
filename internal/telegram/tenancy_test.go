package telegram

import (
	"os"
	"path/filepath"
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/search"
)

// The fts_search scope is honoured rather than overwritten, so the filter can name
// any path a user asks for. This pins the boundary that actually contains it.
//
// The boundary is the index, not the filter. Each project gets its own index file
// keyed by its path, and applySearchScope forces the index from the user's own
// project while taking only the scope from the caller. So a user who points a
// scoped search at somebody else's directory still queries their own index, and
// the query cannot return a row the index does not hold.
func TestAScopedSearchCannotReachAnotherUsersProject(t *testing.T) {
	workspace := t.TempDir()
	alice := filepath.Join(workspace, "alice")
	bob := filepath.Join(workspace, "bob")
	for _, dir := range []string{alice, bob} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bob, "secret.txt"), []byte("marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Alice's model asks for Bob's directory. The scope is hers to choose; the
	// index is not.
	ask := func(projectRoot, wantedScope string) operation.Spec {
		spec := operation.Spec{Type: "fts_search", Args: map[string]any{
			"query": "marker", "path": wantedScope,
		}}
		applySearchScope(&spec, projectRoot, workspace)
		return spec
	}

	aliceHonest := ask(alice, alice)
	aliceReaching := ask(alice, bob)
	bobHonest := ask(bob, bob)

	indexOf := func(s operation.Spec) string {
		v, _ := s.Args["db_path"].(string)
		return v
	}
	if indexOf(aliceHonest) != indexOf(aliceReaching) {
		t.Errorf("asking for another user's project changed the index: %q then %q",
			indexOf(aliceHonest), indexOf(aliceReaching))
	}
	if indexOf(aliceHonest) == indexOf(bobHonest) {
		t.Error("two users share one index, so the index is not the boundary")
	}
	if want := search.ProjectIndexPath(workspace, alice); indexOf(aliceHonest) != want {
		t.Errorf("db_path = %q, want the index for her own project %q", indexOf(aliceHonest), want)
	}
	// The scope she asked for is the one thing that does change.
	if aliceReaching.Args["path"] != bob {
		t.Error("the caller's scope was discarded, which is the bug this fixed")
	}
}

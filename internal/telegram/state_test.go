package telegram

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBridgeStatePersistsSessionAndProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	workspace := t.TempDir()
	t.Setenv("SOORAA_WORKSPACE_ROOT", workspace)
	project := filepath.Join(workspace, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	state := loadBridgeStateFromFile(path)
	if err := state.setSession(42, "session-a"); err != nil {
		t.Fatal(err)
	}
	if err := state.setProject(42, project); err != nil {
		t.Fatal(err)
	}
	loaded := loadBridgeStateFromFile(path)
	if loaded.sessionID(42) != "session-a" {
		t.Fatalf("session = %q", loaded.sessionID(42))
	}
	if loaded.project(42) == "" {
		t.Fatal("expected persisted project")
	}
	if loaded.projectForSession("session-a") == "" {
		t.Fatal("expected session project")
	}
}

func TestBridgeStateDefaultsToWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("SOORAA_WORKSPACE_ROOT", workspace)
	state := loadBridgeStateFromFile(filepath.Join(t.TempDir(), "bridge-state.json"))
	if state.project(42) != workspace {
		t.Fatalf("implicit project = %q", state.project(42))
	}
	if state.projectForSession("session-a") != workspace {
		t.Fatalf("implicit session project = %q", state.projectForSession("session-a"))
	}
}

func TestBridgeStateRuntimeValues(t *testing.T) {
	t.Setenv("SOORAA_MODEL", "test/model")
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	state := loadBridgeStateFromFile(path)
	if err := state.setModel("test/model"); err != nil {
		t.Fatal(err)
	}
	if err := state.setAgent("plan"); err != nil {
		t.Fatal(err)
	}
	if err := state.setVariant("compact"); err != nil {
		t.Fatal(err)
	}
	if err := state.setMaxSteps(7); err != nil {
		t.Fatal(err)
	}
	if err := state.setAutoCompaction(false); err != nil {
		t.Fatal(err)
	}
	model, agent, variant, steps := state.values()
	if model != "test/model" || agent != "plan" || variant != "compact" || steps != 7 {
		t.Fatalf("runtime values = %q %q %q %d", model, agent, variant, steps)
	}
	if enabled, configured := state.autoCompaction(); !configured || enabled {
		t.Fatal("expected automatic compaction to be disabled")
	}
}

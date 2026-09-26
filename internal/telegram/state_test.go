package telegram

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBridgeStatePersistsSessionAndProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	workspace := t.TempDir()
	t.Setenv("CERAA_WORKSPACE_ROOT", workspace)
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
	t.Setenv("CERAA_WORKSPACE_ROOT", workspace)
	state := loadBridgeStateFromFile(filepath.Join(t.TempDir(), "bridge-state.json"))
	if state.project(42) != workspace {
		t.Fatalf("implicit project = %q", state.project(42))
	}
	if state.projectForSession("session-a") != workspace {
		t.Fatalf("implicit session project = %q", state.projectForSession("session-a"))
	}
}

func TestBridgeStateRejectsMalformedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	if err := os.WriteFile(path, []byte(`{"allowed_user_id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := loadBridgeStateFromFile(path); state.loadErr == nil {
		t.Fatal("malformed bridge state must fail closed")
	}
}

func TestBridgeStateRejectsNegativeAllowedUserID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	if err := os.WriteFile(path, []byte(`{"allowed_user_id":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := loadBridgeStateFromFile(path); state.loadErr == nil {
		t.Fatal("negative bridge-state allowlist must fail closed")
	}
}

func TestBridgeStateClaimIsSingleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	state := loadBridgeStateFromFile(path)
	claimed, err := state.claimAllowedUserID(42)
	if err != nil || !claimed {
		t.Fatalf("first claim = %t, %v", claimed, err)
	}
	claimed, err = state.claimAllowedUserID(99)
	if err != nil || claimed {
		t.Fatalf("second claim = %t, %v", claimed, err)
	}
}

func TestBridgeStateRuntimeValues(t *testing.T) {
	t.Setenv("CERAA_MODEL", "test/model")
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
	if err := state.setAutoCompaction(false); err != nil {
		t.Fatal(err)
	}
	model, agent, variant := state.values()
	if model != "test/model" || agent != "plan" || variant != "compact" {
		t.Fatalf("runtime values = %q %q %q", model, agent, variant)
	}
	if enabled, configured := state.autoCompaction(); !configured || enabled {
		t.Fatal("expected automatic compaction to be disabled")
	}
}

// TestSetReasoningValidatesAndPersists pins that a bad level is rejected instead
// of being sent to the provider, and that "default" clears the override.
func TestSetReasoningValidatesAndPersists(t *testing.T) {
	state := loadBridgeStateFromFile(filepath.Join(t.TempDir(), "bridge-state.json"))
	if err := state.setReasoning("high"); err != nil {
		t.Fatalf("setReasoning: %v", err)
	}
	if state.reasoning() != "high" {
		t.Fatalf("reasoning = %q", state.reasoning())
	}
	if err := state.setReasoning("turbo"); err == nil {
		t.Fatal("an unknown level must be rejected")
	}
	if err := state.setReasoning(""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if state.reasoning() != "" {
		t.Fatalf("reasoning = %q, want cleared", state.reasoning())
	}
}

// TestExpectAnswerRoutesTheNextMessageToTheQuestion pins that a typed answer is
// claimed by the question that asked for it, and only once.
//
// Without this, a free-form answer is indistinguishable from a new task, so the
// answer would be run as a prompt and the question would time out.
func TestExpectAnswerRoutesTheNextMessageToTheQuestion(t *testing.T) {
	state := loadBridgeStateFromFile(filepath.Join(t.TempDir(), "bridge-state.json"))
	state.expectAnswer("q-1", 4242)

	if _, waiting := state.takeAnswer(9999); waiting {
		t.Fatal("another chat must not claim this answer")
	}
	if requestID, waiting := state.takeAnswer(4242); !waiting || requestID != "q-1" {
		t.Fatalf("takeAnswer = %q, %v", requestID, waiting)
	}
	if _, waiting := state.takeAnswer(4242); waiting {
		t.Fatal("a second message must be treated as a new task, not another answer")
	}
}

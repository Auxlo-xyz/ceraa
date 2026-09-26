package sessionstore

import (
	"os"
	"path/filepath"
	"testing"

	"ceraa/harness/llm"
)

func TestSaveIsAtomicAndReloadable(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "atomic", Messages: []Message{{Role: llm.RoleUser, Content: "hello"}}}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("atomic")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello" {
		t.Fatalf("unexpected session: %+v", loaded)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary session files remain: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(dir, "atomic.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverAddsMissingToolResults(t *testing.T) {
	sess := &Session{Messages: []Message{
		{
			Role:      llm.RoleUser,
			Content:   "run it",
			ToolCalls: nil,
		}, {
			Role:      llm.RoleAssistant,
			Content:   "calling",
			ToolCalls: []ToolCallRef{{ID: "call_1", Name: "bash", Args: `{"command":"echo hi"}`}},
		}, {
			Role:       llm.RoleTool,
			ToolCallID: "call_1",
			ToolResult: "done",
		}, {
			Role:      llm.RoleAssistant,
			ToolCalls: []ToolCallRef{{ID: "call_2", Name: "bash", Args: `{"command":"echo bye"}`}},
		}}}
	if !Recover(sess) {
		t.Fatal("expected recovery changes")
	}
	last := sess.Messages[len(sess.Messages)-1]
	if last.Role != llm.RoleTool || last.ToolCallID != "call_2" {
		t.Fatalf("unexpected recovery message: %+v", last)
	}
	if len(sess.Messages) < 2 || sess.Messages[len(sess.Messages)-2].Role != llm.RoleAssistant {
		t.Fatalf("recovery result was not placed after its call: %+v", sess.Messages)
	}
	if len(sess.Operations) != 0 {
		t.Fatalf("unexpected operations: %+v", sess.Operations)
	}
}

func TestRecoverMaterializesPersistedOperationResult(t *testing.T) {
	sess := &Session{Messages: []Message{{
		Role:      llm.RoleAssistant,
		ToolCalls: []ToolCallRef{{ID: "call-1", Name: "bash", Args: `{"command":"echo hi"}`}},
	}}, Operations: []Operation{{
		ID:            "call-1",
		ToolCallID:    "call-1",
		Tool:          "bash",
		Status:        StatusCompleted,
		Output:        "hi\n",
		ResultPresent: true,
	}}}
	if !Recover(sess) {
		t.Fatal("expected recovery changes")
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("expected one materialized result, got %d messages", len(sess.Messages))
	}
	if sess.Messages[1].ToolResult != "hi\n" || !sess.Messages[1].ToolResultPresent {
		t.Fatalf("unexpected materialized result: %+v", sess.Messages[1])
	}
	if Recover(sess) {
		t.Fatal("recovery should be idempotent")
	}
}

func TestRecoverMarksRunningOperationUnknown(t *testing.T) {
	sess := &Session{Messages: []Message{{
		Role:      llm.RoleAssistant,
		ToolCalls: []ToolCallRef{{ID: "call-1", Name: "write", Args: `{"path":"answer","content":"done"}`}},
	}}, Operations: []Operation{{
		ID:         "call-1",
		ToolCallID: "call-1",
		Tool:       "write",
		Status:     StatusRunning,
	}}}
	if !Recover(sess) {
		t.Fatal("expected recovery changes")
	}
	if sess.Operations[0].Status != StatusUnknown {
		t.Fatalf("expected unknown operation, got %s", sess.Operations[0].Status)
	}
	if !sess.Messages[1].ToolResultPresent || sess.Messages[1].ResultStatus != StatusUnknown {
		t.Fatalf("unexpected unknown result: %+v", sess.Messages[1])
	}
	if Recover(sess) {
		t.Fatal("recovery should be idempotent")
	}
}

func TestSessionDisplayTitleAndPersistence(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "titled", Messages: []Message{{Role: llm.RoleUser, Content: "Migrate the API"}}}
	if got := sess.DisplayTitle(); got != "Migrate the API" {
		t.Fatalf("fallback title = %q", got)
	}
	if err := store.Update("titled", func(current *Session) error {
		current.Title = "API migration"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("titled")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "API migration" || loaded.DisplayTitle() != "API migration" {
		t.Fatalf("unexpected title: %+v", loaded)
	}
}

func TestDeleteRemovesSession(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&Session{ID: "delete-me"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("delete-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("delete-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "delete-me.json")); !os.IsNotExist(err) {
		t.Fatalf("session file still exists: %v", err)
	}
}

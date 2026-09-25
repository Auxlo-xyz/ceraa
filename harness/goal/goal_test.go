package goal

import "testing"

func TestGoalStoreLifecycle(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.Load("session"); err != nil || exists {
		t.Fatalf("load empty: exists=%t err=%v", exists, err)
	}
	value := Goal{Objective: "finish the migration", Status: StatusActive, TokenBudget: 100}
	if err := store.Save("session", value); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Load("session")
	if err != nil || !exists || loaded.Objective != value.Objective {
		t.Fatalf("loaded=%+v exists=%t err=%v", loaded, exists, err)
	}
	loaded.Status = StatusCompleted
	if err := store.Save("session", loaded); err != nil {
		t.Fatal(err)
	}
	loaded, _, _ = store.Load("session")
	if loaded.Status != StatusCompleted {
		t.Fatalf("status=%s", loaded.Status)
	}
}

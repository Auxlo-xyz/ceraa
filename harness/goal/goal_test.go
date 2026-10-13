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
	if _, err := store.Save("session", value); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Load("session")
	if err != nil || !exists || loaded.Objective != value.Objective {
		t.Fatalf("loaded=%+v exists=%t err=%v", loaded, exists, err)
	}
	loaded.Status = StatusCompleted
	if _, err := store.Save("session", loaded); err != nil {
		t.Fatal(err)
	}
	loaded, _, _ = store.Load("session")
	if loaded.Status != StatusCompleted {
		t.Fatalf("status=%s", loaded.Status)
	}
}

func TestGoalCarriesAPlan(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value := Goal{
		Objective: "cut token cost",
		Status:    StatusActive,
		Todos: []Todo{
			{Content: "measure baseline", Status: TodoCompleted, Priority: "high"},
			{Content: "ship the cache fix", Status: TodoInProgress, Priority: "medium"},
			{Content: "drop the flaky probe", Status: TodoCancelled, Priority: "low"},
		},
	}
	if _, err := store.Save("session", value); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.Load("session")
	if err != nil || !exists {
		t.Fatalf("exists=%t err=%v", exists, err)
	}
	if len(loaded.Todos) != 3 {
		t.Fatalf("plan lost on round trip: %+v", loaded.Todos)
	}
	if Render(nil) != "No items." {
		t.Fatalf("empty render: %q", Render(nil))
	}
	want := "OK [completed] measure baseline (high)\n" +
		">> [in_progress] ship the cache fix (medium)\n" +
		"-- [cancelled] drop the flaky probe (low)"
	if got := Render(loaded.Todos); got != want {
		t.Fatalf("render:\ngot  %q\nwant %q", got, want)
	}
}

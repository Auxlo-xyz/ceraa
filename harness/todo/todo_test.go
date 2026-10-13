package todo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ceraa/harness/goal"
)

// A plan write that clobbers its goal would trade one bug for another: the
// objective and status have to survive being saved onto.
func TestPlanIsCarriedByTheGoalRecord(t *testing.T) {
	dir := t.TempDir()
	goals, err := goal.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := goals.Save("s1", goal.Goal{Objective: "ship the merge", Status: goal.StatusActive}); err != nil {
		t.Fatal(err)
	}

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := []Todo{{Content: "step one", Status: goal.TodoPending, Priority: "high"}}
	if err := store.Save("s1", plan); err != nil {
		t.Fatal(err)
	}

	current, exists, err := goals.Load("s1")
	if err != nil || !exists {
		t.Fatalf("goal after plan write: exists=%t err=%v", exists, err)
	}
	if current.Objective != "ship the merge" || current.Status != goal.StatusActive {
		t.Fatalf("plan write changed the goal: %+v", current)
	}
	if len(current.Todos) != 1 || current.Todos[0].Content != "step one" {
		t.Fatalf("plan not stored on the goal: %+v", current.Todos)
	}

	loaded, err := store.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Content != "step one" {
		t.Fatalf("load: %+v", loaded)
	}
}

// Sessions written before the plan moved to the goal keep their plan, and the
// sidecar is retired on adoption so exactly one plan can ever be read.
func TestALegacyPlanIsAdoptedOntoTheGoal(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "s2.todos.json")
	legacy := []Todo{{Content: "from before the merge", Status: goal.TodoCompleted, Priority: "low"}}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("s2")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Content != "from before the merge" {
		t.Fatalf("legacy plan not visible: %+v", loaded)
	}

	goals, err := goal.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	current, exists, err := goals.Load("s2")
	if err != nil || !exists {
		t.Fatalf("adopted goal: exists=%t err=%v", exists, err)
	}
	if len(current.Todos) != 1 || current.Todos[0].Content != "from before the merge" {
		t.Fatalf("legacy plan not adopted: %+v", current)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy sidecar still present, so two plans can be read: %v", err)
	}
}

func TestAnEmptySessionHasNoPlan(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("nothing-here")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected no plan, got %+v", loaded)
	}
}

func TestUpdateFlipsOneItem(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := []Todo{
		{Content: "first", Status: goal.TodoPending, Priority: "high"},
		{Content: "second", Status: goal.TodoPending, Priority: "low"},
	}
	if err := store.Save("s3", plan); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("s3", "second", goal.TodoCompleted); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("s3")
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].Status != goal.TodoPending || loaded[1].Status != goal.TodoCompleted {
		t.Fatalf("statuses after update: %+v", loaded)
	}
}

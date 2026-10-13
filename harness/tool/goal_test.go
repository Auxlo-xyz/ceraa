package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/goal"
	"ceraa/harness/operation"
	"ceraa/harness/todo"
)

// create_goal answered with "created_at":"0001-01-01T00:00:00Z" while get_goal
// immediately after showed the real time, because encodeGoal encoded the
// caller's copy and Save had filled the timestamps in on its own parameter by
// value. The file on disk was right; the reply was not.
func TestCreateGoalReportsTheTimestampsItPersisted(t *testing.T) {
	dir := t.TempDir()
	result, err := runGoal(operation.Spec{
		Type: "create_goal",
		Args: map[string]any{"goal_dir": dir, "session_id": "s", "objective": "ship it"},
	}, "create")
	if err != nil {
		t.Fatal(err)
	}
	var got goal.Goal
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("create_goal reply is not a goal: %v (%s)", err, result)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("create_goal reported a zeroed created_at: %s", result)
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("create_goal reported a zeroed updated_at: %s", result)
	}
	// And the reply has to agree with what the store actually holds, not merely
	// with a plausible time.
	store, err := goal.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, exists, err := store.Load("s")
	if err != nil || !exists {
		t.Fatalf("load: %v exists=%v", err, exists)
	}
	if !stored.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("reply created_at %v, store %v", got.CreatedAt, stored.CreatedAt)
	}
}

// The plan moved onto the goal when todowrite was retired into update_goal.
// What has to hold: a plan-only call writes the plan, leaves the status and the
// reason it never mentioned alone, and get_goal reads the plan back.
func TestUpdateGoalCarriesThePlan(t *testing.T) {
	dir := t.TempDir()
	args := func(extra map[string]any) map[string]any {
		base := map[string]any{"goal_dir": dir, "session_id": "s"}
		for key, value := range extra {
			base[key] = value
		}
		return base
	}
	update := func(extra map[string]any) (goal.Goal, error) {
		reply, err := runGoal(operation.Spec{Type: "update_goal", Args: args(extra)}, "update")
		if err != nil {
			return goal.Goal{}, err
		}
		var got goal.Goal
		if err := json.Unmarshal([]byte(reply), &got); err != nil {
			return goal.Goal{}, err
		}
		return got, nil
	}
	get := func() goal.Goal {
		reply, err := runGoal(operation.Spec{Type: "get_goal", Args: args(nil)}, "get")
		if err != nil {
			t.Fatal(err)
		}
		var got goal.Goal
		if err := json.Unmarshal([]byte(reply), &got); err != nil {
			t.Fatalf("get_goal reply is not a goal: %v (%s)", err, reply)
		}
		return got
	}

	if _, err := runGoal(operation.Spec{Type: "create_goal", Args: args(map[string]any{"objective": "ship it"})}, "create"); err != nil {
		t.Fatal(err)
	}
	if _, err := update(map[string]any{"status": "blocked", "reason": "waiting on a key"}); err != nil {
		t.Fatal(err)
	}

	written, err := update(map[string]any{"todos": []map[string]any{
		{"content": "wire it", "status": "in_progress", "priority": "high"},
		{"content": "test it", "status": "pending", "priority": "medium"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Todos) != 2 {
		t.Fatalf("the reply carries %d plan items, want 2", len(written.Todos))
	}
	if written.Status != goal.StatusBlocked || written.Reason != "waiting on a key" {
		t.Errorf("a plan-only update changed the goal: %s %q", written.Status, written.Reason)
	}
	if got := get(); len(got.Todos) != 2 || got.Todos[0].Content != "wire it" {
		t.Errorf("get_goal did not read the plan back: %+v", got.Todos)
	}

	// A status change states its own reason and drops the one it replaces, but
	// takes the plan with it rather than leaving a completed goal planned.
	if _, err := update(map[string]any{"status": "active"}); err != nil {
		t.Fatal(err)
	}
	after := get()
	if after.Reason != "" {
		t.Errorf("reason survived a status change that gave none: %q", after.Reason)
	}
	if len(after.Todos) != 2 {
		t.Errorf("a status change dropped the plan: %+v", after.Todos)
	}
}

// An update that carries no field would answer with a success that changed
// nothing, which is the failure a required status used to prevent.
func TestUpdateGoalNeedsSomethingToDo(t *testing.T) {
	tool := &UpdateGoalTool{}
	if _, err := tool.Translate(map[string]any{}); err == nil {
		t.Error("an empty update_goal was accepted")
	}
	statusOnly, err := tool.Translate(map[string]any{"status": "paused"})
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := statusOnly.Args["todos"]; sent {
		t.Error("a status-only update sent the plan key, which would clear the plan")
	}
	planOnly, err := tool.Translate(map[string]any{"todos": []any{
		map[string]any{"content": "x", "status": "pending", "priority": "low"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := planOnly.Args["todos"]; !sent {
		t.Error("the plan did not reach the spec")
	}
}

// A session that crosses the merge still has its plan in the old sidecar file.
// The dashboard adopts it, so get_goal has to adopt it too or the two views of
// one session disagree about what the plan is.
func TestGetGoalAdoptsThePreMergePlan(t *testing.T) {
	dir := t.TempDir()
	if _, err := runGoal(operation.Spec{
		Type: "create_goal",
		Args: map[string]any{"goal_dir": dir, "session_id": "s", "objective": "ship it"},
	}, "create"); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "s.todos.json")
	legacy := `[{"content":"write it","status":"pending","priority":"high"}]`
	if err := os.WriteFile(sidecar, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	reply, err := runGoal(operation.Spec{
		Type: "get_goal",
		Args: map[string]any{"goal_dir": dir, "session_id": "s"},
	}, "get")
	if err != nil {
		t.Fatal(err)
	}
	var got goal.Goal
	if err := json.Unmarshal([]byte(reply), &got); err != nil {
		t.Fatalf("get_goal reply is not a goal: %v (%s)", err, reply)
	}
	if len(got.Todos) != 1 || got.Todos[0].Content != "write it" {
		t.Errorf("the pre-merge plan was not adopted: %+v", got.Todos)
	}
	if got.Objective != "ship it" {
		t.Errorf("adoption disturbed the goal: %q", got.Objective)
	}
}

// todowrite has to create the goal record to carry the plan, so a session that
// planned but never set an objective holds a record with an empty objective and
// status active. Handing that raw record to get_goal reads as a goal that is
// running with nothing to do, and the model skips create_goal to repair a state
// that is not broken.
func TestGetGoalOnAPlanOnlySessionSaysNoGoal(t *testing.T) {
	dir := t.TempDir()
	store, err := todo.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("s", []todo.Todo{
		{Content: "map the diff", Status: goal.TodoPending, Priority: "high"},
		{Content: "run the tests", Status: goal.TodoPending, Priority: "medium"},
	}); err != nil {
		t.Fatal(err)
	}

	reply, err := runGoal(operation.Spec{Type: "get_goal", Args: map[string]any{"goal_dir": dir, "session_id": "s"}}, "get")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "No goal has been set yet") {
		t.Errorf("a plan-only session did not say there is no goal: %s", reply)
	}
	if !strings.Contains(reply, "map the diff") {
		t.Errorf("the plan did not come back: %s", reply)
	}
	if strings.Contains(reply, `"status"`) {
		t.Errorf("a plan-only session was presented as a goal record: %s", reply)
	}

	// A session with neither goal nor plan keeps the old answer.
	empty, err := runGoal(operation.Spec{Type: "get_goal", Args: map[string]any{"goal_dir": t.TempDir(), "session_id": "s"}}, "get")
	if err != nil {
		t.Fatal(err)
	}
	if empty != "No persistent goal exists." {
		t.Errorf("an empty session answered %q", empty)
	}
}

// create_goal used to keep the plan when the goal was active and drop it when
// the goal was paused, whatever the objective said: a new objective inherited
// the old checklist, and re-declaring a paused one lost the plan silently.
// What has to hold is the rule itself: the plan follows its objective.
func TestCreateGoalGivesThePlanToItsOwnObjective(t *testing.T) {
	dir := t.TempDir()
	spec := func(action string, extra map[string]any) operation.Spec {
		args := map[string]any{"goal_dir": dir, "session_id": "s"}
		for key, value := range extra {
			args[key] = value
		}
		return operation.Spec{Type: action, Args: args}
	}
	create := func(objective string) {
		t.Helper()
		if _, err := runGoal(spec("create_goal", map[string]any{"objective": objective}), "create"); err != nil {
			t.Fatal(err)
		}
	}
	plan := func() goal.Goal {
		t.Helper()
		reply, err := runGoal(spec("get_goal", nil), "get")
		if err != nil {
			t.Fatal(err)
		}
		var got goal.Goal
		if err := json.Unmarshal([]byte(reply), &got); err != nil {
			t.Fatalf("get_goal reply is not a goal: %v (%s)", err, reply)
		}
		return got
	}
	writePlan := func() {
		t.Helper()
		if _, err := runGoal(spec("update_goal", map[string]any{"todos": []map[string]any{
			{"content": "wire it", "status": "pending", "priority": "high"},
			{"content": "test it", "status": "pending", "priority": "medium"},
		}}), "update"); err != nil {
			t.Fatal(err)
		}
	}

	create("first objective")
	writePlan()

	// Re-declaring the same objective is how a run resumes: the plan stays.
	create("first objective")
	if got := plan(); len(got.Todos) != 2 {
		t.Fatalf("re-declaring the same objective dropped the plan: %+v", got.Todos)
	}

	// Pausing and re-creating that same objective is a resume too.
	if _, err := runGoal(spec("update_goal", map[string]any{"status": "paused"}), "update"); err != nil {
		t.Fatal(err)
	}
	create("first objective")
	if got := plan(); len(got.Todos) != 2 || got.Status != goal.StatusActive {
		t.Fatalf("resuming a paused goal lost the plan or the status: %+v", got)
	}

	// A genuinely new objective starts with its own plan, not the old one's.
	create("second objective")
	got := plan()
	if got.Objective != "second objective" {
		t.Errorf("objective = %q, want second objective", got.Objective)
	}
	if len(got.Todos) != 0 {
		t.Errorf("the new objective inherited the old checklist: %+v", got.Todos)
	}
}

// The order a model works in is often plan first, objective second: todowrite
// runs before create_goal. The record holding that plan has no objective yet,
// and creating one has to adopt the plan rather than start by wiping it.
func TestCreateGoalAdoptsAPlanWrittenBeforeAnyObjective(t *testing.T) {
	dir := t.TempDir()
	store, err := todo.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("s", []todo.Todo{{Content: "map the diff", Status: goal.TodoPending, Priority: "high"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := runGoal(operation.Spec{Type: "create_goal", Args: map[string]any{"goal_dir": dir, "session_id": "s", "objective": "ship it"}}, "create"); err != nil {
		t.Fatal(err)
	}
	reply, err := runGoal(operation.Spec{Type: "get_goal", Args: map[string]any{"goal_dir": dir, "session_id": "s"}}, "get")
	if err != nil {
		t.Fatal(err)
	}
	var got goal.Goal
	if err := json.Unmarshal([]byte(reply), &got); err != nil {
		t.Fatalf("get_goal reply is not a goal: %v (%s)", err, reply)
	}
	if got.Objective != "ship it" {
		t.Errorf("objective = %q, want ship it", got.Objective)
	}
	if len(got.Todos) != 1 || got.Todos[0].Content != "map the diff" {
		t.Errorf("creating the goal wiped the plan written before it: %+v", got.Todos)
	}
}

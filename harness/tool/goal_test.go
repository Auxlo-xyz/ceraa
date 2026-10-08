package tool

import (
	"encoding/json"
	"testing"

	"ceraa/harness/goal"
	"ceraa/harness/operation"
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

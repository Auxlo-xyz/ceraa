package todo

import (
	"encoding/json"
	"os"
	"path/filepath"

	"ceraa/harness/goal"
)

// Todo is the plan item. The record lives on the goal now, so the type is the
// goal's: this alias keeps every existing caller compiling against it.
type Todo = goal.Todo

// Store reads and writes the plan carried by the goal record.
//
// The plan used to live in its own <id>.todos.json sidecar, which nothing ever
// read back to the model: a todowrite answered with a count, and the plan was
// invisible from then on. Carrying it on the goal makes get_goal the single
// place a plan is both written and read. The pre-merge sidecar is adopted into
// the goal on first access, so sessions that predate the move keep their plan.
type Store struct {
	goals *goal.Store
	dir   string
}

func New(dir string) (*Store, error) {
	store, err := goal.New(dir)
	if err != nil {
		return nil, err
	}
	return &Store{goals: store, dir: dir}, nil
}

func (s *Store) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".todos.json")
}

// readLegacy loads the pre-merge sidecar. A missing file is not an error: it
// means the session has no plan from before the move.
func (s *Store) readLegacy(sessionID string) ([]Todo, error) {
	b, err := os.ReadFile(s.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var todos []Todo
	if err := json.Unmarshal(b, &todos); err != nil {
		return nil, err
	}
	return todos, nil
}

// Load returns the plan for the session.
func (s *Store) Load(sessionID string) ([]Todo, error) {
	current, exists, err := s.goals.Load(sessionID)
	if err != nil {
		return nil, err
	}
	if exists && len(current.Todos) > 0 {
		return current.Todos, nil
	}
	legacy, err := s.readLegacy(sessionID)
	if err != nil {
		return nil, err
	}
	if len(legacy) == 0 {
		return nil, nil
	}
	// Adopt the legacy plan onto the goal, preserving every other field the goal
	// already carries. Serving the legacy list without saving would leave two
	// readable plans, which is the split this move exists to close.
	current.Todos = legacy
	if _, err := s.goals.Save(sessionID, current); err != nil {
		return legacy, nil
	}
	_ = os.Remove(s.path(sessionID))
	return legacy, nil
}

// Save persists the plan onto the goal record, leaving the objective, status and
// counters untouched.
func (s *Store) Save(sessionID string, todos []Todo) error {
	current, _, err := s.goals.Load(sessionID)
	if err != nil {
		return err
	}
	current.Todos = todos
	_, err = s.goals.Save(sessionID, current)
	return err
}

// Update sets a single item status, matched by content.
func (s *Store) Update(sessionID, content, status string) error {
	todos, err := s.Load(sessionID)
	if err != nil {
		return err
	}
	for i := range todos {
		if todos[i].Content == content {
			todos[i].Status = status
			return s.Save(sessionID, todos)
		}
	}
	return nil
}

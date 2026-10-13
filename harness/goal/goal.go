package goal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusBlocked   Status = "blocked"
)

// A plan item. The goal carries its own plan so the model can read back what it
// committed to: a list that is only ever written is indistinguishable from a list
// nobody keeps.
type Todo struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
	TodoCancelled  = "cancelled"
)

// ParseTodos decodes the plan argument a tool carries, whatever JSON shape it
// arrived in. One decoder for both writers keeps the legacy todowrite path and
// update_goal agreeing on what a valid item is.
func ParseTodos(value any) ([]Todo, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var todos []Todo
	if err := json.Unmarshal(data, &todos); err != nil {
		return nil, err
	}
	return todos, nil
}

type Goal struct {
	Objective   string    `json:"objective"`
	Todos       []Todo    `json:"todos,omitempty"`
	Status      Status    `json:"status"`
	TokenBudget int       `json:"token_budget,omitempty"`
	TokensUsed  int       `json:"tokens_used,omitempty"`
	Iterations  int       `json:"iterations,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Store struct {
	mu  sync.Mutex
	dir string
}

func New(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "sessions"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".goal.json")
}

func (s *Store) Load(sessionID string) (Goal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return Goal{}, false, nil
		}
		return Goal{}, false, err
	}
	var value Goal
	if err := json.Unmarshal(data, &value); err != nil {
		return Goal{}, false, fmt.Errorf("decode goal: %w", err)
	}
	return value, true, nil
}

// Save persists the goal and returns what it persisted, with the timestamps the
// store filled in. Returning the value matters because value is a parameter: a
// caller that encodes its own copy after Save reports a zeroed created_at,
// which is what create_goal answered with while the file on disk was right.
func (s *Store) Save(sessionID string, value Goal) (Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value.Status == "" {
		value.Status = StatusActive
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now()
	}
	value.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return Goal{}, err
	}
	path := s.path(sessionID)
	tmp, err := os.CreateTemp(s.dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return Goal{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Goal{}, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return Goal{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Goal{}, err
	}
	if err := tmp.Close(); err != nil {
		return Goal{}, err
	}
	if err := os.Rename(name, path); err != nil {
		return Goal{}, err
	}
	return value, nil
}

func (s *Store) Clear(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Render lays a plan out as readable lines, for a reply that carries the plan
// itself. A bare count would leave the model unable to see what it set.
func Render(todos []Todo) string {
	if len(todos) == 0 {
		return "No items."
	}
	var b strings.Builder
	for _, t := range todos {
		mark := "  "
		switch t.Status {
		case TodoInProgress:
			mark = ">>"
		case TodoCompleted:
			mark = "OK"
		case TodoCancelled:
			mark = "--"
		}
		fmt.Fprintf(&b, "%s [%s] %s (%s)\n", mark, t.Status, t.Content, t.Priority)
	}
	return strings.TrimRight(b.String(), "\n")
}

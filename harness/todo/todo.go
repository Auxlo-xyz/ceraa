package todo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Todo maps to soraacode agent/core/src/session/todo.ts persisted TodoWrite
type Todo struct {
	Content  string `json:"content"`
	Status   string `json:"status"` // pending, in_progress, completed, cancelled
	Priority string `json:"priority"`
}

type Store struct {
	mu  sync.Mutex
	dir string
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".todos.json")
}

// Load returns todos for session
func (s *Store) Load(sessionID string) ([]Todo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return []Todo{}, nil
		}
		return nil, err
	}
	var todos []Todo
	if err := json.Unmarshal(b, &todos); err != nil {
		return nil, err
	}
	return todos, nil
}

// Save persists todos, maps to soraacode TodoWrite tool
func (s *Store) Save(sessionID string, todos []Todo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(todos, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(sessionID), b, 0o644)
}

// Update sets single todo status, like soraacode interview flow
func (s *Store) Update(sessionID string, content, status string) error {
	todos, err := s.Load(sessionID)
	if err != nil {
		return err
	}
	for i, t := range todos {
		if t.Content == content {
			todos[i].Status = status
			return s.Save(sessionID, todos)
		}
	}
	return nil
}

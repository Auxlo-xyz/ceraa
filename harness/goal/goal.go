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

type Goal struct {
	Objective   string    `json:"objective"`
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

func (s *Store) Save(sessionID string, value Goal) error {
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
		return err
	}
	path := s.path(sessionID)
	tmp, err := os.CreateTemp(s.dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
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

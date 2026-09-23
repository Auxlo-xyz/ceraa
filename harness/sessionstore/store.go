// Package sessionstore persists session history and operation state.
// Each session is an append-only JSONL file that can be replayed on restart.
// Fork support creates a new session by copying history up to a turn boundary.
package sessionstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"sooraa/harness/flock"
	"sooraa/harness/llm"
)

const schemaVersion = 2

type OperationStatus string

const (
	StatusPending   OperationStatus = "pending"
	StatusRunning   OperationStatus = "running"
	StatusCompleted OperationStatus = "completed"
	StatusFailed    OperationStatus = "failed"
	StatusCanceled  OperationStatus = "canceled"
)

type Operation struct {
	ID       string          `json:"id"`
	Tool     string          `json:"tool"`
	Args     map[string]any  `json:"args,omitempty"`
	Status   OperationStatus `json:"status"`
	Output   string          `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
	ThreadID string          `json:"thread_id,omitempty"`
}

type Message struct {
	Role       llm.Role        `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolResult string          `json:"tool_result,omitempty"`
	ToolCall   *ToolCallRef    `json:"tool_call,omitempty"`
	ToolCalls  []ToolCallRef   `json:"tool_calls,omitempty"`
	Reasoning  string          `json:"reasoning,omitempty"`
	Compaction *CompactionInfo `json:"compaction,omitempty"`
}

type CompactionInfo struct {
	Summary string `json:"summary"`
	Recent  string `json:"recent,omitempty"`
	Seq     int    `json:"seq"`
}

type ToolCallRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"arguments"`
}

func (m Message) AllToolCalls() []ToolCallRef {
	if len(m.ToolCalls) > 0 {
		return m.ToolCalls
	}
	if m.ToolCall != nil {
		return []ToolCallRef{*m.ToolCall}
	}
	return nil
}

type Session struct {
	ID         string      `json:"id"`
	Messages   []Message   `json:"messages"`
	Operations []Operation `json:"operations"`
	Version    int         `json:"version"`
	ParentID   string      `json:"parent_id,omitempty"`
}

func (s *Session) Clear() error {
	s.Messages = []Message{}
	s.Operations = []Operation{}
	return nil
}

type Store struct {
	mu       sync.Mutex
	dir      string
	fileLock *flock.Lock
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *Store) Load(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return nil, err
	}
	defer s.unlock()
	return s.loadUnlocked(id)
}

func (s *Store) loadUnlocked(id string) (*Session, error) {
	path := s.path(id)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Session{ID: id, Version: schemaVersion}, nil
		}
		return nil, fmt.Errorf("read session %s: %w", id, err)
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, fmt.Errorf("decode session %s: %w", id, err)
	}
	if sess.Version == 0 {
		sess.Version = schemaVersion
	}
	return &sess, nil
}

func (s *Store) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return err
	}
	defer s.unlock()
	return s.saveUnlocked(sess)
}

func (s *Store) saveUnlocked(sess *Session) error {
	if sess.Version == 0 {
		sess.Version = schemaVersion
	}
	b, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %s: %w", sess.ID, err)
	}
	return atomicWriteFile(s.path(sess.ID), b, 0o644)
}

func (s *Store) lock() error {
	lockPath := filepath.Join(s.dir, ".lock")
	lock, err := flock.Acquire(lockPath)
	if err != nil {
		return fmt.Errorf("lock session store: %w", err)
	}
	s.fileLock = lock
	return nil
}

func (s *Store) unlock() {
	if s.fileLock != nil {
		_ = s.fileLock.Release()
		s.fileLock = nil
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp session: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp session: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace session: %w", err)
	}
	removeTemp = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func (s *Store) Update(id string, update func(*Session) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return err
	}
	defer s.unlock()
	sess, err := s.loadUnlocked(id)
	if err != nil {
		return err
	}
	if err := update(sess); err != nil {
		return err
	}
	return s.saveUnlocked(sess)
}

func (s *Store) Clear(id string) error {
	return s.Update(id, func(sess *Session) error {
		sess.Messages = []Message{}
		sess.Operations = []Operation{}
		return nil
	})
}

func (s *Store) List() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, filepath.Base(m[:len(m)-len(filepath.Ext(m))]))
	}
	return ids, nil
}

// Fork creates a new session copying the parent's messages up to and including
// the message at the given index. Operations are not copied.
func (s *Store) Fork(parentID string, messageIndex int) (*Session, error) {
	parent, err := s.Load(parentID)
	if err != nil {
		return nil, err
	}
	if messageIndex >= len(parent.Messages) {
		return nil, fmt.Errorf("message index %d out of range (len=%d)", messageIndex, len(parent.Messages))
	}
	childID := parentID + "-fork-" + fmt.Sprintf("%d", len(parent.Messages))
	child := &Session{
		ID:         childID,
		ParentID:   parentID,
		Version:    schemaVersion,
		Messages:   make([]Message, messageIndex+1),
		Operations: []Operation{},
	}
	copy(child.Messages, parent.Messages[:messageIndex+1])
	if err := s.Save(child); err != nil {
		return nil, err
	}
	return child, nil
}

package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot maps to soraacode agent/core/src/session/snapshot.ts + revert.ts
// Captures file state before write for undo

type Entry struct {
	Path      string    `json:"path"`
	Hash      string    `json:"hash"`
	Content   []byte    `json:"content,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Exists    bool      `json:"exists"`
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

// Capture saves current file state before mutation
func (s *Store) Capture(path string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	exists := err == nil
	var hash string
	if exists {
		h := sha256.Sum256(data)
		hash = hex.EncodeToString(h[:])
	}
	entry := &Entry{
		Path:      path,
		Hash:      hash,
		Content:   data,
		Timestamp: time.Now(),
		Exists:    exists,
	}
	// Save snapshot to dir
	name := fmt.Sprintf("%s-%d.snap", filepath.Base(path), time.Now().UnixNano())
	_ = os.WriteFile(filepath.Join(s.dir, name), data, 0o644)
	return entry, nil
}

// Revert restores file to entry state
func (s *Store) Revert(entry *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !entry.Exists {
		return os.Remove(entry.Path)
	}
	if dir := filepath.Dir(entry.Path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return os.WriteFile(entry.Path, entry.Content, 0o644)
}

// List returns recent snapshots
func (s *Store) List() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.snap"))
	if err != nil {
		return nil, err
	}
	return matches, nil
}

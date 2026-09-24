package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	exists := err == nil
	var hash string
	if exists {
		h := sha256.Sum256(data)
		hash = hex.EncodeToString(h[:])
	}
	entry := &Entry{
		Path:      abs,
		Hash:      hash,
		Content:   data,
		Timestamp: time.Now(),
		Exists:    exists,
	}
	name := fmt.Sprintf("%s-%d.snap", filepath.Base(abs), time.Now().UnixNano())
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(s.dir, name), encoded, 0o600); err != nil {
		return nil, err
	}
	return entry, nil
}

// Revert restores file to entry state
func (s *Store) Revert(entry *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !entry.Exists {
		if err := os.Remove(entry.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if dir := filepath.Dir(entry.Path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return os.WriteFile(entry.Path, entry.Content, 0o644)
}

func (s *Store) Latest(path string) (*Entry, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	matches, err := s.List()
	if err != nil {
		return nil, err
	}
	var entries []*Entry
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		var entry Entry
		if json.Unmarshal(data, &entry) != nil {
			continue
		}
		entryAbs, err := filepath.Abs(entry.Path)
		if err == nil && entryAbs == abs {
			copy := entry
			entries = append(entries, &copy)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no snapshot found for %s", abs)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.After(entries[j].Timestamp) })
	return entries[0], nil
}

func (s *Store) RevertLatest(path string) (*Entry, error) {
	entry, err := s.Latest(path)
	if err != nil {
		return nil, err
	}
	if err := s.Revert(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// List returns recent snapshots
func (s *Store) List() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.snap"))
	if err != nil {
		return nil, err
	}
	return matches, nil
}

func (s *Store) Entries() ([]Entry, error) {
	matches, err := s.List()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(matches))
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		var entry Entry
		if json.Unmarshal(data, &entry) == nil {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.After(entries[j].Timestamp) })
	return entries, nil
}

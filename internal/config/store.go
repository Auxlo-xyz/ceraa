package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ceraa/harness/flock"
	"ceraa/internal/paths"
)

// Store is the single writer of config.json.
//
// It exists because the Telegram menus and the CLI both edited the same file
// with a load, modify, save sequence and no lock, so a write from one could
// silently discard the other's change. Update makes that sequence atomic: the
// read, the edit and the write happen under one lock, against one path, so there
// is no window in which a second writer can be overwritten.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore returns a Store for the canonical config path.
func NewStore() *Store { return &Store{path: paths.ConfigFile()} }

// NewStoreAt returns a Store for an explicit path. Tests use it so a run cannot
// reach the live install's config through a package-level default.
func NewStoreAt(path string) *Store { return &Store{path: path} }

// Path is the file this Store owns.
func (s *Store) Path() string { return s.path }

// Load reads the config from this Store's path, applying environment overrides.
func (s *Store) Load() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*Config, error) {
	cfg := DefaultConfig()
	if err := loadFileAt(cfg, s.path); err != nil {
		return nil, err
	}
	applyEnv(cfg)
	return cfg, nil
}

// Update applies fn to the stored config and writes the result back.
//
// The whole cycle is one critical section, so a caller changing the active
// provider cannot interleave with a caller changing a model and lose either
// write. fn receives the config as loaded, so it must not assume a cached copy.
func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The in-memory mutex covers goroutines, not a second address space. The CLI
	// edits this file while the daemon runs, and a second daemon started by hand
	// holds the same token and the same file, so two read-modify-write cycles
	// interleaved across processes silently discard one side's change. That is
	// how a provider switch confirmed on screen never took effect for the next
	// run. The lock file serialises every holder of this path, whatever process
	// it lives in; a dead holder releases on process exit, so there is nothing
	// stale to clean up.
	lock, err := flock.Acquire(s.path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	cfg, err := s.loadLocked()
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return writeConfigAt(cfg, s.path)
}

// loadFileAt reads one specific path, leaving loadedFrom alone: a Store reports
// its own path, and the global is only meaningful for the process-wide Load.
func loadFileAt(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// writeConfigAt writes config.json atomically: a temp file in the same directory,
// then a rename. A crash mid-write leaves the previous config intact rather than
// a truncated file, which would otherwise read as an empty config and silently
// unconfigure the install.
func writeConfigAt(cfg *Config, path string) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

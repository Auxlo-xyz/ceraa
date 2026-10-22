// Package coverage records what has already been tried.
//
// A long recon loop is a model asking the same question twice because it
// cannot see what it already asked. The ledger is the memory of the sweep:
// every technique that was tried against a target, with the outcome, so the
// next question can be the one that was not asked yet.
//
// It is a record of attempts, not of findings. A technique that found
// nothing is worth remembering precisely because nothing is what it found.
package coverage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Outcome is what a technique produced.
type Outcome string

const (
	// Clean means the technique ran and found nothing.
	Clean Outcome = "clean"
	// Finding means the technique produced something worth recording.
	Finding Outcome = "finding"
	// Blocked means the technique could not run.
	Blocked Outcome = "blocked"
	// Skipped means the technique was deliberately not run.
	Skipped Outcome = "skipped"
)

// Entry is one recorded attempt.
type Entry struct {
	// Technique names what was tried: "cors", "auth_bypass", "idor" and so
	// on. The catalogue in this package is the list of names a sweep is
	// expected to cover.
	Technique string `json:"technique"`
	// Target is what it was tried against.
	Target string `json:"target"`
	// Detail says how, in one line.
	Detail string `json:"detail,omitempty"`
	// Outcome is what it produced.
	Outcome Outcome `json:"outcome"`
	// Note carries anything the operator or the model wants to remember.
	Note string `json:"note,omitempty"`
	// At is when it ran.
	At time.Time `json:"at"`
}

// Store keeps the ledger on disk.
type Store struct {
	mu  sync.Mutex
	dir string
}

// Open opens (and creates) the ledger directory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is where the ledger lives.
func (s *Store) Dir() string { return s.dir }

// path is the ledger file.
func (s *Store) path() string { return filepath.Join(s.dir, "coverage.json") }

// Record appends an attempt.
func (s *Store) Record(entry Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	entry.Technique = strings.ToLower(strings.TrimSpace(entry.Technique))
	if entry.Technique == "" {
		return Entry{}, fmt.Errorf("a coverage entry needs a technique")
	}
	if entry.Target == "" {
		return Entry{}, fmt.Errorf("a coverage entry needs a target")
	}
	if entry.Outcome == "" {
		entry.Outcome = Clean
	}
	if entry.At.IsZero() {
		entry.At = time.Now()
	}
	entries = append(entries, entry)
	if err := s.save(entries); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// List returns the ledger, newest last, optionally filtered by technique and
// by target.
func (s *Store) List(technique string, target string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return nil, err
	}
	technique = strings.ToLower(strings.TrimSpace(technique))
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if technique != "" && entry.Technique != technique {
			continue
		}
		if target != "" && !strings.Contains(entry.Target, target) {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// Tried reports the techniques that were run against a target.
func (s *Store) Tried(target string) (map[string]bool, error) {
	entries, err := s.List("", target)
	if err != nil {
		return nil, err
	}
	tried := make(map[string]bool, len(entries))
	for _, entry := range entries {
		tried[entry.Technique] = true
	}
	return tried, nil
}

// Gaps returns the catalogue techniques that were not tried against a target.
//
// The catalogue is a checklist, not a standard: it names the techniques a
// web application review is expected to cover, so a sweep that skipped half
// of them can see that it did.
func (s *Store) Gaps(target string, catalogue []string) ([]string, error) {
	tried, err := s.Tried(target)
	if err != nil {
		return nil, err
	}
	if len(catalogue) == 0 {
		catalogue = Catalogue()
	}
	gaps := make([]string, 0, len(catalogue))
	for _, technique := range catalogue {
		if !tried[technique] {
			gaps = append(gaps, technique)
		}
	}
	return gaps, nil
}

// load reads the ledger file.
func (s *Store) load() ([]Entry, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("the coverage ledger at %s could not be read: %w", s.path(), err)
	}
	return entries, nil
}

// save writes the ledger atomically.
func (s *Store) save(entries []Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// catalogue is the technique checklist.
var catalogue = []string{
	"auth_bypass",
	"idor",
	"mass_assignment",
	"privilege_escalation",
	"cors_misconfig",
	"cors_preflight",
	"csrf",
	"session_fixation",
	"cookie_flags",
	"jwt_weakness",
	"secret_exposure",
	"security_headers",
	"clickjacking",
	"host_header_injection",
	"open_redirect",
	"ssrf",
	"path_traversal",
	"local_file_inclusion",
	"remote_file_inclusion",
	"command_injection",
	"sql_injection",
	"nosql_injection",
	"xss",
	"template_injection",
	"xxe",
	"deserialization",
	"race_condition",
	"rate_limit",
	"graphql",
	"websocket",
	"api_versioning",
	"subdomain_takeover",
	"content_discovery",
	"port_scan",
	"service_enum",
	"tls_config",
	"dns_records",
}

// Catalogue returns the technique checklist, sorted.
func Catalogue() []string {
	out := append([]string(nil), catalogue...)
	sort.Strings(out)
	return out
}

package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"sooraa/harness/flock"
)

const schemaVersion = 1

type Scope string

const (
	Project Scope = "project"
	Session Scope = "session"
)

type Entry struct {
	ID        string    `json:"id"`
	Scope     Scope     `json:"scope"`
	ProjectID string    `json:"project_id"`
	SessionID string    `json:"session_id,omitempty"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	mu   sync.Mutex
	dir  string
	path string
}

type document struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create memory store: %w", err)
	}
	return &Store{dir: dir, path: filepath.Join(dir, "memory.json")}, nil
}

func (s *Store) Add(projectID, sessionID, content string) (*Entry, error) {
	projectID = strings.TrimSpace(projectID)
	sessionID = strings.TrimSpace(sessionID)
	content = compact(content)
	if projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}
	if content == "" {
		return nil, fmt.Errorf("memory content is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".memory.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock memory store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	for i := range doc.Entries {
		if sameMemory(doc.Entries[i], projectID, sessionID, content) {
			entry := doc.Entries[i]
			return &entry, nil
		}
	}

	now := time.Now().UTC()
	entry := Entry{
		ID:        memoryID(projectID, sessionID, content),
		Scope:     scopeFor(sessionID),
		ProjectID: projectID,
		SessionID: sessionID,
		Content:   content,
		CreatedAt: now,
	}
	doc.Entries = append(doc.Entries, entry)
	if err := save(s.path, doc); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (s *Store) List(projectID, sessionID string) ([]Entry, error) {
	projectID = strings.TrimSpace(projectID)
	sessionID = strings.TrimSpace(sessionID)
	if projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".memory.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock memory store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(doc.Entries))
	for _, entry := range doc.Entries {
		if entry.ProjectID != projectID {
			continue
		}
		if entry.SessionID == "" || entry.SessionID == sessionID {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func (s *Store) Remove(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("memory ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := flock.Acquire(filepath.Join(s.dir, ".memory.lock"))
	if err != nil {
		return fmt.Errorf("lock memory store: %w", err)
	}
	defer lock.Release()

	doc, err := load(s.path)
	if err != nil {
		return err
	}
	for i := range doc.Entries {
		if doc.Entries[i].ID != id {
			continue
		}
		doc.Entries = append(doc.Entries[:i], doc.Entries[i+1:]...)
		return save(s.path, doc)
	}
	return fmt.Errorf("remove memory %s: %w", id, os.ErrNotExist)
}

func (s *Store) RenderContext(projectID, sessionID string, maxBytes int) (string, error) {
	entries, err := s.List(projectID, sessionID)
	if err != nil {
		return "", err
	}
	return RenderContext(entries, maxBytes), nil
}

func RenderContext(entries []Entry, maxBytes int) string {
	if maxBytes <= 0 || len(entries) == 0 {
		return ""
	}
	const prefix = "<memory>\n"
	var out strings.Builder
	out.WriteString(prefix)
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		item := fmt.Sprintf("<memory-entry scope=%q project=%q", entry.Scope, html.EscapeString(entry.ProjectID))
		if entry.SessionID != "" {
			item += fmt.Sprintf(" session=%q", html.EscapeString(entry.SessionID))
		}
		item += ">" + html.EscapeString(entry.Content) + "</memory-entry>\n"
		if out.Len()+len(item)+len("</memory>\n") <= maxBytes {
			out.WriteString(item)
		}
	}
	out.WriteString("</memory>\n")
	if out.Len() <= maxBytes {
		return out.String()
	}
	return truncateUTF8(out.String(), maxBytes)
}

func scopeFor(sessionID string) Scope {
	if sessionID == "" {
		return Project
	}
	return Session
}

func sameMemory(entry Entry, projectID, sessionID, content string) bool {
	return entry.ProjectID == projectID && entry.SessionID == sessionID && compact(entry.Content) == content
}

func compact(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func memoryID(projectID, sessionID, content string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + sessionID + "\x00" + content))
	return hex.EncodeToString(sum[:16])
}

func load(path string) (*document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &document{Version: schemaVersion, Entries: []Entry{}}, nil
		}
		return nil, fmt.Errorf("read memory store: %w", err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode memory store: %w", err)
	}
	if doc.Version != 0 && doc.Version != schemaVersion {
		return nil, fmt.Errorf("unsupported memory store version %d", doc.Version)
	}
	if doc.Entries == nil {
		doc.Entries = []Entry{}
	}
	return &doc, nil
}

func save(path string, doc *document) error {
	doc.Version = schemaVersion
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode memory store: %w", err)
	}
	return atomicWrite(path, data)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".memory.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary memory store: %w", err)
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set memory store permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary memory store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary memory store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary memory store: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace memory store: %w", err)
	}
	remove = false
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

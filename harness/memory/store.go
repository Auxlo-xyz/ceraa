package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ceraa/harness/flock"
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
	// Seen counts how many times this exact memory has been asserted. It is the
	// corroboration count, and Add maintains it rather than treating a repeat as a
	// duplicate to discard.
	//
	// The old behaviour returned the existing entry unchanged on a repeat, so
	// re-stating something the agent already knew was worth nothing: the store
	// could not tell a preference mentioned once from one confirmed many times.
	Seen int `json:"seen,omitempty"`
	// Applies narrows this memory to a path or glob, so it can be delivered at
	// the point of use instead of on every turn.
	//
	// Empty means a standing preference that applies everywhere, which is what the
	// prompt block is for. Set means "this is about this path", and it is what
	// makes a deterministic trigger possible - the operation already knows which
	// path it is touching, so no retrieval step has to be right.
	Applies string `json:"applies,omitempty"`
	// Why is the optional rationale.
	//
	// A benchmark of four production memory systems found that preserving the
	// rationale alongside the instruction was necessary but not sufficient, and
	// that what mattered was retaining the constraints anchoring it to the target.
	// That is the reason this field exists, and the reason it is bounded at render
	// time rather than trusted to be short.
	Why string `json:"why,omitempty"`
	// SeenAt is when it was last asserted, which is what decay is measured from.
	//
	// CreatedAt is deliberately not reused for that. Decay is about how long since
	// anyone last confirmed it, not how long ago the file learned about it, and
	// conflating them would make a repeatedly confirmed old preference fade at the
	// rate of an unconfirmed new one.
	SeenAt time.Time `json:"seen_at,omitempty"`
}

// Decay and corroboration, and why the score is never stored.
//
// memoryHalfLifeDays is how long a single unconfirmed assertion takes to halve in
// weight. One constant and no config: the shape Hermes uses for this is a
// DecayConfig with an enabled flag and two dialects, and nothing here needs either,
// because the cost of being wrong is a preference that fades and can be re-asserted
// rather than a preference that is never learned.
const memoryHalfLifeDays = 30.0

// memoryScoreFloor is the weight below which an entry is not worth the prompt
// bytes. Entries fade rather than disappear, so this is what keeps a long-lived
// store from rendering its entire history forever.
//
// It is low on purpose. The failure this guards against is unbounded growth in the
// tail of the prompt, and anything genuinely relevant will be re-confirmed by the
// next time it matters, which pushes it straight back above the floor.
const memoryScoreFloor = 0.08

// corroborationWeight maps a confirmation count onto [0,1), saturating.
//
// 1 - exp(-k*n) rises steeply for the first few confirmations and then flattens, so
// the difference between once and twice counts for much more than the difference
// between nine times and ten. A preference does not become more trustworthy
// linearly with repetition, and a linear count would let a frequently-touched
// trivial note outrank a genuinely load-bearing one.
func corroborationWeight(seen int) float64 {
	if seen < 1 {
		seen = 1
	}
	return 1 - math.Exp(-0.3*float64(seen))
}

// score is one entry's weight at a point in time: corroboration discounted by age.
//
// Computed, never persisted. The store is read on every turn by Live, so a stored
// score would be a number that has to be refreshed, snapshotted and migrated;
// computing it here means it cannot be stale, and there is nothing to restore.
//
// A zero Seen means the entry predates this field. It is treated as one
// confirmation rather than zero, so an existing store keeps working unchanged
// instead of every memory falling below the floor on upgrade.
func score(entry Entry, now time.Time) float64 {
	corroboration := corroborationWeight(entry.Seen)
	if entry.SeenAt.IsZero() {
		return corroboration
	}
	ageDays := now.Sub(entry.SeenAt).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	return corroboration * math.Exp(-math.Ln2*ageDays/memoryHalfLifeDays)
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
	return s.AddScoped(projectID, sessionID, content, "", "")
}

// AddScoped adds a memory that is optionally narrowed to a path and carries a
// rationale.
//
// Add remains the two-argument form so the existing callers and tests are
// untouched; there is no reason for a caller that has no path to pass two empty
// strings to say so.
func (s *Store) AddScoped(projectID, sessionID, content, applies, why string) (*Entry, error) {
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
	now := time.Now().UTC()
	for i := range doc.Entries {
		if sameMemory(doc.Entries[i], projectID, sessionID, content) {
			// A repeat is evidence, not a duplicate. Returning early here is what
			// made a re-confirmed preference worth exactly as much as one asserted
			// once, so the count and the refresh below are the whole change.
			doc.Entries[i].Seen++
			doc.Entries[i].SeenAt = now
			if err := save(s.path, doc); err != nil {
				return nil, err
			}
			entry := doc.Entries[i]
			return &entry, nil
		}
	}

	entry := Entry{
		ID:        memoryID(projectID, sessionID, content),
		Scope:     scopeFor(sessionID),
		ProjectID: projectID,
		SessionID: sessionID,
		Content:   content,
		Applies:   strings.TrimSpace(applies),
		Why:       compact(why),
		CreatedAt: now,
		Seen:      1,
		SeenAt:    now,
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

// Confirm records that an existing memory still holds, strengthening it and
// resetting its decay.
//
// By id rather than by content on purpose. Add asserts a statement and dedups on
// exact wording, so a model asked to "confirm" something from the rendered context
// has to reproduce the stored text byte for byte to hit the same entry. Restating
// it with different punctuation creates a second, near-duplicate memory instead,
// which is the failure this method exists to make impossible.
//
// Scoped like every other write: an id from another project or session is not
// found, so a guess cannot corroborate something it cannot see.
func (s *Store) Confirm(projectID, sessionID, id string) error {
	projectID = strings.TrimSpace(projectID)
	sessionID = strings.TrimSpace(sessionID)
	id = strings.TrimSpace(id)
	if projectID == "" {
		return fmt.Errorf("project ID is required")
	}
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
		entry := &doc.Entries[i]
		if entry.ID != id {
			continue
		}
		if entry.ProjectID != projectID || (entry.SessionID != "" && entry.SessionID != sessionID) {
			return fmt.Errorf("confirm memory %s: %w", id, os.ErrNotExist)
		}
		entry.Seen++
		entry.SeenAt = time.Now().UTC()
		return save(s.path, doc)
	}
	return fmt.Errorf("confirm memory %s: %w", id, os.ErrNotExist)
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
	now := time.Now()

	// Order by weight rather than by recency, and drop what has faded below the
	// floor. Newest-first meant a store only ever forgot by being scrolled off the
	// byte budget, which is a memory that cannot expire.
	//
	// A stable sort by descending weight, so equal scores keep insertion order and
	// two memories asserted in the same turn cannot swap places between renders.
	ranked := make([]Entry, len(entries))
	copy(ranked, entries)
	sort.SliceStable(ranked, func(i, j int) bool {
		return score(ranked[i], now) > score(ranked[j], now)
	})

	var out strings.Builder
	out.WriteString(prefix)
	for _, entry := range ranked {
		if score(entry, now) < memoryScoreFloor {
			continue
		}
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

// sameMemory is identity: the project, the session, and the wording.
//
// Applies is deliberately not part of it. Two memories can share a subject and
// apply to different paths - "the build needs JAVA_HOME set" for the backend, "the
// site deploys from /srv/www" for the frontend - and folding the path in here
// would silently keep only the first one written.
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

// DefaultMemoryFile is the workspace-relative path that holds durable memory.
//
// A markdown file rather than only store entries, because memory that can be
// edited with an ordinary editor is memory that gets edited. Entries added by the
// model are still kept; the file is the part a human maintains.
const DefaultMemoryFile = "memory/AGENTS.md"

// Live returns a renderer that re-reads memory on every call.
//
// Memory used to be rendered once, at startup, into the runner config. An edit to
// the memory file then did not reach the model until the surrounding agent was
// rebuilt, which for a long-running daemon meant not at all: the file on disk and
// the memory in the prompt silently diverged, and the divergence was invisible
// because nothing logged either side.
//
// Reading per turn costs a small file read. The result sits after the static
// prompt and the date, so an edit invalidates only the varying suffix.
func Live(store *Store, projectID, sessionID, filePath string, maxBytes int) func() (string, error) {
	return func() (string, error) {
		var parts []string
		if filePath != "" {
			if rendered, err := RenderFile(filePath, maxBytes); err != nil {
				// A missing memory file is the normal case, not an error worth
				// propagating into the turn.
				if !os.IsNotExist(err) {
					return "", err
				}
			} else if rendered != "" {
				parts = append(parts, rendered)
			}
		}
		if store != nil {
			entries, err := store.List(projectID, sessionID)
			if err != nil {
				return "", err
			}
			if rendered := RenderContext(entries, remainingBytes(maxBytes, parts)); rendered != "" {
				parts = append(parts, strings.TrimSuffix(rendered, "\n"))
			}
		}
		if len(parts) == 0 {
			return "", nil
		}
		return strings.Join(parts, "\n") + "\n", nil
	}
}

func remainingBytes(maxBytes int, used []string) int {
	if maxBytes <= 0 {
		return 0
	}
	spent := 0
	for _, part := range used {
		spent += len(part)
	}
	if spent >= maxBytes {
		return 0
	}
	return maxBytes - spent
}

// RenderFile wraps a memory markdown file in the memory block, bounded by
// maxBytes. A file over the limit is truncated whole rather than dropped, so a
// large memory file degrades to a stale-but-present one instead of to nothing.
func RenderFile(path string, maxBytes int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return "", nil
	}
	const open = "<memory-file>\n"
	entry := open + html.EscapeString(body) + "\n</memory-file>\n"
	if maxBytes > 0 && len(entry) > maxBytes {
		return open + truncateUTF8(html.EscapeString(body), maxBytes-len(open)-len("\n</memory-file>\n")) + "\n</memory-file>\n", nil
	}
	return entry, nil
}

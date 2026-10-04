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

// Veracity is how a memory came to be believed, and it is weighted rather than
// filtered.
//
// The extractor already emitted a confidence of high, medium or low and Commit
// threw away everything that was not "high", so a medium inference could never be
// learned no matter how often it was re-asserted. Weighting instead of filtering
// keeps the statement and lets repetition earn it trust, which is the whole point
// of the corroboration count.
//
// The order is deliberate and it runs against the usual assumption. A tool
// observation is worth less than something the user said outright, because the
// model is the thing being uncertain about and the user is not. Tool output is
// still weighted at 0.5 rather than discarded: it is real evidence about the
// world, and a fact the agent verified itself is often better than one it inferred
// from conversation.
//
// The values are a shape, not a measurement. They come from a different codebase
// on a different model, and nothing here has benchmarked them. What is claimed is
// only that the ordering is right.
type Veracity string

const (
	// Stated is what the user said outright. The strongest thing in the store.
	Stated Veracity = "stated"
	// Inferred is the extractor's read of a conversation that did not say so
	// directly. The extractor's most common output.
	Inferred Veracity = "inferred"
	// Tool is an observation the agent made for itself: a test result, a command
	// output, a file it read.
	Tool Veracity = "tool"
	// Imported was handed to us rather than learned here, such as a memory block
	// committed from an earlier session or written into memory/AGENTS.md.
	Imported Veracity = "imported"
	// Unknown is an entry that predates this field. It is deliberately mid-range:
	// treating it as stated would promote every existing memory on upgrade, and
	// treating it as inferred would sink a store full of good ones.
	Unknown Veracity = "unknown"
)

// veracityWeight maps a provenance onto [0,1]. Frozen because it is a table of
// judgement, not a calculation, and a caller that could mutate it would make every
// score in the store depend on evaluation order.
var veracityWeight = map[Veracity]float64{
	Stated:   1.0,
	Inferred: 0.7,
	Tool:     0.5,
	Imported: 0.6,
	Unknown:  0.8,
}

// Weight is the provenance's share of an entry's score, in [0,1].
//
// An unset or unrecognised provenance reads as Unknown rather than as zero, so a
// hand-written memory.json entry and a store written by an older build both keep
// scoring instead of vanishing below the floor on upgrade.
func (v Veracity) Weight() float64 {
	if weight, ok := veracityWeight[v]; ok {
		return weight
	}
	return veracityWeight[Unknown]
}

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
	// Veracity is how this memory came to be believed.
	//
	// Absent on entries written before this field, and read as Unknown rather than
	// as zero: a store that predates provenance has real preferences in it, and
	// scoring them all at zero would silently empty everyone's prompt block on the
	// upgrade that introduced this.
	Veracity Veracity `json:"veracity,omitempty"`
	// Type is what kind of claim this is, and it sets the half-life.
	//
	// Absent on entries written before this field, and read as the old single
	// half-life rather than as anything typed, so an upgrade changes nothing about
	// how an existing store decays.
	Type MemoryType `json:"type,omitempty"`
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
// MemoryType is what kind of thing a memory is, which decides how long it lasts.
//
// One decay constant for everything meant a stated preference faded at exactly the
// rate of a one-off note, so the things most worth remembering were the first to be
// forgotten. A profile and a request are not the same kind of claim and must not
// share a lifetime.
type MemoryType string

const (
	// Preference is how the user wants work done. The bulk of what this store
	// holds, and the thing that was previously being under-protected.
	Preference MemoryType = "preference"
	// Profile is who the user is and how they work: role, conventions, standing
	// constraints. Changes rarely and should not need re-stating.
	Profile MemoryType = "profile"
	// Instruction is a standing directive, phrased as one: "never force push".
	Instruction MemoryType = "instruction"
	// Decision is a choice with a reason that still holds.
	Decision MemoryType = "decision"
	// Goal is something the user is working toward, which expires on its own terms.
	Goal MemoryType = "goal"
	// Constraint is a limit that applies until lifted.
	Constraint MemoryType = "constraint"
	// Request is a one-off task. Deliberately the shortest life of all: these are
	// what should be forgotten first, and were previously kept the longest.
	Request MemoryType = "request"
	// Observation is something noticed once, about the state of the world.
	Observation MemoryType = "observation"
	// Error is a mistake worth not repeating.
	Error MemoryType = "error"
	// Context is a fact about the project or codebase.
	Context MemoryType = "context"
)

// memoryHalfLifeDays is the half-life for an entry with no recognised type.
//
// It is the old single constant, kept as the fallback so an unclassifiable memory
// behaves exactly as it did before typed decay existed rather than silently
// adopting somebody else's lifetime.
const memoryHalfLifeDays = 30.0

// halfLifeDaysByType is how long each type takes to halve in weight, in days.
//
// The spread is the whole point and it is deliberately wide. A profile outlives a
// request by two and a half orders of magnitude, because one is a standing fact
// about a person and the other is a thing that was asked for once.
//
// The numbers are ordered by how often the underlying claim stops being true, not
// by how important it is. That is the distinction that matters: a goal is important
// and short-lived, a preference is ordinary and long-lived, and ranking by
// importance would have got both backwards.
var halfLifeDaysByType = map[MemoryType]float64{
	Profile:     3650, // ten years
	Instruction: 1825, // five
	Preference:  1095, // three
	Decision:    730,  // two
	Constraint:  730,  // two
	Goal:        365,  // one
	Context:     365,  // one
	Error:       270,
	Observation: 120,
	Request:     45, // six weeks
}

// halfLifeDays is the half-life for an entry's type.
//
// An unset or unrecognised type reads as the old single constant, so every store in
// the field keeps decaying exactly as it did before this existed and nothing is
// silently promoted to a lifetime nobody chose.
func halfLifeDays(entry Entry) float64 {
	if halfLife, ok := halfLifeDaysByType[entry.Type]; ok {
		return halfLife
	}
	return memoryHalfLifeDays
}

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

// score is one entry's weight at a point in time: provenance, then corroboration,
// then age.
//
// Computed, never persisted. The store is read on every turn by Live, so a stored
// score would be a number that has to be refreshed, snapshotted and migrated;
// computing it here means it cannot be stale, and there is nothing to restore.
//
// A zero Seen means the entry predates this field. It is treated as one
// confirmation rather than zero, so an existing store keeps working unchanged
// instead of every memory falling below the floor on upgrade.
//
// Provenance is the ceiling and corroboration is the climb, not the other way
// round. A hard multiplier would make an inferred memory that was re-asserted
// ninety times permanently outrank something the user stated once, which is
// backwards: repetition is evidence the statement is right, so it should be able
// to lift an entry toward the provenance's ceiling and stop there.
func score(entry Entry, now time.Time) float64 {
	ceiling := entry.Veracity.Weight()
	corroboration := corroborationWeight(entry.Seen)
	// Corroboration scales the ceiling rather than interpolating toward it. An
	// earlier version took ceiling*(1-(1-c)*0.5), which let an inference converge
	// on its 0.7 ceiling and overtake a stated memory sitting at 0.63 on a single
	// confirmation: forty re-assertions of a guess outranked the user, which is
	// the inversion this weighting exists to prevent. Scaling keeps the ranking
	// total, so whatever one provenance can reach a weaker one can never pass by
	// being repeated more often.
	earned := ceiling * corroboration
	if entry.SeenAt.IsZero() {
		return earned
	}
	ageDays := now.Sub(entry.SeenAt).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	return earned * math.Exp(-math.Ln2*ageDays/halfLifeDays(entry))
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
	return s.AddTyped(projectID, sessionID, content, "", "", Unknown, "")
}

// AddScoped adds a memory that is optionally narrowed to a path, carries a
// rationale, and records how it came to be believed. Its type is inferred.
func (s *Store) AddScoped(projectID, sessionID, content, applies, why string, veracity Veracity) (*Entry, error) {
	return s.AddTyped(projectID, sessionID, content, applies, why, veracity, "")
}

// AddTyped is AddScoped with an explicit memory type, for a caller that already
// knows what kind of claim it is making.
//
// An empty memoryType is inferred from the statement rather than defaulted, because
// defaulting would mean every entry that did not name its type shared one lifetime
// and typed decay would only ever apply to the minority that opted in.
func (s *Store) AddTyped(projectID, sessionID, content, applies, why string, veracity Veracity, memoryType MemoryType) (*Entry, error) {
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
			// Provenance only ever rises on a repeat. A user restating a preference
			// they had once only inferred is the user confirming it, and the strongest
			// provenance they can give; letting a later low-trust re-assert demote an
			// entry the user has since vouched for would be the exact inversion
			// weighting exists to prevent.
			doc.Entries[i].Veracity = strongerVeracity(doc.Entries[i].Veracity, veracity)
			// The type is never re-derived on a repeat. The statement has not
			// changed, so a classifier run twice would only risk flipping a
			// classified memory to a different lifetime for no new information.
			if _, known := halfLifeDaysByType[doc.Entries[i].Type]; !known {
				doc.Entries[i].Type = resolveMemoryType(doc.Entries[i].Content, memoryType)
			}
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
		Veracity:  normalizeVeracity(veracity),
		Type:      resolveMemoryType(content, memoryType),
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

// resolveMemoryType prefers the caller's type and otherwise reads it off the
// statement.
//
// An unrecognised explicit type is inferred rather than rejected, because the
// alternative is a caller who typo'd a type silently getting the shortest life in
// the table for a standing preference.
func resolveMemoryType(content string, explicit MemoryType) MemoryType {
	if _, known := halfLifeDaysByType[explicit]; known {
		return explicit
	}
	return classifyMemoryType(content)
}

// normalizeVeracity maps an unset or unrecognised value onto Unknown, so a caller
// that passes "" gets the middle of the table rather than the bottom of it.
func normalizeVeracity(v Veracity) Veracity {
	if _, ok := veracityWeight[v]; ok {
		return v
	}
	return Unknown
}

// strongerVeracity returns whichever of the two provenances is worth more.
//
// Used only on a repeat, and only upwards: corroboration is meant to lift an
// entry toward its ceiling, and the only way that can go wrong is if the repeat
// quietly lowers the ceiling it is climbing toward.
func strongerVeracity(current, incoming Veracity) Veracity {
	if incoming.Weight() > current.Weight() {
		return normalizeVeracity(incoming)
	}
	return current
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
//
// The two sources are independent, so an error from one does not discard the
// other. The file error is returned alongside whatever rendered rather than
// instead of it: a memory file that cannot be read used to blank the store
// entries too, and the turn then looked exactly like "this user has no memory".
// The path is carried because "permission denied" on its own named no file, and
// 74 identical warnings in one log could not be traced to a location.
func Live(store *Store, projectID, sessionID, filePath string, maxBytes int) func() (string, error) {
	return func() (string, error) {
		var parts []string
		var fileErr error
		if filePath != "" {
			rendered, err := RenderFile(filePath, maxBytes)
			switch {
			case err != nil && !os.IsNotExist(err):
				fileErr = fmt.Errorf("%s: %w", filePath, err)
			case err == nil && rendered != "":
				parts = append(parts, rendered)
			}
		}
		if store != nil {
			entries, err := store.List(projectID, sessionID)
			if err != nil {
				if fileErr != nil {
					return "", fmt.Errorf("%v; %w", fileErr, err)
				}
				return "", err
			}
			if rendered := RenderContext(entries, remainingBytes(maxBytes, parts)); rendered != "" {
				parts = append(parts, strings.TrimSuffix(rendered, "\n"))
			}
		}
		if len(parts) == 0 {
			return "", fileErr
		}
		return strings.Join(parts, "\n") + "\n", fileErr
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

// isImperative reports whether a statement is phrased as a standing directive.
//
// Anywhere in the statement, not only at the front. "for this task, always run the
// gate" is a one-off phrase followed by a standing rule, and the rule is the part
// that decides the lifetime.
func isImperative(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{
		"never ", "always ", "do not ", "don't ", "must ", "avoid ", "stop ",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
		if strings.Contains(trimmed, ", "+prefix) {
			return true
		}
	}
	return false
}

// containsWord reports whether a bare word appears in the text, so "mistake," and
// "mistakes." match while "remistake" does not.
func containsWord(text, word string) bool {
	for offset := 0; ; {
		index := strings.Index(text[offset:], word)
		if index < 0 {
			return false
		}
		at := offset + index
		beforeOK := at == 0 || isSeparator(text[at-1])
		after := at + len(word)
		afterOK := after >= len(text) || isSeparator(text[after])
		if beforeOK && afterOK {
			return true
		}
		offset = at + len(word)
	}
}

// isSeparator reports whether a byte ends a word. Anything that is not a letter or
// digit does, which is why the check is a negation rather than an allow-list: a
// miss here means a memory keeps the wrong lifetime, and the set of bytes that are
// not word characters is far larger than any list worth writing down.
func isSeparator(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' {
		return false
	}
	return true
}

// classifyMemoryType infers the kind of claim from the statement's own wording.
//
// Deliberately a classifier over text rather than a field the model fills in. The
// extractor is asked for a preference every time and it complies, so asking it
// again for a type would produce a type for everything and therefore no
// information. Reading the statement for the words that actually carry the claim
// is the only signal here that the model does not control.
//
// It is conservative on purpose and returns "" when nothing matches, which reads as
// the old single half-life. A wrong type is worse than no type: mislabelling a
// one-off request as a profile would keep it for ten years.
//
// The order is significant. Instruction and constraint are checked before
// preference, because "always run gofmt before committing" is a preference by
// subject and an instruction by form, and the shorter life of an instruction is the
// safer error when it is genuinely one. Request is checked last so that a request
// phrased as a rule does not get the short life, which is the failure that would
// actually lose a preference.
func classifyMemoryType(content string) MemoryType {
	text := strings.ToLower(strings.TrimSpace(content))
	if text == "" {
		return ""
	}

	// A prohibition on a specific thing is a constraint, and is checked before the
	// generic imperative block because "do not use the vendored SDK" is an
	// instruction by form and a constraint by meaning. Reading it the other way
	// gives a two-year lifetime where the claim is about something being off the
	// table, which is the wrong error: over-keeping a constraint is recoverable and
	// under-keeping an instruction is not.
	for _, marker := range []string{"must not ", "out of scope", "do not use ", "cannot ", "never use "} {
		if strings.Contains(text, marker) {
			return Constraint
		}
	}
	// Something that went wrong, so it is not repeated. Before the imperative block
	// for the same reason: "avoid the mistake of ..." is about the past.
	if containsWord(text, "mistake") || containsWord(text, "broke") ||
		containsWord(text, "failed") || containsWord(text, "regression") ||
		strings.Contains(text, "the bug was ") {
		return Error
	}
	// Standing directives, by the imperative or the prohibition.
	for _, prefix := range []string{
		"never ", "always ", "do not ", "don't ", "must ", "avoid ", "stop ",
	} {
		if strings.HasPrefix(text, prefix) {
			return Instruction
		}
	}
	// Explicit self-description of the user, which outlives any single task.
	if strings.Contains(text, "i am ") || strings.Contains(text, "i'm ") ||
		strings.Contains(text, "the user is ") || strings.Contains(text, "the user prefers ") {
		return Profile
	}
	// A goal is something being worked towards, not a rule or a fact.
	for _, marker := range []string{"i want to", "the goal is", "we are building", "the plan is"} {
		if strings.Contains(text, marker) {
			return Goal
		}
	}
	// A choice that was made, with its reason attached.
	if strings.Contains(text, "because ") || strings.Contains(text, "decided to ") ||
		strings.Contains(text, "chose ") || strings.Contains(text, "we use ") {
		return Decision
	}
	// A one-off, and only when it is unmistakably one. Checked last on purpose, see
	// above: anything that reads as a standing rule keeps the longer life.
	for _, marker := range []string{"this time", "just for now", "for this task", "right now "} {
		if !strings.Contains(text, marker) {
			continue
		}
		// A one-off phrase inside a standing rule is still a standing rule.
		// "for this task, always run the gate" carries both, and taking the shorter
		// life would forget a directive the user meant to keep.
		if isImperative(text) {
			continue
		}
		return Request
	}
	// Anything about the project's own shape.
	if strings.Contains(text, "the repo ") || strings.Contains(text, "the build ") ||
		strings.Contains(text, "the api ") || strings.Contains(text, "architecture") {
		return Context
	}
	// The default, because this store is a preference extractor and almost
	// everything it holds is a preference.
	return Preference
}

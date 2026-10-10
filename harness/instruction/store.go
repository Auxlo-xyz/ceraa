package instruction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/internal/logger"
	"ceraa/internal/paths"
)

// cacheVersion is the on-disk cache shape.
//
// A snapshot written by an older compiler is discarded rather than read: a
// rule's identity depends on the segmentation that produced it, so a stale shape
// would resurrect ids the current compiler would never generate and the delivered
// set would suppress rules it has never seen.
const cacheVersion = 1

// Stats is what a store did, and it is the only way a caller can tell a real
// recompile from a cache hit without instrumenting the filesystem underneath it.
type Stats struct {
	// Compiles is how many times Compile ran. It is the number that matters: a
	// stat-gated refresh that recompiled anyway is a broken gate.
	Compiles int
	// Reads is how many times a source file was opened at all. It is what
	// separates the stat gate from the hash gate: a file whose stat is unchanged
	// is never opened, and a file whose mtime moved is opened exactly once.
	// Without it the two gates are indistinguishable from outside, because with
	// both present a recompile is rare either way - and a gate nobody can
	// observe is a gate nobody can tell has stopped working.
	Reads int
	// CacheReads, CacheWrites and CacheDiscards count disk cache traffic.
	CacheReads    int
	CacheWrites   int
	CacheDiscards int
}

// fileStamp is the cheap gate: size and modification time.
//
// Both, because either alone lies on some filesystem. A same-size edit keeps the
// size; a coarse timestamp granularity keeps the mtime.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func stampOf(info os.FileInfo) fileStamp {
	return fileStamp{size: info.Size(), modTime: info.ModTime()}
}

// entry is one source's compiled state.
//
// It is replaced wholesale rather than mutated, so a reader holding one never
// observes half of an update.
type entry struct {
	snap  Snapshot
	diags Diagnostics
	stamp fileStamp
	// Withheld is retained alongside the snapshot because it is a compile-time
	// finding and must survive a cache read, not just a recompile: a finding that
	// disappears when the file stops changing is a finding nobody reads.
	withheld []ScopeWithheld
}

// Store holds compiled project instructions and keeps them current.
//
// # Why there is no watcher
//
// Not fsnotify. /workspace is a virtiofs mount, and a notification that is
// delivered once and then missed leaves the store serving rules the file on disk
// no longer says, with nothing in the log to say so: the model is told the old
// rules and every consumer of this store is wrong in the direction nobody
// notices. A stat poll has no such state. It is either current or about to be,
// and the gap is bounded by the call.
//
// # Where the cache lives
//
// Under paths.DataDir(), never /tmp. This host has wiped /tmp three times in one
// day, each time taking the running bot with it. A cache that vanishes is merely
// a cold compile, but a lock or a log that vanishes is a crash with no history,
// and the difference is the reason the same reasoning applies here.
//
// # Cache failures are cold compiles
//
// An unreadable, unparseable or half-written cache is deleted and ignored. It is
// a cache: the only correct response to losing one is to rebuild, and turning a
// missing optimisation into a failed request is strictly worse than paying for
// the parse.
type Store struct {
	// mu guards everything below it. It is held for the duration of a publish,
	// for the duration of a pointer copy on read, and for nothing else. No file
	// I/O and no parsing happens under it, so a reader is never blocked behind
	// somebody else's recompile.
	mu      sync.RWMutex
	root    string
	sources []string
	entries map[string]*entry
	stats   Stats
	// started and published order concurrent refreshes. A refresh that parsed
	// slowly discards its own result when a later one has already published,
	// which is the only case where publishing unconditionally would overwrite a
	// newer compile with an older one.
	started   uint64
	published uint64

	// propose, when set, is asked to scope rules that declared none and named
	// nothing the filesystem could confirm. It is a field rather than a
	// parameter so a caller wires it once and every refresh uses it, and it is
	// nil by default because the deterministic pass alone is worth most of the
	// coverage and a model call must stay opt-in.
	propose Proposer
}

// WithProposer returns the store with a model pass installed.
//
// A builder rather than a second constructor so existing callers keep
// constructing the store in one expression. The proposer runs off the hot path:
// a refresh happens when the file's stamp or digest moved, and the compiled
// result is cached, so a model call costs one per edit rather than one per run.
func (s *Store) WithProposer(propose Proposer) *Store {
	s.propose = propose
	return s
}

// NewStore returns a store over project-relative sources resolved against root.
//
// Order is the order given, and it is the delivery order of the merged snapshot:
// the project instruction file first, then the workspace one. A store with no
// sources compiles nothing, which is a state rather than an error - a project
// that has not written an AGENTS.md is a project that has not written one yet.
//
// NewStore reads nothing but the caches it may have to discard. Refresh has to
// be called before Snapshot means anything, and that is deliberate: a constructor
// that reads five files blocks its caller on five files, and a caller that wants
// the current rules should be able to see that it asked.
func NewStore(root string, sources ...string) *Store {
	cleaned := make([]string, 0, len(sources))
	seen := map[string]struct{}{}
	for _, source := range sources {
		trimmed := filepath.ToSlash(strings.TrimSpace(source))
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		cleaned = append(cleaned, trimmed)
	}
	store := &Store{root: root, sources: cleaned, entries: map[string]*entry{}}
	// A corrupt cache gets one line, here, where somebody is looking at the
	// store. Repeated on every refresh it becomes log noise that stops being
	// read, and a store that discards caches silently looks like one that never
	// wrote any.
	for _, source := range cleaned {
		if discardCorruptCache(cachePath(root, source)) {
			store.stats.CacheDiscards++
			logger.Warn("instruction cache for " + source + " was unreadable and has been discarded; the file will be recompiled")
		}
	}
	return store
}

// Sources returns the project-relative paths this store compiles, in delivery
// order.
func (s *Store) Sources() []string { return append([]string(nil), s.sources...) }

// Stats returns the counters. A copy, so a caller formatting them is not holding
// the lock while it does.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// Snapshot returns the merged compiled state, with every source's rules in
// delivery order.
//
// The rules themselves are shared rather than copied: they are immutable once
// published, so a caller can read every field of every rule while a recompile
// runs and will see one whole compile rather than a mixture of two.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mergedLocked()
}

func (s *Store) mergedLocked() Snapshot {
	merged := Snapshot{}
	var parts []string
	for _, source := range s.sources {
		item, ok := s.entries[source]
		if !ok {
			continue
		}
		merged.Instructions = append(merged.Instructions, item.snap.Instructions...)
		parts = append(parts, source+":"+item.snap.Digest)
	}
	merged.Digest = digest(strings.Join(parts, "\n"))
	return merged
}

// Diagnostics returns every finding from the last refresh, in source order.
func (s *Store) Diagnostics() Diagnostics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out Diagnostics
	for _, source := range s.sources {
		if item, ok := s.entries[source]; ok {
			out = append(out, item.diags...)
		}
	}
	return out
}

// Refresh re-reads every source and republishes the store.
//
// Three gates, cheapest first:
//
//  1. stat. An unchanged size and mtime returns without opening the file, which
//     is the common case on a turn where nothing was edited.
//  2. hash. A filesystem that reports a new mtime for identical bytes - a touch,
//     a checkout, a restore from a snapshot - costs one read and nothing else.
//  3. recompile, and only for the source whose bytes actually moved.
//
// Every step reads and parses outside the lock. A slow mount delays one refresh
// rather than every reader of the store.
func (s *Store) Refresh() Snapshot {
	s.mu.Lock()
	s.started++
	seq := s.started
	s.mu.Unlock()

	// A nil result paired with publish=false means "this source has not changed",
	// and the existing entry stays. A nil result paired with publish=true means
	// the source is gone and its rules should stop being delivered. The
	// distinction is the difference between a fact and a failure: a file that
	// does not exist has been withdrawn, and a file that cannot be read has to
	// keep its last known good compile rather than taking every rule the project
	// owns down with one permission error.
	results := make([]*entry, len(s.sources))
	publish := make([]bool, len(s.sources))
	for index, source := range s.sources {
		results[index], publish[index] = s.refreshOne(source)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.published > seq {
		return s.mergedLocked()
	}
	s.published = seq
	for index, source := range s.sources {
		switch {
		case results[index] != nil:
			s.entries[source] = results[index]
		case publish[index]:
			delete(s.entries, source)
		}
	}
	return s.mergedLocked()
}

// refreshOne returns the new entry for one source, and whether the caller should
// publish it. A nil entry with publish=true is a withdrawal.
func (s *Store) refreshOne(source string) (*entry, bool) {
	path := filepath.Join(s.root, filepath.FromSlash(source))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true
		}
		return nil, false
	}
	stamp := stampOf(info)

	s.mu.RLock()
	current := s.entries[source]
	s.mu.RUnlock()

	if current != nil && current.stamp == stamp {
		return nil, false
	}

	body, err := os.ReadFile(path)
	s.mu.Lock()
	s.stats.Reads++
	s.mu.Unlock()
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256(body)
	sourceDigest := hex.EncodeToString(sum[:])

	if current != nil && current.snap.Digest == sourceDigest {
		// Touched, not edited. The stamp moves so the next refresh takes the
		// cheap path, and not one rule is recompiled.
		return &entry{snap: current.snap, diags: current.diags, stamp: stamp}, true
	}

	if cached, ok := readCache(s.root, source, sourceDigest); ok {
		cached.stamp = stamp
		s.mu.Lock()
		s.stats.CacheReads++
		s.mu.Unlock()
		return cached, true
	}

	snap, diags := Compile(source, body)
	s.mu.Lock()
	s.stats.Compiles++
	s.mu.Unlock()

	// Scoping runs before the cache write so a tagged snapshot is what gets
	// persisted and the model is not asked again on the next run for a file that
	// has not changed.
	tagged := TagScopes(context.Background(), s.root, source, snap, s.propose)
	diags = append(diags, scopeDiagnostics(source, tagged.Withheld)...)

	s.writeCache(s.root, source, tagged.Snapshot, diags)
	return &entry{snap: tagged.Snapshot, diags: diags, stamp: stamp, withheld: tagged.Withheld}, true
}

// scopeDiagnostics renders withheld scopes as findings.
//
// Hard findings rather than warnings, and the distinction matters: a warning is
// read by nobody at the moment it counts, while a finding is reported beside the
// line that caused it and has to be dealt with.
func scopeDiagnostics(source string, withheld []ScopeWithheld) Diagnostics {
	if len(withheld) == 0 {
		return nil
	}
	out := make(Diagnostics, 0, len(withheld))
	for _, item := range withheld {
		out = append(out, Diagnostic{
			Kind:    DiagUnresolvedScope,
			Source:  source,
			Line:    item.Line,
			EndLine: item.Line,
			Message: item.Reason + ": " + item.Applies,
		})
	}
	return out
}

// writeCache stores a snapshot beside the rest of Ceraa's data.
//
// A failure here is silent by design. The cache is an optimisation, the file it
// was built from is still there, and the next refresh reads that file anyway, so
// a failure costs one parse and nothing a caller could observe.
func (s *Store) writeCache(root, source string, snap Snapshot, diags Diagnostics) {
	payload, err := json.MarshalIndent(cachedSnapshot{
		Version:      cacheVersion,
		Source:       source,
		Digest:       snap.Digest,
		Instructions: snap.Instructions,
		Diagnostics:  diags,
	}, "", "  ")
	if err != nil {
		return
	}
	path := cachePath(root, source)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instructions-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	// Written beside the target and renamed into it, so a crash mid-write leaves
	// the previous cache rather than a truncated one that every later start would
	// have to discover and discard.
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return
	}
	s.mu.Lock()
	s.stats.CacheWrites++
	s.mu.Unlock()
}

// readCache returns a cached snapshot only when its digest matches the bytes
// that were just read.
//
// Checked rather than trusted, because surviving a restart is the entire point
// of the cache and a cache that survives without checking is a stale set of
// rules served to a model that cannot tell.
func readCache(root, source, digestOfFile string) (*entry, bool) {
	data, err := os.ReadFile(cachePath(root, source))
	if err != nil {
		return nil, false
	}
	var payload cachedSnapshot
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, false
	}
	if payload.Version != cacheVersion || payload.Source != source || payload.Digest != digestOfFile {
		return nil, false
	}
	return &entry{
		snap:  Snapshot{Source: source, Digest: payload.Digest, Instructions: payload.Instructions},
		diags: payload.Diagnostics,
	}, true
}

type cachedSnapshot struct {
	Version      int           `json:"version"`
	Source       string        `json:"source"`
	Digest       string        `json:"digest"`
	Instructions []Instruction `json:"instructions"`
	Diagnostics  Diagnostics   `json:"diagnostics,omitempty"`
}

// discardCorruptCache removes a cache that cannot be read and reports whether it
// did.
func discardCorruptCache(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var payload cachedSnapshot
	if err := json.Unmarshal(data, &payload); err != nil || payload.Version != cacheVersion {
		_ = os.Remove(path)
		return true
	}
	return false
}

// cachePath is where one source's compiled snapshot lives on disk.
//
// Keyed on the project root as well as the file name. Two projects both have an
// AGENTS.md, and a cache keyed on the name alone is one file that both of them
// write: the digest check stops the wrong rules being served, but every refresh
// then reads a cache it will reject, which is a gate that costs what it saves.
//
// The name is flattened to one filename and anything that could climb out of
// the directory is refused rather than rewritten into something that merely
// looks safe. A source is operator configuration, and configuration should
// never choose where the process writes.
func cachePath(root, source string) string {
	name := filepath.Base(filepath.FromSlash(source))
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
	if clean == "" || clean == "." || clean == ".." {
		clean = "instructions"
	}
	if ext := filepath.Ext(clean); ext == ".json" {
		clean = strings.TrimSuffix(clean, ext)
	}
	rootSum := sha256.Sum256([]byte(root))
	return filepath.Join(paths.DataDir(), "instructions", hex.EncodeToString(rootSum[:4])+"-"+clean+".json")
}

// CachePath is where one source's compiled snapshot is kept, so a caller can
// find it and delete it without reading this file.
func CachePath(root, source string) string { return cachePath(root, source) }

package search

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Follower keeps an index current as files change, at the point a write succeeds.
//
// The index was never the problem with search. The problem was that nothing
// maintained it: the index file did not exist on this host at all, so fts_search
// had been called once and returned "No matches" against an empty database. An
// indexer that only runs on a timer or a watcher would be the same problem with
// more moving parts, so this is driven by the write.
//
// Why a debounce rather than indexing inside the write: a single agent turn can
// complete several edits at once, and a run that rewrites a file in three steps
// should cost one index update, not three. The last write wins, which is also the
// only one whose content is final.
type Follower struct {
	// DBPath is the index to maintain.
	DBPath string
	// Root bounds what may be indexed. A path outside it is ignored, so a write to
	// a temporary file or a path outside the project cannot pull unrelated content
	// into the index.
	Root string
	// Debounce is how long to wait after the last queued path before indexing.
	Debounce time.Duration

	mu      sync.Mutex
	pending map[string]time.Time
	timer   *time.Timer
	closed  bool
	// now is the clock, so the debounce is testable without sleeping.
	now func() time.Time
	// indexed is a hook for tests and for reporting.
	indexed func(path string, err error)
}

// NewFollower returns a follower with a sane debounce.
func NewFollower(dbPath, root string) *Follower {
	return &Follower{
		DBPath:   dbPath,
		Root:     root,
		Debounce: 750 * time.Millisecond,
		pending:  map[string]time.Time{},
		now:      time.Now,
	}
}

// Note records that a file changed and schedules it.
func (f *Follower) Note(path string) {
	if f == nil || f.DBPath == "" {
		return
	}
	path = strings.TrimSpace(path)
	if path == "" || !f.inScope(path) {
		return
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.pending[absolute] = f.now()
	if f.timer != nil {
		f.timer.Stop()
	}
	f.timer = time.AfterFunc(f.Debounce, f.flush)
}

// flush indexes everything queued.
func (f *Follower) flush() {
	f.mu.Lock()
	if f.closed || len(f.pending) == 0 {
		f.mu.Unlock()
		return
	}
	batch := make([]string, 0, len(f.pending))
	for path := range f.pending {
		batch = append(batch, path)
	}
	f.pending = map[string]time.Time{}
	f.mu.Unlock()

	idx, err := Open(f.DBPath)
	if err != nil {
		f.report(batch, err)
		return
	}
	defer func() { _ = idx.Close() }()
	for _, path := range batch {
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			// The file went away between the write and here, which happens when an
			// operation renames or removes its target. Drop it from the index rather
			// than leaving a path that no longer resolves.
			_ = idx.RemoveFile(path)
			f.reportOne(path, readErr)
			continue
		}
		f.reportOne(path, idx.IndexFile(path, string(contents)))
	}
}

// FlushNow indexes anything queued, without waiting. For shutdown and for tests.
func (f *Follower) FlushNow() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	f.mu.Unlock()
	f.flush()
}

// Close stops accepting work and indexes what is left, so a queued write is not
// lost on shutdown.
func (f *Follower) Close() {
	if f == nil {
		return
	}
	f.FlushNow()
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// Pending reports how many paths are queued, for tests and for status.
func (f *Follower) Pending() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// inScope keeps the index to the tree it was built for.
func (f *Follower) inScope(path string) bool {
	if f.Root == "" {
		return true
	}
	root, err := filepath.Abs(f.Root)
	if err != nil {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, absolute)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (f *Follower) report(paths []string, err error) {
	for _, path := range paths {
		f.reportOne(path, err)
	}
}

func (f *Follower) reportOne(path string, err error) {
	if f.indexed != nil {
		f.indexed(path, err)
	}
}

package sessioncontrol

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"ceraa/harness/inbox"
	"ceraa/harness/operation"
	"ceraa/harness/permission"
	"ceraa/harness/question"
)

var (
	ErrDuplicateRun  = errors.New("active run already registered")
	ErrStaleHandle   = errors.New("stale run handle")
	ErrInvalidRun    = errors.New("invalid active run")
	ErrActorConflict = errors.New("run actor is already bound")
)

// RunHandle is the ownership record for one live run. The owner that creates a
// run keeps this handle and uses it to publish the run's live dependencies.
// The registry never copies or mutates those dependencies.
type RunHandle struct {
	SessionID  SessionID
	RunID      RunID
	Inbox      *inbox.Inbox
	Operations operation.Manager
	Permission *permission.Manager
	Question   *question.Manager

	doneMu   sync.Mutex
	doneOnce sync.Once
	done     chan struct{}
	registry *Registry
	serial   uint64
	actor    Actor
	actorSet bool
}

// NewRunHandle creates the ownership record for a run. Permission and question
// managers may be nil when that surface does not provide them.
func NewRunHandle(sessionID SessionID, runID RunID, ib *inbox.Inbox, operations operation.Manager, permissions *permission.Manager, questions *question.Manager) *RunHandle {
	return &RunHandle{
		SessionID:  sessionID,
		RunID:      runID,
		Inbox:      ib,
		Operations: operations,
		Permission: permissions,
		Question:   questions,
		done:       make(chan struct{}),
	}
}

func (h *RunHandle) ensureDone() chan struct{} {
	h.doneMu.Lock()
	defer h.doneMu.Unlock()
	if h.done == nil {
		h.done = make(chan struct{})
	}
	return h.done
}

// Done returns a channel closed when the run has finished.
func (h *RunHandle) Done() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.ensureDone()
}

// MarkDone closes the run's completion channel once.
func (h *RunHandle) MarkDone() {
	if h == nil {
		return
	}
	done := h.ensureDone()
	h.doneOnce.Do(func() { close(done) })
}

// IsDone reports whether the run has finished.
func (h *RunHandle) IsDone() bool {
	if h == nil {
		return true
	}
	select {
	case <-h.Done():
		return true
	default:
		return false
	}
}

// Registry tracks the one active run allowed for each session.
type Registry struct {
	mu   sync.RWMutex
	next uint64
	runs map[SessionID]*RunHandle
}

// NewRegistry creates an empty active-run registry.
func NewRegistry() *Registry {
	return &Registry{runs: make(map[SessionID]*RunHandle)}
}

// Register makes handle the active owner for its session.
func (r *Registry) Register(handle *RunHandle) error {
	if r == nil || handle == nil {
		return fmt.Errorf("%w: handle is required", ErrInvalidRun)
	}
	if strings.TrimSpace(string(handle.SessionID)) == "" || strings.TrimSpace(string(handle.RunID)) == "" {
		return fmt.Errorf("%w: session and run ids are required", ErrInvalidRun)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = make(map[SessionID]*RunHandle)
	}
	if handle.registry != nil {
		return fmt.Errorf("%w: handle is already owned", ErrDuplicateRun)
	}
	if existing := r.runs[handle.SessionID]; existing != nil {
		return fmt.Errorf("%w: session %q", ErrDuplicateRun, handle.SessionID)
	}
	handle.ensureDone()
	r.next++
	handle.registry = r
	handle.serial = r.next
	r.runs[handle.SessionID] = handle
	return nil
}

// Get returns the current active run for sessionID.
func (r *Registry) Get(sessionID SessionID) (*RunHandle, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	handle, ok := r.runs[sessionID]
	r.mu.RUnlock()
	return handle, ok
}

// LookupRun returns the current run only when both its session and run IDs
// match. This prevents an old run ID from selecting a replacement run.
func (r *Registry) LookupRun(sessionID SessionID, runID RunID) (*RunHandle, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	handle := r.runs[sessionID]
	ok := handle != nil && handle.RunID == runID && handle.registry == r
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return handle, true
}

// BindActor binds the authenticated actor that owns handle. Binding is
// idempotent for the same actor and cannot be changed to another actor.
func (r *Registry) BindActor(handle *RunHandle, actor Actor) error {
	if r == nil || handle == nil || !actor.valid() {
		return fmt.Errorf("%w: handle and actor are required", ErrInvalidRun)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.currentLocked(handle) {
		return ErrStaleHandle
	}
	if handle.actorSet {
		if handle.actor == actor {
			return nil
		}
		return ErrActorConflict
	}
	handle.actor = actor
	handle.actorSet = true
	return nil
}

// Actor returns the actor bound to the current handle, or the zero Actor when
// the handle is stale or has not been bound.
func (r *Registry) Actor(handle *RunHandle) Actor {
	if r == nil || handle == nil {
		return Actor{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.currentLocked(handle) || !handle.actorSet {
		return Actor{}
	}
	return handle.actor
}

// CancellationEligible atomically checks that the requested run is still the
// registered run, is unfinished, and belongs to actor. The returned handle is
// the same current handle checked by this operation.
func (r *Registry) CancellationEligible(sessionID SessionID, runID RunID, actor Actor) (*RunHandle, bool) {
	if r == nil || !actor.valid() {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	handle := r.runs[sessionID]
	if handle == nil || handle.RunID != runID || !r.currentLocked(handle) || handle.IsDone() || !handle.actorSet || handle.actor != actor {
		return nil, false
	}
	return handle, true
}

// List returns a stable snapshot of all current handles, sorted by session ID.
func (r *Registry) List() []*RunHandle {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]*RunHandle, 0, len(r.runs))
	for _, handle := range r.runs {
		out = append(out, handle)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// currentLocked reports whether handle is the still-current registration.
// The caller must hold r.mu.
func (r *Registry) currentLocked(handle *RunHandle) bool {
	return handle != nil && r.runs[handle.SessionID] == handle && handle.registry == r && handle.serial != 0
}

// Unregister removes handle only if it is still the current owner. A handle
// from an earlier run cannot remove a replacement registered for the session.
func (r *Registry) Unregister(handle *RunHandle) error {
	if r == nil || handle == nil {
		return ErrStaleHandle
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.runs[handle.SessionID]
	if current != handle || handle.registry != r || current.serial != handle.serial {
		return ErrStaleHandle
	}
	delete(r.runs, handle.SessionID)
	handle.registry = nil
	return nil
}

// Package inbox guards a session against duplicate events.
// Each session gets one Inbox. External callers Submit() events with a stable
// ID; the inbox deduplicates by ID so retries or redeliveries from Telegram
// never cause double execution. Control events (stop, heartbeat, settings)
// use the same ID space so a steering message can interrupt mid-turn.
package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"ceraa/harness/llm"
)

type RequestID string

type Kind string

const (
	KindExternal Kind = "external"
	KindControl  Kind = "control"
	KindCrash    Kind = "crash"
)

// ControlMode is the verb inside a control event.
type ControlMode string

const (
	ModeStopWhenIdle   ControlMode = "when_idle"
	ModeStopHard       ControlMode = "hard"
	ModeHeartbeat      ControlMode = "heartbeat"
	ModeUpdateSettings ControlMode = "settings"
)

type Settings struct {
	ReasoningEffort llm.ReasonLevel `json:"reasoning,omitempty"`
}

type Control struct {
	Mode       ControlMode
	Reason     string
	Parameters Settings `json:"-"`
}

// Submission wraps an event that arrived via Submit().
type Submission struct {
	ID       RequestID
	Kind     Kind
	Payload  json.RawMessage
	Received time.Time
}

// Inbox is the per-session deduplicator.
type Inbox struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	enqueue chan Submission
	out     chan Submission

	seen    map[RequestID]struct{}
	mu      sync.RWMutex
	recent  []RequestID
	cap     int32
	seenIDs func() []RequestID
}

// New returns an Inbox. seenIDs seeds the dedup set (from session recovery).
func New(ctx context.Context, seenIDs []RequestID, capacity int32) (*Inbox, error) {
	if len(seenIDs) > 0 {
		// reject empty IDs early
		for _, id := range seenIDs {
			if id == "" {
				return nil, fmt.Errorf("seen input ID is empty")
			}
		}
	}
	if capacity <= 0 {
		capacity = 10000
	}
	ctx, cancel := context.WithCancelCause(ctx)
	in := &Inbox{
		ctx:     ctx,
		cancel:  cancel,
		enqueue: make(chan Submission),
		out:     make(chan Submission),
		seen:    make(map[RequestID]struct{}, len(seenIDs)),
		cap:     capacity,
	}
	for _, id := range seenIDs {
		in.seen[id] = struct{}{}
	}
	go in.run()
	return in, nil
}

func (in *Inbox) Submit(ctx context.Context, sub Submission) error {
	if err := in.validate(sub); err != nil {
		return err
	}
	if sub.ID != "" {
		in.mu.Lock()
		if _, dup := in.seen[sub.ID]; dup {
			in.mu.Unlock()
			return nil // silent dedup
		}
		in.seen[sub.ID] = struct{}{}
		in.recent = append(in.recent, sub.ID)
		if int32(len(in.recent)) > in.cap {
			old := in.recent[0]
			in.recent = in.recent[1:]
			delete(in.seen, old)
		}
		in.mu.Unlock()
	}
	select {
	case in.enqueue <- sub:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-in.ctx.Done():
		return context.Cause(in.ctx)
	}
}

func (in *Inbox) validate(sub Submission) error {
	if sub.Kind != KindExternal && sub.Kind != KindControl && sub.Kind != KindCrash {
		return fmt.Errorf("input kind=%q unsupported", sub.Kind)
	}
	if sub.Kind == KindControl {
		var env struct {
			Mode       ControlMode     `json:"mode"`
			Reason     string          `json:"reason"`
			Parameters json.RawMessage `json:"parameters,omitempty"`
		}
		if err := json.Unmarshal(sub.Payload, &env); err != nil {
			return fmt.Errorf("decode control: %w", err)
		}
		switch env.Mode {
		case ModeStopHard, ModeStopWhenIdle:
			if len(env.Parameters) != 0 {
				return fmt.Errorf("control mode %q does not accept parameters", env.Mode)
			}
		case ModeHeartbeat:
			if env.Reason == "" {
				return fmt.Errorf("heartbeat reason is empty")
			}
		case ModeUpdateSettings:
			var s Settings
			if err := json.Unmarshal(env.Parameters, &s); err != nil {
				return fmt.Errorf("decode settings: %w", err)
			}
			if !s.ReasoningEffort.Valid() {
				return fmt.Errorf("unsupported reasoning effort %q", s.ReasoningEffort)
			}
		default:
			return fmt.Errorf("unsupported control mode %q", env.Mode)
		}
	}
	return nil
}

func (in *Inbox) Output() <-chan Submission {
	return in.out
}

func (in *Inbox) Drain() []Submission {
	out := make([]Submission, 0, len(in.enqueue)+len(in.out))
	for {
		select {
		case s := <-in.enqueue:
			out = append(out, s)
		default:
			return out
		}
	}
}

// run moves submissions from enqueue to out with basic ordering.
func (in *Inbox) run() {
	defer close(in.out)
	pending := make([]Submission, 0, 16)
	for {
		var out chan Submission
		var next Submission
		if len(pending) > 0 {
			out = in.out
			next = pending[0]
		}
		select {
		case sub := <-in.enqueue:
			pending = append(pending, sub)
		case out <- next:
			pending[0] = Submission{}
			pending = pending[:len(pending)-1] // shift
			if len(pending) > 0 {
				// rotate
				pending = append(pending[1:], Submission{})[:len(pending)-1]
			}
		case <-in.ctx.Done():
			for _, sub := range pending {
				select {
				case in.out <- sub:
				default:
					return
				}
			}
			return
		}
	}
}

func (in *Inbox) SeenIDs() []RequestID {
	in.mu.RLock()
	defer in.mu.RUnlock()
	result := make([]RequestID, len(in.recent))
	copy(result, in.recent)
	return result
}

func (in *Inbox) Stop() {
	in.cancel(context.Canceled)
}

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	cp := make(json.RawMessage, len(b))
	copy(cp, b)
	return cp
}

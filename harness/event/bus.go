package event

import (
	"sync"
	"time"
)

// Type of event, maps to soraacode summaryAggregator setOn* types
type Type string

const (
	TypeThinking        Type = "thinking"
	TypeContent         Type = "content"
	TypePermission      Type = "permission"
	TypeQuestion        Type = "question"
	TypeCost            Type = "cost"
	TypeTokens          Type = "tokens"
	TypeToolStart       Type = "tool_start"
	TypeToolEnd         Type = "tool_end"
	TypeComplete        Type = "complete"
	TypeError           Type = "error"
	TypeCompact         Type = "compact"
	TypeSubagent        Type = "subagent"
	TypeCompactProgress Type = "compact_progress"
)

// Event is a harness event streamed to bridge, mirrors soraacode event-subscription-service
type Event struct {
	Type      Type           `json:"type"`
	SessionID string         `json:"session_id"`
	Data      map[string]any `json:"data"`
	Time      time.Time      `json:"time"`
}

// Bus is an SSE-like in-memory event bus with reconnect support
// Maps to soraacode soraacode/events.js subscribeToEvents + globalEvents
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]chan Event
}

// New creates a bus
func New() *Bus {
	return &Bus{subs: make(map[string][]chan Event)}
}

var Default = New()

// Wildcard is the reserved session key that receives every event.
//
// A surface watching the process rather than one conversation subscribes here,
// because it has not been told which session matters yet: a dashboard opened
// before anything has been picked has no id to ask for. It is a key like any
// other, so nothing about Publish changes shape to accommodate it.
const Wildcard = "*"

// Publish sends event to all subscribers of sessionID, non-blocking
func (b *Bus) Publish(e Event) {
	e.Time = time.Now()
	// Sends happen under the write lock rather than a read lock over a copy.
	// Unsubscribe closes the channel under the same lock, so a copy taken before
	// the close would let Publish send on a channel that was already closed. Every
	// send is non-blocking, so holding the lock cannot stall a publisher behind a
	// slow reader.
	b.mu.Lock()
	defer b.mu.Unlock()
	send := func(chs []chan Event) {
		for _, ch := range chs {
			select {
			case ch <- e:
			default:
			}
		}
	}
	send(b.subs[e.SessionID])
	if e.SessionID != Wildcard {
		send(b.subs[Wildcard])
	}
}

// Subscribe returns a channel for sessionID events, like soraacode subscribeToEvents
// Caller must handle idle timeout and reconnect (30s idle, 1s->15s backoff)
func (b *Bus) Subscribe(sessionID string, buf int) <-chan Event {
	if buf <= 0 {
		buf = 64
	}
	ch := make(chan Event, buf)
	b.mu.Lock()
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes channel
func (b *Bus) Unsubscribe(sessionID string, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subs[sessionID]
	for i, c := range subs {
		if c == ch {
			close(c)
			b.subs[sessionID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(b.subs[sessionID]) == 0 {
		delete(b.subs, sessionID)
	}
}

// SubscribeWithRetry keeps resubscribing with exponential backoff like soraacode events.js
// Calls handler for each event until ctx done
func SubscribeWithRetry(bus *Bus, sessionID string, handler func(Event)) {
	// Simple wrapper: caller loops, we just provide single subscription
	// Reconnect logic is in bridge's event loop
	ch := bus.Subscribe(sessionID, 64)
	for e := range ch {
		handler(e)
	}
}

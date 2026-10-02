package miniapp

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"ceraa/harness/event"
)

// maxEventLog bounds what a reader can ask to have replayed.
//
// A reader that lost its connection resumes from a number it already holds,
// and a personal surface has no reason to reach back further than a few
// hundred events. The bound is what keeps the buffer tracking the length of a
// reconnect gap instead of the length of the session.
const maxEventLog = 512

// loggedEvent is one bus event with the number that orders it.
//
// The sequence is what makes a gap detectable rather than silent. A reader
// asks for everything after a number it already has, and the answer either
// continues from there or says the entry has aged out. A reader that cannot be
// told the difference would render a transcript with a hole in it and no way
// to know one is there.
type loggedEvent struct {
	Seq   int64       `json:"seq"`
	Event event.Event `json:"event"`
}

// eventLog is the replay buffer both transports read from.
//
// One subscriber fills it for the life of the process and the HTTP handlers
// only read. Buffering per request instead would leave a hole in the interval
// between two requests, which is exactly the interval a reader that has just
// reconnected is asking about.
type eventLog struct {
	mu     sync.Mutex
	seq    int64
	items  []loggedEvent
	signal chan struct{}
}

func newEventLog() *eventLog {
	return &eventLog{signal: make(chan struct{})}
}

// append files one event and wakes every reader waiting for something new.
//
// The signal is closed and replaced rather than written to, because a waiter
// that is between checks would otherwise miss a send that no longer has a
// reader to receive it.
func (l *eventLog) append(one event.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.items = append(l.items, loggedEvent{Seq: l.seq, Event: one})
	if len(l.items) > maxEventLog {
		l.items = l.items[len(l.items)-maxEventLog:]
	}
	wake := l.signal
	l.signal = make(chan struct{})
	close(wake)
}

// head is the newest number issued, zero before anything has been filed.
func (l *eventLog) head() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// since returns what a reader positioned after this number has not seen, plus
// the channel that fires when the next event is filed.
//
// stale reports that the number is older than the buffer reaches back. Being
// told is better than being skipped: the caller re-reads the transcript from
// disk instead of continuing from a gap it cannot see.
func (l *eventLog) since(after int64) (items []loggedEvent, stale bool, wake <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.items) > 0 && after < l.items[0].Seq-1 {
		stale = true
	}
	for _, one := range l.items {
		if one.Seq > after {
			items = append(items, one)
		}
	}
	return items, stale, l.signal
}

// cursorFrom is where a reader wants to resume from.
//
// A browser sets Last-Event-ID on its own when it reconnects, so a stream that
// dropped can come back without the page having to remember anything. A poll
// asks for the same value by name because it has no header to carry it. When
// neither is present the answer is the head rather than the beginning: a
// reader that has just arrived wants what happens next, not a replay of
// everything it never saw.
func cursorFrom(r *http.Request, head int64) (int64, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return head, true
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ensureLog starts the one subscriber that fills the buffer.
//
// It runs on demand rather than in New, because New builds servers that a test
// drives through httptest without ever binding a socket, and a goroutine
// started there would outlive the test that never asked for it.
func (s *Server) ensureLog() {
	s.logOnce.Do(func() {
		if s.cfg.Bus == nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.logMu.Lock()
		s.logStop = cancel
		s.logMu.Unlock()
		sub := s.cfg.Bus.Subscribe(event.Wildcard, 1024)
		go func() {
			defer s.cfg.Bus.Unsubscribe(event.Wildcard, sub)
			for {
				select {
				case <-ctx.Done():
					return
				case one, open := <-sub:
					if !open {
						return
					}
					s.log.append(one)
				}
			}
		}()
	})
}

// stopLog ends the subscriber so a shutdown does not leave one running.
func (s *Server) stopLog() {
	s.logMu.Lock()
	cancel := s.logStop
	s.logStop = nil
	s.logMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

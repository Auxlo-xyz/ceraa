package tracker

import (
	"fmt"
	"sync"
	"time"
)

// RunningToolTracker maps to soraacode bot/streaming/running-tool-tracker.js
// Tracks elapsed time for running tools, tick every 10s, max 2h

const (
	Threshold = 10 * time.Second
	MaxDuration = 2 * time.Hour
)

type Tracker struct {
	mu    sync.Mutex
	start map[string]time.Time
	stop  map[string]chan struct{}
}

func New() *Tracker {
	return &Tracker{
		start: make(map[string]time.Time),
		stop:  make(map[string]chan struct{}),
	}
}

func (t *Tracker) Start(id, tool string, onTick func(string)) {
	t.mu.Lock()
	t.start[id] = time.Now()
	ch := make(chan struct{})
	t.stop[id] = ch
	t.mu.Unlock()

	go func() {
		ticker := time.NewTicker(Threshold)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.mu.Lock()
				start, ok := t.start[id]
				t.mu.Unlock()
				if !ok {
					return
				}
				elapsed := time.Since(start)
				if elapsed > MaxDuration {
					return
				}
				if onTick != nil {
					onTick(fmt.Sprintf("⏳ %s running %s", tool, elapsed.Round(time.Second)))
				}
			case <-ch:
				return
			}
		}
	}()
}

func (t *Tracker) Stop(id string) {
	t.mu.Lock()
	if ch, ok := t.stop[id]; ok {
		close(ch)
		delete(t.stop, id)
		delete(t.start, id)
	}
	t.mu.Unlock()
}

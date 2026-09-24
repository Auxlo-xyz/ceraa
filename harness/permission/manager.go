package permission

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Manager struct {
	mu      sync.Mutex
	pending map[string]chan string
	always  map[string]map[string]bool
}

func NewManager() *Manager {
	return &Manager{pending: make(map[string]chan string), always: make(map[string]map[string]bool)}
}

var Default = NewManager()

func key(sessionID, requestID string) string {
	return sessionID + ":" + requestID
}

func (m *Manager) Create(parent context.Context, sessionID, requestID, toolName string, timeout time.Duration, ready func()) (string, error) {
	m.mu.Lock()
	if m.always[toolName]["allowAlways"] {
		m.mu.Unlock()
		return "allowAlways", nil
	}
	ch := make(chan string, 1)
	m.pending[key(sessionID, requestID)] = ch
	m.mu.Unlock()
	if ready != nil {
		ready()
	}
	defer func() {
		m.mu.Lock()
		delete(m.pending, key(sessionID, requestID))
		m.mu.Unlock()
	}()
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	select {
	case decision := <-ch:
		return decision, nil
	case <-parent.Done():
		return "", parent.Err()
	case <-time.After(timeout):
		return "", fmt.Errorf("permission timeout after %s", timeout)
	}
}

func (m *Manager) Answer(sessionID, requestID, decision string) bool {
	m.mu.Lock()
	ch, ok := m.pending[key(sessionID, requestID)]
	m.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- decision:
		return true
	default:
		return false
	}
}

func (m *Manager) AllowAlways(toolName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.always[toolName] == nil {
		m.always[toolName] = make(map[string]bool)
	}
	m.always[toolName]["allowAlways"] = true
}

func (m *Manager) ClearAlways(toolName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.always, toolName)
}

func (m *Manager) IsPending(sessionID, requestID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pending[key(sessionID, requestID)]
	return ok
}

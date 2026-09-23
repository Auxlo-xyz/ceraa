package question

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Manager handles pending questions, maps to soraacode question-manager.js + question.ts
type Manager struct {
	mu      sync.Mutex
	pending map[string]chan string // key: sessionID:requestID
}

func NewManager() *Manager {
	return &Manager{pending: make(map[string]chan string)}
}

var Default = NewManager()

func key(sessionID, requestID string) string { return sessionID + ":" + requestID }

// Create creates a pending question and waits for answer, like soraacode question poll
func (m *Manager) Create(sessionID, requestID, question string, options []string, timeout time.Duration) (string, error) {
	return m.CreateWithContext(context.Background(), sessionID, requestID, question, options, timeout, nil)
}

func (m *Manager) CreateWithReady(sessionID, requestID string, timeout time.Duration, ready func()) (string, error) {
	return m.CreateWithContext(context.Background(), sessionID, requestID, "", nil, timeout, ready)
}

func (m *Manager) CreateWithContext(parent context.Context, sessionID, requestID, question string, options []string, timeout time.Duration, ready func()) (string, error) {
	return m.create(parent, sessionID, requestID, timeout, ready)
}

func (m *Manager) create(parent context.Context, sessionID, requestID string, timeout time.Duration, ready func()) (string, error) {
	ch := make(chan string, 1)
	k := key(sessionID, requestID)
	m.mu.Lock()
	m.pending[k] = ch
	m.mu.Unlock()
	if ready != nil {
		ready()
	}
	defer func() {
		m.mu.Lock()
		delete(m.pending, k)
		m.mu.Unlock()
	}()
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	select {
	case ans := <-ch:
		return ans, nil
	case <-parent.Done():
		return "", parent.Err()
	case <-time.After(timeout):
		return "", fmt.Errorf("question timeout after %s", timeout)
	}
}

// Answer resolves a pending question, called from telegram callback
func (m *Manager) Answer(sessionID, requestID, answer string) bool {
	k := key(sessionID, requestID)
	m.mu.Lock()
	ch, ok := m.pending[k]
	m.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- answer:
		return true
	default:
		return false
	}
}

// IsPending checks if question exists
func (m *Manager) IsPending(sessionID, requestID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pending[key(sessionID, requestID)]
	return ok
}

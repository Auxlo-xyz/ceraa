package question

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Manager struct {
	mu      sync.Mutex
	pending map[string]chan string
}

func NewManager() *Manager {
	return &Manager{pending: make(map[string]chan string)}
}

var Default = NewManager()

func key(sessionID, requestID string) string { return sessionID + ":" + requestID }

func (m *Manager) Create(sessionID, requestID, question string, options []string, timeout time.Duration) (string, error) {
	return m.CreateWithContext(context.Background(), sessionID, requestID, question, options, timeout, nil)
}

func (m *Manager) CreateWithReady(sessionID, requestID string, timeout time.Duration, ready func()) (string, error) {
	return m.CreateWithContext(context.Background(), sessionID, requestID, "", nil, timeout, ready)
}

func (m *Manager) CreateWithContext(parent context.Context, sessionID, requestID, question string, options []string, timeout time.Duration, ready func()) (string, error) {
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
	case answer := <-ch:
		return answer, nil
	case <-parent.Done():
		return "", parent.Err()
	case <-time.After(timeout):
		return "", fmt.Errorf("question timeout after %s", timeout)
	}
}

func (m *Manager) Answer(sessionID, requestID, answer string) bool {
	m.mu.Lock()
	ch, ok := m.pending[key(sessionID, requestID)]
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

func (m *Manager) Pending(sessionID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := sessionID + ":"
	out := []string{}
	for k := range m.pending {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k[len(prefix):])
		}
	}
	sort.Strings(out)
	return out
}

func (m *Manager) IsPending(sessionID, requestID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pending[key(sessionID, requestID)]
	return ok
}

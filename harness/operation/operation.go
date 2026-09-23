package operation

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"soraa-agent/harness/primitives"
)

// Spec describes work to be done.
type Spec struct {
	Type string         `json:"type"`
	Args map[string]any `json:"args,omitempty"`
}

// Status tracks the lifecycle of an operation.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Operation is the unit of durable work.
type Operation struct {
	ID     string `json:"id"`
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Manager runs operations. Results stream back on Results().
type Manager interface {
	Add(op Operation)
	Cancel(id string)
	Results() <-chan ResultEvent
}

// ResultEvent fires when an operation completes.
type ResultEvent struct {
	OpID   string
	Output string
	Error  string
	Status Status
}

// LocalManager runs operations in goroutines within the current process.
type LocalManager struct {
	mu      sync.Mutex
	results chan ResultEvent
	cancels map[string]context.CancelCauseFunc
}

func New() *LocalManager {
	return &LocalManager{
		results: make(chan ResultEvent, 64),
		cancels: make(map[string]context.CancelCauseFunc),
	}
}

func (m *LocalManager) Results() <-chan ResultEvent {
	return m.results
}

func (m *LocalManager) Cancel(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.cancels[id]; ok {
		cancel(context.Canceled)
		delete(m.cancels, id)
	}
}

// Add dispatches an operation for execution and returns immediately.
func (m *LocalManager) Add(op Operation) {
	m.mu.Lock()
	if _, exists := m.cancels[op.ID]; exists {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	m.cancels[op.ID] = cancel
	m.mu.Unlock()

	go m.execute(ctx, op)
}

func (m *LocalManager) execute(ctx context.Context, op Operation) {
	result := ResultEvent{OpID: op.ID}

	select {
	case <-ctx.Done():
		result.Status = StatusCanceled
		result.Error = "canceled"
	default:
	}

	select {
	case <-ctx.Done():
		result.Status = StatusCanceled
		result.Error = "canceled"
	default:
		switch op.Spec.Type {
		case "shell":
			cmd, _ := op.Spec.Args["command"].(string)
			to := 60 * time.Second
			if t, ok := op.Spec.Args["timeout"]; ok {
				if ts, ok := t.(int); ok {
					to = time.Duration(ts) * time.Second
				}
			}
			r, err := primitives.RunShell(cmd, "", to)
			result.Output = r.Content
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "read":
			p, _ := op.Spec.Args["path"].(string)
			r, err := primitives.ReadFile(p)
			result.Output = r.Content
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "write":
			p, _ := op.Spec.Args["path"].(string)
			c, _ := op.Spec.Args["content"].(string)
			if d := filepath.Dir(p); d != "" {
				_ = os.MkdirAll(d, 0o755)
			}
			if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
				result.Output = "written"
			}
		case "glob":
			p, _ := op.Spec.Args["pattern"].(string)
			r, _ := primitives.GlobFiles(p)
			result.Output = r.Content
			result.Status = StatusCompleted
		case "view_image":
			result.Output = "image-view: (stub) - view via Telegram"
			result.Status = StatusCompleted
		default:
			result.Error = "unknown operation type: " + op.Spec.Type
			result.Status = StatusFailed
		}
	}

	m.mu.Lock()
	delete(m.cancels, op.ID)
	m.mu.Unlock()

	select {
	case m.results <- result:
	case <-time.After(5 * time.Second):
	}
}

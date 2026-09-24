// Package sessionstore persists session history and operation state.
// Each session is stored as an atomically replaced JSON document with recovery checkpoints.
// Fork support creates a new session by copying history up to a turn boundary.
package sessionstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sooraa/harness/flock"
	"sooraa/harness/llm"
)

const schemaVersion = 3

type OperationStatus string

const (
	StatusPending         OperationStatus = "pending"
	StatusRunning         OperationStatus = "running"
	StatusCompleted       OperationStatus = "completed"
	StatusFailed          OperationStatus = "failed"
	StatusCanceled        OperationStatus = "canceled"
	StatusUnknown         OperationStatus = "unknown"
	StatusCancelRequested OperationStatus = "cancel_requested"
	StatusCanceledUnknown OperationStatus = "canceled_unknown"
)

type RunStatus string

const (
	RunRunning     RunStatus = "running"
	RunCompleted   RunStatus = "completed"
	RunFailed      RunStatus = "failed"
	RunCanceled    RunStatus = "canceled"
	RunInterrupted RunStatus = "interrupted"
)

type Operation struct {
	ID            string          `json:"id"`
	Tool          string          `json:"tool"`
	Args          map[string]any  `json:"args,omitempty"`
	Status        OperationStatus `json:"status"`
	Output        string          `json:"output,omitempty"`
	Error         string          `json:"error,omitempty"`
	ThreadID      string          `json:"thread_id,omitempty"`
	RunID         string          `json:"run_id,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	SpecType      string          `json:"spec_type,omitempty"`
	SpecArgs      map[string]any  `json:"spec_args,omitempty"`
	ResultPresent bool            `json:"result_present,omitempty"`
}

type Message struct {
	Role              llm.Role        `json:"role"`
	Content           string          `json:"content,omitempty"`
	ToolCallID        string          `json:"tool_call_id,omitempty"`
	ToolResult        string          `json:"tool_result,omitempty"`
	ToolResultPresent bool            `json:"tool_result_present,omitempty"`
	ResultStatus      OperationStatus `json:"result_status,omitempty"`
	ToolCall          *ToolCallRef    `json:"tool_call,omitempty"`
	ToolCalls         []ToolCallRef   `json:"tool_calls,omitempty"`
	Reasoning         string          `json:"reasoning,omitempty"`
	Compaction        *CompactionInfo `json:"compaction,omitempty"`
}

type CompactionInfo struct {
	Summary string `json:"summary"`
	Recent  string `json:"recent,omitempty"`
	Seq     int    `json:"seq"`
}

type ToolCallRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"arguments"`
}

func (m Message) AllToolCalls() []ToolCallRef {
	if len(m.ToolCalls) > 0 {
		return m.ToolCalls
	}
	if m.ToolCall != nil {
		return []ToolCallRef{*m.ToolCall}
	}
	return nil
}

type Run struct {
	ID                 string             `json:"id"`
	User               string             `json:"user,omitempty"`
	Status             RunStatus          `json:"status"`
	Step               int                `json:"step"`
	Requests           int                `json:"requests"`
	Usage              llm.UsageInfo      `json:"usage,omitempty"`
	Cost               llm.CostInfo       `json:"cost"`
	InputBreakdown     llm.TokenBreakdown `json:"input_breakdown,omitempty"`
	PricingModel       string             `json:"pricing_model,omitempty"`
	PricingStatus      string             `json:"pricing_status,omitempty"`
	PricingFetchedAt   time.Time          `json:"pricing_fetched_at,omitempty"`
	ModelContextWindow int                `json:"model_context_window,omitempty"`
	ModelMaxTokens     int                `json:"model_max_tokens,omitempty"`
	ReasoningOptions   []string           `json:"reasoning_options,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	Error              string             `json:"error,omitempty"`
}

type Session struct {
	ID         string      `json:"id"`
	Messages   []Message   `json:"messages"`
	Operations []Operation `json:"operations"`
	Runs       []Run       `json:"runs,omitempty"`
	Version    int         `json:"version"`
	ParentID   string      `json:"parent_id,omitempty"`
}

func (s *Session) Clear() error {
	s.Messages = []Message{}
	s.Operations = []Operation{}
	s.Runs = []Run{}
	return nil
}

type Store struct {
	mu       sync.Mutex
	dir      string
	fileLock *flock.Lock
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) AcquireRun(id string) (func(), error) {
	hash := sha256.Sum256([]byte(id))
	path := filepath.Join(s.dir, ".run-"+hex.EncodeToString(hash[:])+".lock")
	lock, err := flock.Acquire(path)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *Store) Load(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return nil, err
	}
	defer s.unlock()
	return s.loadUnlocked(id)
}

func (s *Store) loadUnlocked(id string) (*Session, error) {
	path := s.path(id)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Session{ID: id, Version: schemaVersion}, nil
		}
		return nil, fmt.Errorf("read session %s: %w", id, err)
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, fmt.Errorf("decode session %s: %w", id, err)
	}
	if sess.Version == 0 {
		sess.Version = schemaVersion
	}
	if sess.Messages == nil {
		sess.Messages = []Message{}
	}
	if sess.Operations == nil {
		sess.Operations = []Operation{}
	}
	if sess.Runs == nil {
		sess.Runs = []Run{}
	}
	return &sess, nil
}

func (s *Store) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return err
	}
	defer s.unlock()
	return s.saveUnlocked(sess)
}

func (s *Store) saveUnlocked(sess *Session) error {
	if sess.Version == 0 {
		sess.Version = schemaVersion
	}
	b, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %s: %w", sess.ID, err)
	}
	return atomicWriteFile(s.path(sess.ID), b, 0o644)
}

func (s *Store) lock() error {
	lockPath := filepath.Join(s.dir, ".lock")
	lock, err := flock.Acquire(lockPath)
	if err != nil {
		return fmt.Errorf("lock session store: %w", err)
	}
	s.fileLock = lock
	return nil
}

func (s *Store) unlock() {
	if s.fileLock != nil {
		_ = s.fileLock.Release()
		s.fileLock = nil
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp session: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp session: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp session: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace session: %w", err)
	}
	removeTemp = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func (s *Store) Update(id string, update func(*Session) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return err
	}
	defer s.unlock()
	sess, err := s.loadUnlocked(id)
	if err != nil {
		return err
	}
	if err := update(sess); err != nil {
		return err
	}
	return s.saveUnlocked(sess)
}

func (s *Store) Clear(id string) error {
	return s.Update(id, func(sess *Session) error {
		sess.Messages = []Message{}
		sess.Operations = []Operation{}
		sess.Runs = []Run{}
		return nil
	})
}

func Recover(sess *Session) bool {
	changed := false
	if sess.Messages == nil {
		sess.Messages = []Message{}
	}
	if sess.Operations == nil {
		sess.Operations = []Operation{}
	}
	if sess.Runs == nil {
		sess.Runs = []Run{}
	}

	operations := make(map[string]*Operation, len(sess.Operations))
	for i := range sess.Operations {
		op := &sess.Operations[i]
		key := op.ToolCallID
		if key == "" {
			key = op.ID
		}
		if key != "" {
			operations[key] = op
		}
		if op.Status == StatusPending || op.Status == StatusRunning || op.Status == StatusCancelRequested {
			op.Status = StatusUnknown
			op.Error = "operation outcome is unknown after restart"
			changed = true
		}
		if (op.Status == StatusCompleted || op.Status == StatusFailed || op.Status == StatusCanceled || op.Status == StatusUnknown) && !op.ResultPresent {
			op.ResultPresent = true
			changed = true
		}
	}
	for i := range sess.Runs {
		if sess.Runs[i].Status == RunRunning {
			sess.Runs[i].Status = RunInterrupted
			sess.Runs[i].Error = "run interrupted before completion"
			changed = true
		}
	}

	results := make(map[string]bool)
	for _, message := range sess.Messages {
		if message.Role == llm.RoleTool && message.ToolCallID != "" {
			if message.ToolResultPresent || message.ToolResult != "" {
				results[message.ToolCallID] = true
			}
		}
	}
	recovered := make([]Message, 0, len(sess.Messages))
	for _, message := range sess.Messages {
		if message.Role == llm.RoleTool && message.ToolCallID != "" && !message.ToolResultPresent && message.ToolResult != "" {
			message.ToolResultPresent = true
			changed = true
		}
		recovered = append(recovered, message)
		for _, call := range message.AllToolCalls() {
			if results[call.ID] {
				continue
			}
			result := "Error: operation outcome is unknown after restart"
			status := StatusUnknown
			if op := operations[call.ID]; op != nil {
				if op.Output != "" || op.ResultPresent || op.Status == StatusCompleted || op.Status == StatusFailed || op.Status == StatusCanceled {
					if op.Output != "" {
						result = op.Output
					} else if op.Error != "" {
						result = "Error: " + op.Error
					}
					status = op.Status
				}
			}
			recovered = append(recovered, Message{
				Role:              llm.RoleTool,
				ToolCallID:        call.ID,
				ToolResult:        result,
				ToolResultPresent: true,
				ResultStatus:      status,
			})
			results[call.ID] = true
			changed = true
		}
	}
	sess.Messages = recovered
	return changed
}

func (s *Store) List() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, filepath.Base(m[:len(m)-len(filepath.Ext(m))]))
	}
	return ids, nil
}

// Fork creates a new session copying the parent's messages up to and including
// the message at the given index. Operations are not copied.
func (s *Store) Fork(parentID string, messageIndex int) (*Session, error) {
	parent, err := s.Load(parentID)
	if err != nil {
		return nil, err
	}
	if messageIndex >= len(parent.Messages) {
		return nil, fmt.Errorf("message index %d out of range (len=%d)", messageIndex, len(parent.Messages))
	}
	childID := parentID + "-fork-" + fmt.Sprintf("%d", len(parent.Messages))
	child := &Session{
		ID:         childID,
		ParentID:   parentID,
		Version:    schemaVersion,
		Messages:   make([]Message, messageIndex+1),
		Operations: []Operation{},
	}
	copy(child.Messages, parent.Messages[:messageIndex+1])
	if err := s.Save(child); err != nil {
		return nil, err
	}
	return child, nil
}

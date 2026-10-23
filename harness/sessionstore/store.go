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
	"strings"
	"sync"
	"time"

	"ceraa/harness/flock"
	"ceraa/harness/llm"
	"ceraa/harness/redact"
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
	ID     string          `json:"id"`
	Tool   string          `json:"tool"`
	Args   map[string]any  `json:"args,omitempty"`
	Status OperationStatus `json:"status"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
	// Images are image paths this operation produced, kept as paths so a session
	// file does not carry a copy of every screenshot it has seen.
	Images        []string       `json:"images,omitempty"`
	ThreadID      string         `json:"thread_id,omitempty"`
	RunID         string         `json:"run_id,omitempty"`
	ToolCallID    string         `json:"tool_call_id,omitempty"`
	SpecType      string         `json:"spec_type,omitempty"`
	SpecArgs      map[string]any `json:"spec_args,omitempty"`
	ResultPresent bool           `json:"result_present,omitempty"`
}

type Message struct {
	Role              llm.Role `json:"role"`
	Content           string   `json:"content,omitempty"`
	ToolCallID        string   `json:"tool_call_id,omitempty"`
	ToolName          string   `json:"tool_name,omitempty"`
	ToolResult        string   `json:"tool_result,omitempty"`
	ToolResultPresent bool     `json:"tool_result_present,omitempty"`
	// ToolImages are image paths a read produced, kept as paths so a session file
	// does not carry a copy of every screenshot it has ever seen.
	ToolImages       []string        `json:"tool_images,omitempty"`
	ResultStatus     OperationStatus `json:"result_status,omitempty"`
	ToolCall         *ToolCallRef    `json:"tool_call,omitempty"`
	ToolCalls        []ToolCallRef   `json:"tool_calls,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	Usage            llm.UsageInfo   `json:"usage,omitempty"`
	Compaction       *CompactionInfo `json:"compaction,omitempty"`
}

type CompactionInfo struct {
	Summary string `json:"summary"`
	Recent  string `json:"recent,omitempty"`
	Seq     int    `json:"seq"`
	// Tier and Generation drive tier distillation: new folds land at tier 1
	// young, survivors promote to old, and old blocks of one tier merge into
	// the next. Zero values read as tier 1 young, so checkpoints written
	// before tiers existed need no migration.
	Tier       int    `json:"tier,omitempty"`
	Generation string `json:"generation,omitempty"`
	Survived   int    `json:"survived,omitempty"`
	// Delivered is the delivered set as the last fold judged it: one entry
	// content-hash id per line.
	//
	// Vestigial. It existed so a fold could record what the model had been told
	// about, and the memory feature it served is retired. The compaction carry
	// chain still threads it (carry at compaction.go, and CheckpointDelivered
	// reads it back), but the Carry hook that would have populated it from the
	// memory side has no non-test caller, so carry is a pass-through and
	// nothing in the product reads the set for any purpose.
	//
	// Kept rather than deleted because removing it means changing the fold carry
	// chain, which is compaction behaviour and out of scope for the memory
	// retirement. It is a removal candidate on its own terms.
	//
	// omitempty so a checkpoint with nothing to say writes the bytes it always
	// wrote and a session stored before this field loads unchanged: an absent
	// key is an empty set, schemaVersion stays 3, loadUnlocked needs no default
	// and Recover needs no branch.
	Delivered []string `json:"delivered,omitempty"`
}

type ToolCallRef struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Args             string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
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

// CheckpointDelivered is every delivered id the standing checkpoints carry, in
// first-seen order with blanks and repeats dropped.
//
// It is the union across checkpoints rather than the newest set because a fold
// keeps every earlier checkpoint standing, and the newest one is not always the
// one holding an id: a checkpoint written before this field existed carries
// none, and a later fold can decline to carry. Taking the union is what makes
// the result monotone, which is the direction that loses nothing silently.
func (s *Session) CheckpointDelivered() []string {
	if s == nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, message := range s.Messages {
		if message.Compaction == nil {
			continue
		}
		for _, id := range message.Compaction.Delivered {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

type Run struct {
	ID             string             `json:"id"`
	User           string             `json:"user,omitempty"`
	Status         RunStatus          `json:"status"`
	Step           int                `json:"step"`
	Requests       int                `json:"requests"`
	Usage          llm.UsageInfo      `json:"usage,omitempty"`
	Cost           llm.CostInfo       `json:"cost"`
	InputBreakdown llm.TokenBreakdown `json:"input_breakdown,omitempty"`
	// Requests records the per-request split, not just the run total. The total
	// says a run cost 197,084 tokens of tool output; this says which request made
	// it jump, which is the difference between an opportunity that can be acted on
	// and one that can only be sized.
	//
	// The counts are the local estimate, computed from the messages actually sent,
	// so they are available before the provider answers and are trustworthy even
	// when its own reporting is a running total. A run that never reaches a
	// request keeps an empty list rather than a zero entry.
	RequestBreakdown []llm.RequestTokenBreakdown `json:"request_breakdown,omitempty"`
	// UsageTrust records whether the provider's token counts describe these
	// requests. Some endpoints report a running total instead, which makes both
	// the cache ratio and the cost fiction, so cost is left unpriced when it does
	// not.
	UsageTrust   llm.UsageTrust `json:"usage_trust,omitempty"`
	PricingModel string         `json:"pricing_model,omitempty"`
	// ProviderHost is the endpoint the run was billed by, so cache behaviour can
	// be attributed to a route rather than only to a model name.
	ProviderHost  string `json:"provider_host,omitempty"`
	PricingStatus string `json:"pricing_status,omitempty"`
	// PricingFetchedAt and the model envelope belong to the run rather than the
	// session: they are set when the run is made, and two runs of one session
	// can disagree about the model they used.
	PricingFetchedAt   time.Time `json:"pricing_fetched_at,omitempty"`
	ModelContextWindow int       `json:"model_context_window,omitempty"`
	ModelMaxTokens     int       `json:"model_max_tokens,omitempty"`
	ReasoningOptions   []string  `json:"reasoning_options,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Error     string    `json:"error,omitempty"`
}

type Session struct {
	ID               string           `json:"id"`
	Title            string           `json:"title,omitempty"`
	TitleSource      string           `json:"title_source,omitempty"`
	TitleGeneratedAt time.Time        `json:"title_generated_at,omitempty"`
	TitleAttempts    int              `json:"title_attempts,omitempty"`
	TitleLastAttempt time.Time        `json:"title_last_attempt,omitempty"`
	Messages         []Message        `json:"messages"`
	Operations       []Operation      `json:"operations"`
	Runs             []Run            `json:"runs,omitempty"`
	RunEvents        []RunEvent       `json:"run_events,omitempty"`
	Attempts         []Attempt        `json:"attempts,omitempty"`
	ExternalActions  []ExternalAction `json:"external_actions,omitempty"`
	Version          int              `json:"version"`
	ParentID         string           `json:"parent_id,omitempty"`

	// PrefixRewrites counts how many times this session has rewritten the front
	// of the prompt: a fold, a tier merge, or a checkpoint-chain roll.
	//
	// It is the observable half of the cache contract. Everything inside an
	// epoch is append-only, so a request is the previous request with a suffix
	// and the provider reads the whole prefix warm; a rewrite is the one event
	// that breaks that, and it should be rare, deliberate and countable. A
	// number that climbs faster than the number of folds is a second writer
	// touching the front of the history, which is the bug this exists to make
	// visible.
	//
	// omitempty so a session that has never rewritten writes the bytes it always
	// wrote and loads unchanged with schemaVersion 3.
	PrefixRewrites int `json:"prefix_rewrites,omitempty"`

	// PrefixSignature is the shape of the last request this session sent: one
	// entry per message, its kind and its length. It exists so the first request
	// of the next run can be compared against the last request of the previous
	// one.
	//
	// The comparison cannot live in the client, because a client is built per
	// run and its in-memory prefix state dies with it. It cannot live in a
	// process global either, because a subagent session interleaving with the
	// main one corrupts it: the audit then reports a divergence at item1 for a
	// conversation whose prefix never moved. Per session is the only scope where
	// the thing being compared is actually the same conversation.
	PrefixSignature string `json:"prefix_signature,omitempty"`
	// PrefixSignatureEpoch is PrefixRewrites as of the moment PrefixSignature
	// was written. A divergence is only a regression when the epoch matches: a
	// fold or a chain roll is supposed to change the front of the history, and
	// warning about it would bury the warning that matters.
	PrefixSignatureEpoch int `json:"prefix_signature_epoch,omitempty"`
}

func (s *Session) DisplayTitle() string {
	if title := strings.TrimSpace(s.Title); title != "" {
		return truncateTitle(title)
	}
	for _, message := range s.Messages {
		if message.Role != llm.RoleUser {
			continue
		}
		if text := strings.TrimSpace(redact.SecretsInText(message.Content)); text != "" {
			return truncateTitle(text)
		}
	}
	return "Untitled session"
}

func truncateTitle(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 56 {
		return string(runes[:53]) + "..."
	}
	return value
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lock(); err != nil {
		return err
	}
	defer s.unlock()
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	for _, suffix := range []string{".compacted.json", ".todos.json", ".goal.json"} {
		if err := os.Remove(filepath.Join(s.dir, id+suffix)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete session %s sidecar %s: %w", id, suffix, err)
		}
	}
	return nil
}

func (s *Session) Clear() error {
	s.Messages = []Message{}
	s.Operations = []Operation{}
	s.Runs = []Run{}
	s.ExternalActions = []ExternalAction{}
	// The rewrite counter goes with the transcript: an empty session has rewritten
	// nothing.
	s.PrefixRewrites = 0
	s.PrefixSignature = ""
	s.PrefixSignatureEpoch = 0
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

// Dir is where this store keeps its files, and the sidecars beside them: the
// compaction archive lives next to the session it summarises, so a tool that
// reads the archive resolves it from here rather than being told a second
// path that can drift from this one.
func (s *Store) Dir() string {
	return s.dir
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

// Exists reports whether a session has ever been written.
//
// Load answers an unknown id with an empty session rather than an error, which
// is right for the caller about to start one and wrong for a handler routing on
// the id: there a mistyped conversation and a brand new one are the same
// response. This is the distinction Load deliberately does not make.
func (s *Store) Exists(id string) bool {
	info, err := os.Stat(s.path(id))
	return err == nil && !info.IsDir()
}

// LastModified reports when a session was last written, without loading it.
//
// The idle sweeper uses it as last activity: every completed turn saves, so
// the file's mtime is the last time the conversation moved. No schema change,
// no migration: a timestamp field would need backfilling for every existing
// session, and the filesystem already records it.
func (s *Store) LastModified(id string) (time.Time, error) {
	info, err := os.Stat(s.path(id))
	if err != nil {
		return time.Time{}, fmt.Errorf("stat session %s: %w", id, err)
	}
	return info.ModTime(), nil
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
	if sess.RunEvents == nil {
		sess.RunEvents = []RunEvent{}
	}
	if sess.Attempts == nil {
		sess.Attempts = []Attempt{}
	}
	if sess.ExternalActions == nil {
		sess.ExternalActions = []ExternalAction{}
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
	// Reasoning is stored one block per turn, not one per streamed chunk. A turn
	// that streamed a thousand chunks used to persist a thousand entries, each
	// repeating type/format/index, and the trim that measures stored bytes cut a
	// real conversation because of them. Normalizing on write repairs sessions
	// recorded before the merge learned to coalesce, at no cost to the text.
	for i := range sess.Messages {
		if len(sess.Messages[i].ReasoningDetails) == 0 {
			continue
		}
		sess.Messages[i].ReasoningDetails = llm.NormalizeReasoningDetails(sess.Messages[i].ReasoningDetails)
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
	if sess.RunEvents == nil {
		sess.RunEvents = []RunEvent{}
	}
	if sess.ExternalActions == nil {
		sess.ExternalActions = []ExternalAction{}
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
			if event, ok := sess.CurrentRunEvent(sess.Runs[i].ID); !ok || event.Status != RunInterrupted {
				sess.AppendRunEvent(RunEvent{Type: RunEventInterrupted, RunID: sess.Runs[i].ID, Status: RunInterrupted, CreatedAt: time.Now(), Error: sess.Runs[i].Error})
			}
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
				ToolName:          call.Name,
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
		ID:              childID,
		ParentID:        parentID,
		Version:         schemaVersion,
		Messages:        make([]Message, messageIndex+1),
		Operations:      []Operation{},
		ExternalActions: []ExternalAction{},
	}
	copy(child.Messages, parent.Messages[:messageIndex+1])
	if err := s.Save(child); err != nil {
		return nil, err
	}
	return child, nil
}

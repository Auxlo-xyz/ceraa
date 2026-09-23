package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sooraa/harness/event"
	"sooraa/harness/primitives"
	"sooraa/harness/question"
	"sooraa/harness/redact"
	"sooraa/harness/search"
	"sooraa/harness/snapshot"
	"sooraa/harness/todo"
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

// ToolRunner allows tools to be executed by the operation manager.
type ToolRunner interface {
	RunTool(ctx context.Context, spec Spec) (string, error)
}

// LocalManager runs operations in goroutines within the current process.
type LocalManager struct {
	mu      sync.Mutex
	results chan ResultEvent
	cancels map[string]context.CancelCauseFunc
	runner  ToolRunner
}

// New creates a LocalManager. If runner is nil, only built-in types (shell, read, write, glob) are supported.
func New(runner ToolRunner) *LocalManager {
	return &LocalManager{
		results: make(chan ResultEvent, 64),
		cancels: make(map[string]context.CancelCauseFunc),
		runner:  runner,
	}
}

func (m *LocalManager) Results() <-chan ResultEvent {
	return m.results
}

func (m *LocalManager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.cancels {
		cancel(context.Canceled)
		delete(m.cancels, id)
	}
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
			r, err := primitives.RunShellContext(ctx, cmd, "", to)
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
			// Snapshot before write -> maps to soraacode snapshot.ts + revert.ts
			if snapStore, err := newSnapshotStore(); err == nil {
				_, _ = snapStore.Capture(p)
			}
			if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
				result.Output = "written"
			}
		case "todowrite":
			// Maps to soraacode session/todo.ts TodoWrite
			todosRaw, _ := op.Spec.Args["todos"]
			sessionID, _ := op.Spec.Args["session_id"].(string)
			if sessionID == "" {
				sessionID = "default"
			}
			// Save via todo store
			if dir, err := todoStoreDir(); err == nil {
				if store, err := newTodoStore(dir); err == nil {
					// Marshal and save
					if data, err := json.Marshal(todosRaw); err == nil {
						var todos []todo.Todo
						if err := json.Unmarshal(data, &todos); err == nil {
							_ = store.Save(sessionID, todos)
							result.Output = fmt.Sprintf("Todos updated: %d items", len(todos))
							result.Status = StatusCompleted
							break
						}
					}
				}
			}
			result.Error = "todowrite failed"
			result.Status = StatusFailed
		case "glob":
			p, _ := op.Spec.Args["pattern"].(string)
			r, _ := primitives.GlobFiles(p)
			result.Output = r.Content
			result.Status = StatusCompleted
		case "grep":
			pattern, _ := op.Spec.Args["pattern"].(string)
			path, _ := op.Spec.Args["path"].(string)
			if path == "" {
				path = "."
			}
			// Use ripgrep if available, fallback to grep
			var cmd string
			if _, err := os.Stat("/usr/bin/rg"); err == nil {
				cmd = fmt.Sprintf("rg --no-heading -n --max-count 100 --glob '!.git/*' %q %q 2>&1 | head -c 4000", pattern, path)
			} else if _, err := exec.LookPath("rg"); err == nil {
				cmd = fmt.Sprintf("rg --no-heading -n --max-count 100 --glob '!.git/*' %q %q 2>&1 | head -c 4000", pattern, path)
			} else {
				cmd = fmt.Sprintf("grep -rn --include='*.go' --include='*.md' --include='*.json' --include='*.txt' %q %q 2>&1 | head -c 4000", pattern, path)
			}
			r, _ := primitives.RunShell(cmd, "", 30*time.Second)
			result.Output = r.Content
			// grep/rg returns exit 1 when no matches - still success
			result.Status = StatusCompleted
			if r.Content == "" && r.Error != "" && !strings.Contains(r.Error, "exit status 1") {
				result.Error = r.Error
			}
		case "fts_search":
			query, _ := op.Spec.Args["query"].(string)
			limit := 20
			if v, ok := op.Spec.Args["limit"]; ok {
				switch n := v.(type) {
				case float64:
					limit = int(n)
				case int:
					limit = n
				case int64:
					limit = int(n)
				}
			}
			dbPath, _ := op.Spec.Args["db_path"].(string)
			if dbPath == "" {
				home, _ := os.UserHomeDir()
				if home != "" {
					dbPath = filepath.Join(home, ".cache", "sooraa", "index.db")
				} else {
					dbPath = ".sooraa-index.db"
				}
			}
			out, err := runFTSSearch(ctx, dbPath, query, limit, op.Spec.Args)
			result.Output = out
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "view_image":
			result.Output = "image-view: (stub) - view via Telegram"
			result.Status = StatusCompleted
		case "question":
			// Maps to soraacode tool/question.ts + question-manager poll with buttons
			ques, _ := op.Spec.Args["question"].(string)
			var opts []string
			if v, ok := op.Spec.Args["options"]; ok {
				switch arr := v.(type) {
				case []string:
					opts = arr
				case []any:
					for _, o := range arr {
						if s, ok := o.(string); ok {
							opts = append(opts, s)
						}
					}
				}
			}
			if len(opts) == 0 {
				opts = []string{"Yes", "No"}
			}
			sessionID, _ := op.Spec.Args["session_id"].(string)
			if sessionID == "" {
				sessionID = "default"
			}
			requestID := op.ID
			answer, err := question.Default.CreateWithContext(ctx, sessionID, requestID, ques, opts, 2*time.Minute, func() {
				event.Default.Publish(event.Event{Type: event.TypeQuestion, SessionID: sessionID, Data: map[string]any{"question": ques, "options": opts, "request_id": requestID}})
			})
			if err != nil {
				// Check if context cancelled
				select {
				case <-ctx.Done():
					result.Error = "question cancelled"
				default:
					result.Error = err.Error()
				}
				result.Status = StatusFailed
			} else {
				result.Output = answer
				result.Status = StatusCompleted
			}
		case "skills_catalog", "command_catalog", "mcp_catalog":
			var out string
			var err error
			switch op.Spec.Type {
			case "skills_catalog":
				out, err = runCatalog("skills", op.Spec.Args)
			case "command_catalog":
				out, err = runCatalog("commands", op.Spec.Args)
			case "mcp_catalog":
				out, err = runCatalog("mcp", op.Spec.Args)
			}
			result.Output = out
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "webfetch", "websearch":
			if m.runner != nil {
				// Default timeout: 30s for webfetch, 25s for websearch
				to := 30 * time.Second
				if t, ok := op.Spec.Args["timeout"]; ok {
					if ts, ok := t.(int); ok && ts > 0 {
						to = time.Duration(ts) * time.Second
					}
				}
				tctx, tcancel := context.WithTimeout(ctx, to)
				defer tcancel()
				out, err := m.runner.RunTool(tctx, op.Spec)
				result.Output = out
				if err != nil {
					result.Error = err.Error()
					result.Status = StatusFailed
				} else {
					result.Status = StatusCompleted
				}
			} else {
				result.Error = "tool runner not configured for: " + op.Spec.Type
				result.Status = StatusFailed
			}
		default:
			result.Error = "unknown operation type: " + op.Spec.Type
			result.Status = StatusFailed
		}
	}

	// Scrub secrets from results before persistence
	result.Output = redact.SecretsInText(result.Output)
	result.Error = redact.SecretsInText(result.Error)

	m.mu.Lock()
	delete(m.cancels, op.ID)
	m.mu.Unlock()

	select {
	case m.results <- result:
	case <-time.After(5 * time.Second):
	}
}

func runFTSSearch(ctx context.Context, dbPath, query string, limit int, args map[string]any) (string, error) {
	idx, err := search.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer idx.Close()

	// Handle index flag
	if doIndex, _ := args["index"].(bool); doIndex {
		root, _ := args["path"].(string)
		if root == "" {
			if cwd, err := os.Getwd(); err == nil {
				root = cwd
			} else {
				root = "."
			}
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		_, _ = idx.IndexDir(root)
	}
	// Auto-index if empty
	var cnt int
	if err := idx.DB().QueryRow(`SELECT count(*) FROM files`).Scan(&cnt); err == nil && cnt == 0 {
		root, _ := args["path"].(string)
		if root == "" {
			if cwd, err := os.Getwd(); err == nil {
				root = cwd
			} else {
				root = "."
			}
		}
		_, _ = idx.IndexDir(root)
	}
	results, err := idx.Search(query, limit)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No matches for: " + query, nil
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s (rank %.2f)\n", i+1, r.Path, r.Rank)
	}
	return b.String(), nil
}

func runFTSSearchInternal(ctx context.Context, dbPath, query string, limit int, args map[string]any) (string, error) {
	return runFTSSearch(ctx, dbPath, query, limit, args)
}

func runCatalog(kind string, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	lowerQ := strings.ToLower(query)
	switch kind {
	case "skills":
		skills := []struct{ Name, Desc string }{
			{"read", "Read file contents or list directory"},
			{"write", "Create or overwrite file"},
			{"bash", "Execute shell command"},
			{"grep", "Search via ripgrep"},
			{"glob", "File pattern match"},
			{"fts_search", "Indexed full-text search via FTS5"},
			{"question", "Ask user with buttons"},
			{"webfetch", "Fetch URL content"},
			{"websearch", "Search web via Exa"},
			{"view_image", "View image file"},
		}
		var out strings.Builder
		for _, s := range skills {
			if lowerQ != "" && !strings.Contains(strings.ToLower(s.Name), lowerQ) && !strings.Contains(strings.ToLower(s.Desc), lowerQ) {
				continue
			}
			fmt.Fprintf(&out, "• %s: %s\n", s.Name, s.Desc)
		}
		if out.Len() == 0 {
			return "No skills match: " + query, nil
		}
		return out.String(), nil
	case "commands":
		cmds := []struct{ Name, Desc string }{
			{"/new", "New session"},
			{"/sessions", "List sessions"},
			{"/open", "Open session"},
			{"/messages", "Show recent messages"},
			{"/ls", "List files"},
			{"/status", "Show harness status"},
			{"/settings", "Show config"},
			{"/model", "Select model"},
			{"/agent", "Select agent"},
			{"/project", "Project menu"},
			{"/worktree", "Git worktree"},
			{"/compaction", "Compaction status"},
			{"/help", "Full help"},
			{"/abort", "Abort turn"},
			{"/setsecret", "Store secret"},
		}
		var out strings.Builder
		for _, c := range cmds {
			if lowerQ != "" && !strings.Contains(strings.ToLower(c.Name), lowerQ) {
				continue
			}
			fmt.Fprintf(&out, "• %s: %s\n", c.Name, c.Desc)
		}
		return out.String(), nil
	case "mcp":
		return "No MCP servers configured. Add to config.json mcp section. (soraacode has mcp-websearch, etc.)", nil
	}
	return "", fmt.Errorf("unknown catalog: %s", kind)
}

func newSnapshotStore() (*snapshot.Store, error) {
	return snapshot.New("sessions/.snapshots")
}

func todoStoreDir() (string, error) {
	return "sessions", nil
}

func newTodoStore(dir string) (*todo.Store, error) {
	return todo.New(dir)
}

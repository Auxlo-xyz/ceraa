package operation

import (
	"ceraa/internal/envs"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/harness/event"
	"ceraa/harness/llm"
	"ceraa/harness/primitives"
	"ceraa/harness/question"
	"ceraa/harness/redact"
	"ceraa/harness/search"
	"ceraa/harness/snapshot"
	"ceraa/harness/todo"
	"ceraa/internal/secret"
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

// setProviderCredential is the seam the provider profile write goes through.
//
// Injected by the bridge so this package does not have to know how a profile is
// stored, and so the value crosses exactly one boundary on its way out of the
// secret store.
var setProviderCredential = func(provider, value string) error {
	return fmt.Errorf("no provider store is attached")
}

// SetProviderCredentialWriter attaches the profile write that set_provider_key
// resolves to. Exported so the application can supply it; without it the tool
// reports that no store is attached rather than pretending to have set anything.
func SetProviderCredentialWriter(fn func(provider, value string) error) {
	if fn != nil {
		setProviderCredential = fn
	}
}

// resolveUsableSecret reads a secret the model is permitted to use.
//
// Both refusals are deliberate. A reserved name is Ceraa's own credential — a tool
// that could read one hands the model the account, and one that could read a bot
// token would own the host. An unknown name is a typo in a name the model chose,
// and passing it through as empty would fail later and somewhere else.
func resolveUsableSecret(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("secret name is required")
	}
	if secret.IsInternal(name) {
		return "", fmt.Errorf("secret %q is reserved for Ceraa's own provider credentials and cannot be used by a tool", name)
	}
	value, err := secret.Get(name)
	if err != nil {
		return "", fmt.Errorf("no secret named %q", name)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("secret %q is stored but empty", name)
	}
	// Armed here as well as at startup, so a secret set since then is scrubbed
	// before it can be printed.
	redact.RegisterLiteral(value)
	return value, nil
}

// specEdits reads the edits argument in either shape it can arrive in.
//
// A spec is a map[string]any that may have been through JSON, where a
// []map[string]any becomes a []any of map[string]any, so asserting one shape
// silently yields no edits and the tool appears to do nothing.
func specEdits(raw any) []map[string]any {
	switch typed := raw.(type) {
	case []map[string]any:
		return typed
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, entry := range typed {
			if pair, ok := entry.(map[string]any); ok {
				out = append(out, pair)
			}
		}
		return out
	default:
		return nil
	}
}

// ResultEvent fires when an operation completes.
type ResultEvent struct {
	OpID       string
	Output     string
	OutputPath string
	Error      string
	Status     Status
	// Images are paths the operation wants attached to this result, for a read
	// that landed on an image file.
	Images []string
}

// ToolRunner allows tools to be executed by the operation manager.
type ToolRunner interface {
	RunTool(ctx context.Context, spec Spec) (string, error)
}

type ContextRunner interface {
	RunContext(ctx context.Context, spec Spec) (string, error)
}

// LocalManager runs operations in goroutines within the current process.
type LocalManager struct {
	mu       sync.Mutex
	results  chan ResultEvent
	cancels  map[string]context.CancelCauseFunc
	runner   ToolRunner
	slots    chan struct{}
	serialMu sync.Mutex
}

// New creates a LocalManager. If runner is nil, only built-in types (shell, read, write, glob) are supported.
func New(runner ToolRunner) *LocalManager {
	return NewWithBuffer(runner, 64)
}

// defaultRunnerTimeout bounds an in-process tool the manager does not know
// natively. It is generous because such a tool may start a browser or fetch a
// large resource, and the coordinator imposes its own outer deadline anyway.
const defaultRunnerTimeout = 150 * time.Second

// defaultShellTimeout bounds one shell command.
//
// This was 60s, which is shorter than a cold `go build ./...` on this host and
// comparable to a full test suite, so ordinary long-horizon work was killed
// mid-command and reported as an operation failure indistinguishable from the
// command itself failing. The per-call `timeout` argument overrides it.
const defaultShellTimeout = 900 * time.Second

// maxShellTimeout caps what a tool call may ask for, so one bad argument cannot
// pin a run open for a day. The bash schema documents this ceiling.
const maxShellTimeout = 3600 * time.Second

// clampShellTimeout converts a requested timeout in seconds into a duration
// inside the allowed range.
func clampShellTimeout(seconds int) time.Duration {
	requested := time.Duration(seconds) * time.Second
	if requested > maxShellTimeout {
		return maxShellTimeout
	}
	return requested
}

func NewWithBuffer(runner ToolRunner, capacity int) *LocalManager {
	if capacity < 1 {
		capacity = 1
	}
	return &LocalManager{
		results: make(chan ResultEvent, capacity),
		cancels: make(map[string]context.CancelCauseFunc),
		runner:  runner,
	}
}

func (m *LocalManager) Results() <-chan ResultEvent {
	return m.results
}

func (m *LocalManager) SetConcurrencyLimit(limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		m.slots = nil
		return
	}
	m.slots = make(chan struct{}, limit)
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
	m.add(op, false)
}

func (m *LocalManager) AddSequential(op Operation) {
	m.add(op, true)
}

func (m *LocalManager) add(op Operation, sequential bool) {
	m.mu.Lock()
	if _, exists := m.cancels[op.ID]; exists {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	m.cancels[op.ID] = cancel
	slots := m.slots
	m.mu.Unlock()

	if slots != nil {
		slots <- struct{}{}
	}
	go func() {
		if slots != nil {
			defer func() { <-slots }()
		}
		if sequential {
			m.serialMu.Lock()
			defer m.serialMu.Unlock()
		}
		m.execute(ctx, op)
	}()
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
		case "set_provider_key":
			// The value is read here, used, and dropped. It never enters the spec,
			// which is persisted as SpecArgs, and never enters the result.
			name, _ := op.Spec.Args["secret"].(string)
			provider, _ := op.Spec.Args["provider"].(string)
			value, err := resolveUsableSecret(name)
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
				break
			}
			if err := setProviderCredential(provider, value); err != nil {
				result.Error = fmt.Sprintf("could not set the credential for %s: %v", provider, err)
				result.Status = StatusFailed
				break
			}
			result.Output = fmt.Sprintf("Gave %s the credential from %s (%d chars). The value was not shown.",
				provider, name, len([]rune(value)))
			result.Status = StatusCompleted
		case "run_command_with_secret":
			// Read, inject into the child only, drop. The command's own output is
			// scrubbed below by the same redaction every other result goes through,
			// so `env` and `echo $VAR` cannot deliver the value into the model's
			// context.
			name, _ := op.Spec.Args["secret"].(string)
			envVar, _ := op.Spec.Args["env_var"].(string)
			cmd, _ := op.Spec.Args["command"].(string)
			workDir, _ := op.Spec.Args["workdir"].(string)
			value, err := resolveUsableSecret(name)
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
				break
			}
			workDir = operationPath(workDir, workDir)
			r, err := primitives.RunShellContextWithEnv(ctx, cmd, workDir, defaultShellTimeout,
				[]string{envVar + "=" + value})
			// Overwrite the local so it is not left in a long-lived slice while the
			// rest of the operation finishes.
			value = ""
			result.Output = r.Content
			result.OutputPath = r.OutputPath
			if r.OutputPath != "" {
				if data, readErr := os.ReadFile(r.OutputPath); readErr == nil {
					_ = os.WriteFile(r.OutputPath, []byte(redact.SecretsInText(string(data))), 0o600)
				}
			}
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "shell":
			cmd, _ := op.Spec.Args["command"].(string)
			to := defaultShellTimeout
			if t, ok := op.Spec.Args["timeout"]; ok {
				if ts, ok := t.(int); ok && ts > 0 {
					to = clampShellTimeout(ts)
				}
			}
			workDir, _ := op.Spec.Args["workdir"].(string)
			r, err := primitives.RunShellContext(ctx, cmd, workDir, to)
			result.Output = r.Content
			result.OutputPath = r.OutputPath
			if r.OutputPath != "" {
				if data, readErr := os.ReadFile(r.OutputPath); readErr == nil {
					_ = os.WriteFile(r.OutputPath, []byte(redact.SecretsInText(string(data))), 0o600)
				}
			}
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "read":
			p, _ := op.Spec.Args["path"].(string)
			workDir, _ := op.Spec.Args["workdir"].(string)
			p = operationPath(p, workDir)
			info, statErr := os.Stat(p)
			if statErr == nil && info.IsDir() {
				result.Output = listDirectory(p)
				result.Status = StatusCompleted
				break
			}
			// An image is attached, not transcribed.
			//
			// It used to fall through to the generic reader, which returns
			// string(bytes) and hands the model up to 5 MB of binary as text. A
			// 400 KB JPEG is around a hundred thousand tokens of mojibake, and it
			// silently consumes the context budget before the model sees anything
			// useful. The model is multimodal, so the bytes go over as an image.
			if mime, kind, ok := llm.MimeTypeForFile(p); ok && kind == llm.MediaTypeImage {
				attachment, err := llm.LoadMediaAttachment(p)
				if err != nil {
					result.Error = err.Error()
					result.Status = StatusFailed
					break
				}
				result.Images = []string{p}
				result.Output = fmt.Sprintf("%s attached as an image (%s, %s).", filepath.Base(p), mime, llm.HumanBytes(len(attachment.Data)))
				result.Status = StatusCompleted
				break
			}
			r, err := primitives.ReadFile(p)
			result.Output = r.Content
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
			}
		case "batch_edit_file":
			// All or nothing. Every change is checked against the same starting
			// content first, so a set that half-matches cannot leave a file with
			// some of the intended changes and none of the report.
			p, _ := op.Spec.Args["path"].(string)
			workDir, _ := op.Spec.Args["workdir"].(string)
			p = operationPath(p, workDir)
			edits := specEdits(op.Spec.Args["edits"])
			if len(edits) == 0 {
				result.Error = "no edits to apply"
				result.Status = StatusFailed
				break
			}
			original, readErr := os.ReadFile(p)
			content := string(original)
			// Validate every edit against the untouched content before applying
			// any, so the error names the change that did not match rather than a
			// cascade from the one before it.
			for i, pair := range edits {
				oldText, _ := pair["old_text"].(string)
				if count := strings.Count(content, oldText); count != 1 {
					result.Error = fmt.Sprintf("edit %d: old_text must occur exactly once in %s; found %d occurrences", i, filepath.Base(p), count)
					result.Status = StatusFailed
					break
				}
			}
			if result.Status == StatusFailed {
				break
			}
			updated := content
			for _, pair := range edits {
				oldText, _ := pair["old_text"].(string)
				newText, _ := pair["new_text"].(string)
				updated = strings.Replace(updated, oldText, newText, 1)
			}
			if readErr != nil {
				result.Error = readErr.Error()
				result.Status = StatusFailed
				break
			}
			err := withMutationLock(p, func() error {
				if d := filepath.Dir(p); d != "" {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				if snapStore, snapshotErr := newSnapshotStore(); snapshotErr == nil {
					_, _ = snapStore.Capture(p)
				}
				return os.WriteFile(p, []byte(updated), 0o644)
			})
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Output = fmt.Sprintf("applied %d changes to %s", len(edits), filepath.Base(p))
				result.Status = StatusCompleted
			}
		case "write":
			p, _ := op.Spec.Args["path"].(string)
			workDir, _ := op.Spec.Args["workdir"].(string)
			p = operationPath(p, workDir)
			c, _ := op.Spec.Args["content"].(string)
			err := withMutationLock(p, func() error {
				if d := filepath.Dir(p); d != "" {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				if snapStore, snapshotErr := newSnapshotStore(); snapshotErr == nil {
					_, _ = snapStore.Capture(p)
				}
				return os.WriteFile(p, []byte(c), 0o644)
			})
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
				result.Output = "written"
			}
		case "edit_line", "edit":
			// "edit" is the type stored by sessions from before the tool was
			// renamed; both dispatch to the same work.
			p, _ := op.Spec.Args["path"].(string)
			workDir, _ := op.Spec.Args["workdir"].(string)
			p = operationPath(p, workDir)
			oldText, _ := op.Spec.Args["old_text"].(string)
			newText, _ := op.Spec.Args["new_text"].(string)
			err := withMutationLock(p, func() error {
				if d := filepath.Dir(p); d != "" {
					if err := os.MkdirAll(d, 0o755); err != nil {
						return err
					}
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				content := string(data)
				if count := strings.Count(content, oldText); count != 1 {
					return fmt.Errorf("old_text must occur exactly once; found %d occurrences", count)
				}
				if snapStore, snapshotErr := newSnapshotStore(); snapshotErr == nil {
					_, _ = snapStore.Capture(p)
				}
				return os.WriteFile(p, []byte(strings.Replace(content, oldText, newText, 1)), 0o644)
			})
			if err != nil {
				result.Error = err.Error()
				result.Status = StatusFailed
			} else {
				result.Status = StatusCompleted
				result.Output = "edited"
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
			if workDir, _ := op.Spec.Args["workdir"].(string); workDir != "" {
				if _, ok := op.Spec.Args["path"]; !ok {
					op.Spec.Args["path"] = workDir
				}
			}
			dbPath, _ := op.Spec.Args["db_path"].(string)
			if dbPath == "" {
				home, _ := os.UserHomeDir()
				if home != "" {
					dbPath = filepath.Join(home, ".cache", "ceraa", "index.db")
				} else {
					dbPath = ".ceraa-index.db"
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
		case "webfetch", "websearch", "create_goal", "get_goal", "update_goal":
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
			// A tool that executes in process is dispatched through the
			// configured runner. This used to be a hardcoded list of type names,
			// which meant a new in-process tool was rejected with "unknown
			// operation type" until the executor was also edited. Anything the
			// manager does not handle natively is the runner's business.
			if m.runner != nil {
				to := defaultRunnerTimeout
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
				result.Error = "unknown operation type: " + op.Spec.Type
				result.Status = StatusFailed
			}
		}
	}

	if ctx.Err() != nil && result.Status == StatusFailed {
		result.Status = StatusCanceled
		result.Error = ctx.Err().Error()
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

func operationPath(path, workDir string) string {
	if path == "" {
		path = "."
	}
	if filepath.IsAbs(path) || workDir == "" {
		return path
	}
	return filepath.Join(workDir, path)
}

func newSnapshotStore() (*snapshot.Store, error) {
	dir := envs.Get("SESSION_FILE")
	if dir == "" {
		dir = "sessions"
	}
	return snapshot.New(filepath.Join(dir, ".snapshots"))
}

func todoStoreDir() (string, error) {
	return "sessions", nil
}

func newTodoStore(dir string) (*todo.Store, error) {
	return todo.New(dir)
}

func listDirectory(path string) string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for i, entry := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		b.WriteString(name)
	}
	return b.String()
}

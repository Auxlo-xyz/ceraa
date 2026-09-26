package tool

import (
	"ceraa/internal/envs"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"ceraa/harness/memory"
	"ceraa/harness/operation"
	"ceraa/harness/skills"
)

// Tool is a capability with a schema and translator.
type Tool interface {
	Name() string
	Description() string
	Schema() map[string]any
	Translate(args map[string]any) (operation.Spec, error)
	TranslateResult(op operation.Operation) string
}

// Runner is an optional interface tools can implement for direct execution.
type Runner interface {
	Run(spec operation.Spec) (string, error)
}

type SequentialTool interface {
	Sequential() bool
}

type MutationKeyer interface {
	MutationKey(args map[string]any) string
}

// Registry holds available tools.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// toToolDef converts an internal Tool into a generic map for LLM schema.
func toToolDef(t Tool) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters":  t.Schema(),
		},
	}
}

// BashTool - executes shell commands.
type BashTool struct{}

func (b *BashTool) Name() string { return "bash" }
func (b *BashTool) Description() string {
	return "Execute a bash command and return the combined stdout/stderr output."
}
func (b *BashTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Bash command to execute"},
			"timeout": map[string]any{"type": "integer", "description": "Timeout seconds (default 60)"},
		},
		"required": []string{"command"},
	}
}
func (b *BashTool) Translate(args map[string]any) (operation.Spec, error) {
	cmd, ok := args["command"].(string)
	if !ok || strings.ContainsRune(cmd, 0) {
		return operation.Spec{}, fmt.Errorf("invalid command")
	}
	return operation.Spec{
		Type: "shell",
		Args: map[string]any{
			"command": cmd,
			"timeout": args["timeout"],
		},
	}, nil
}
func (b *BashTool) TranslateResult(op operation.Operation) string {
	var parts []string
	if op.Output != "" {
		parts = append(parts, op.Output)
	}
	if op.Error != "" {
		parts = append(parts, "Error: "+op.Error)
	}
	return strings.Join(parts, "\n")
}

// ReadTool - reads files or lists directories.
type ReadTool struct{}

func (r *ReadTool) Name() string        { return "read" }
func (r *ReadTool) Description() string { return "Read file contents or list a directory's entries." }
func (r *ReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Absolute or relative path"},
		},
		"required": []string{"path"},
	}
}
func (r *ReadTool) Translate(args map[string]any) (operation.Spec, error) {
	p, ok := args["path"].(string)
	if !ok || p == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	return operation.Spec{Type: "read", Args: map[string]any{"path": p}}, nil
}
func (r *ReadTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// WriteTool - writes/overwrites files.
type WriteTool struct{}

func (w *WriteTool) Name() string { return "write" }
func (w *WriteTool) Description() string {
	return "Create or overwrite a single file at the given path."
}
func (w *WriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path"},
			"content": map[string]any{"type": "string", "description": "File contents"},
		},
		"required": []string{"path", "content"},
	}
}
func (w *WriteTool) Translate(args map[string]any) (operation.Spec, error) {
	p, _ := args["path"].(string)
	c, _ := args["content"].(string)
	if p == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	return operation.Spec{Type: "write", Args: map[string]any{"path": p, "content": c}}, nil
}
func (w *WriteTool) TranslateResult(op operation.Operation) string {
	return "written"
}
func (w *WriteTool) Sequential() bool { return true }
func (w *WriteTool) MutationKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return path
}

type EditTool struct{}

func (e *EditTool) Name() string { return "edit" }
func (e *EditTool) Description() string {
	return "Replace one exact occurrence of old_text with new_text in a file."
}
func (e *EditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":     map[string]any{"type": "string", "description": "File path"},
			"old_text": map[string]any{"type": "string", "description": "Existing text to replace"},
			"new_text": map[string]any{"type": "string", "description": "Replacement text"},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}
func (e *EditTool) Translate(args map[string]any) (operation.Spec, error) {
	p, _ := args["path"].(string)
	oldText, _ := args["old_text"].(string)
	newText, _ := args["new_text"].(string)
	if p == "" || oldText == "" {
		return operation.Spec{}, fmt.Errorf("path and non-empty old_text are required")
	}
	return operation.Spec{Type: "edit", Args: map[string]any{"path": p, "old_text": oldText, "new_text": newText}}, nil
}
func (e *EditTool) TranslateResult(op operation.Operation) string { return "edited" }
func (e *EditTool) Sequential() bool                              { return true }
func (e *EditTool) MutationKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return path
}

type LsTool struct{}

func (l *LsTool) Name() string { return "ls" }
func (l *LsTool) Description() string {
	return "List files and directories at a path."
}
func (l *LsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path"},
		},
		"required": []string{"path"},
	}
}
func (l *LsTool) Translate(args map[string]any) (operation.Spec, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	return operation.Spec{Type: "ls", Args: map[string]any{"path": path}}, nil
}
func (l *LsTool) TranslateResult(op operation.Operation) string { return op.Output }

// GlobTool - glob file patterns.
type GlobTool struct{}

func (g *GlobTool) Name() string        { return "glob" }
func (g *GlobTool) Description() string { return "Glob a file pattern and return matched paths." }
func (g *GlobTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern"},
		},
		"required": []string{"pattern"},
	}
}
func (g *GlobTool) Translate(args map[string]any) (operation.Spec, error) {
	p, ok := args["pattern"].(string)
	if !ok || p == "" {
		return operation.Spec{}, fmt.Errorf("pattern is required")
	}
	return operation.Spec{Type: "glob", Args: map[string]any{"pattern": p}}, nil
}
func (g *GlobTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// GrepTool - search file contents.
type GrepTool struct{}

func (g *GrepTool) Name() string        { return "grep" }
func (g *GrepTool) Description() string { return "Search for a pattern in files using ripgrep." }
func (g *GrepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Search pattern (regex)"},
			"path":    map[string]any{"type": "string", "description": "Directory to search (default current)"},
		},
		"required": []string{"pattern"},
	}
}
func (g *GrepTool) Translate(args map[string]any) (operation.Spec, error) {
	p, ok := args["pattern"].(string)
	if !ok || p == "" {
		return operation.Spec{}, fmt.Errorf("pattern is required")
	}
	return operation.Spec{Type: "grep", Args: map[string]any{"pattern": p, "path": args["path"]}}, nil
}
func (g *GrepTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// ViewImageTool - views images (stubs for now, returns placeholder).
type ViewImageTool struct{}

func (v *ViewImageTool) Name() string        { return "view_image" }
func (v *ViewImageTool) Description() string { return "View an image file and return a description." }
func (v *ViewImageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Absolute path to image file (JPEG/PNG/BMP/TIFF/WebP)"},
		},
		"required": []string{"path"},
	}
}
func (v *ViewImageTool) Translate(args map[string]any) (operation.Spec, error) {
	p, ok := args["path"].(string)
	if !ok || p == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	workDir, _ := args["workdir"].(string)
	if !filepath.IsAbs(p) && workDir != "" {
		p = filepath.Join(workDir, p)
	}
	if _, err := os.Stat(p); err != nil {
		return operation.Spec{}, fmt.Errorf("image not found: %s", p)
	}
	return operation.Spec{Type: "view_image", Args: map[string]any{"path": p, "workdir": workDir}}, nil
}
func (v *ViewImageTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// DefaultRegistry returns a registry with all built-in tools.
func DefaultRegistry() *Registry {
	return DefaultRegistryWithStores(nil, nil)
}

func DefaultRegistryWithStores(memoryStore *memory.Store, skillStore *skills.Store) *Registry {
	r := NewRegistry()
	r.Register(&BashTool{})
	r.Register(&ReadTool{})
	r.Register(&WriteTool{})
	r.Register(&EditTool{})
	r.Register(&LsTool{})
	r.Register(&GlobTool{})
	r.Register(&GrepTool{})
	r.Register(&ViewImageTool{})
	r.Register(&WebFetchTool{})
	// One manager per registry, so a run keeps a single browser and a sequence of
	// actions continues where the last one stopped.
	r.Register(NewBrowserTool())
	r.Register(&FTSSearchTool{})
	r.Register(&QuestionTool{})
	r.Register(&SkillsCatalogTool{})
	r.Register(&CommandCatalogTool{})
	r.Register(&MCPCatalogTool{})
	r.Register(&TodoWriteTool{})
	r.Register(&CreateGoalTool{})
	r.Register(&GetGoalTool{})
	r.Register(&UpdateGoalTool{})

	// WebSearch with config from env
	provider := envs.Get("WEBSEARCH_PROVIDER")
	apiKey := os.Getenv("EXA_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	r.Register(NewWebSearchTool(apiKey, provider))
	if memoryStore != nil {
		r.Register(&MemoryTool{Store: memoryStore})
	}
	if skillStore != nil {
		r.Register(&SkillProposalTool{Store: skillStore})
	}

	return r
}

func DefaultRegistryForUser(user string, memoryStore *memory.Store, skillStore *skills.Store) *Registry {
	lower := strings.ToLower(user)
	memoryEnabled := strings.Contains(lower, "remember") || strings.Contains(lower, "memory") || strings.Contains(lower, "preference")
	skillEnabled := strings.Contains(lower, "wrong") || strings.Contains(lower, "incorrect") || strings.Contains(lower, "not right") || strings.Contains(lower, "feedback") || strings.Contains(lower, "skill")
	switch {
	case memoryEnabled && skillEnabled:
		return DefaultRegistryWithStores(memoryStore, skillStore)
	case memoryEnabled:
		return DefaultRegistryWithStores(memoryStore, nil)
	case skillEnabled:
		return DefaultRegistryWithStores(nil, skillStore)
	default:
		return DefaultRegistry()
	}
}

// ResolvePath returns the absolute path, following symlinks.
func ResolvePath(p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, p), nil
}

// RunBash executes a command for fallback cases.
func RunBash(command, workDir string, timeoutSec int) (string, error) {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	cmd := exec.Command("bash", "-c", command)
	if workDir != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

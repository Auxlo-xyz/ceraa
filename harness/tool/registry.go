package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sooraa/harness/operation"
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
	if _, err := os.Stat(p); err != nil {
		return operation.Spec{}, fmt.Errorf("image not found: %s", p)
	}
	return operation.Spec{Type: "view_image", Args: map[string]any{"path": p}}, nil
}
func (v *ViewImageTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// DefaultRegistry returns a registry with all built-in tools.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(&BashTool{})
	r.Register(&ReadTool{})
	r.Register(&WriteTool{})
	r.Register(&GlobTool{})
	r.Register(&GrepTool{})
	r.Register(&ViewImageTool{})
	r.Register(&WebFetchTool{})
	r.Register(&FTSSearchTool{})

	// WebSearch with config from env
	provider := os.Getenv("SOORAA_WEBSEARCH_PROVIDER")
	apiKey := os.Getenv("EXA_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	r.Register(NewWebSearchTool(apiKey, provider))

	return r
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

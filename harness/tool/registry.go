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
			"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (default 900, max 3600)."},
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

func (e *EditTool) Name() string { return "edit_line" }

// Description states the failure modes, because a tool that fails silently is
// worse than one that is missing: the model believes it edited a file that is
// unchanged. Both failures here are deterministic and the model can correct
// itself from the message.
//
// The failure modes are stated because a tool that fails silently is worse than
// one that is missing: the model believes it edited a file that is unchanged.
// Both are deterministic and the model can correct itself from the message.
//
// Which of this and batch_edit_file to reach for is guidance, and guidance
// belongs in the prompt's toolrules rather than in a description — a description
// is the tool's contract, and a rule about when to prefer it over a sibling is
// the prompt's job. TestDescriptionsCarryNoUsageGuidance enforces that split.
func (e *EditTool) Description() string {
	return "Replace one exact occurrence of old_text with new_text in a file. " +
		"Fails without writing if old_text is absent or matches more than once, " +
		"naming how many were found."
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
	return operation.Spec{Type: "edit_line", Args: map[string]any{"path": p, "old_text": oldText, "new_text": newText}}, nil
}
func (e *EditTool) TranslateResult(op operation.Operation) string { return "edited" }
func (e *EditTool) Sequential() bool                              { return true }
func (e *EditTool) MutationKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return path
}

// BatchEditFileTool applies several changes to one file as a single operation.
//
// It exists because edit_line cannot express a change set: making five separate
// edits costs five round trips and five chances to half-apply, and a model that
// gives up halfway leaves a file in a state neither it nor the user asked for.
// Everything or nothing.
type BatchEditFileTool struct{}

func (b *BatchEditFileTool) Name() string { return "batch_edit_file" }
func (b *BatchEditFileTool) Description() string {
	return "Apply several replacements to one file together, as a single write. " +
		"Give each change as an old_text and a new_text pair; every one must match " +
		"exactly once. Fails without writing anything if any change does not match, " +
		"nailing which one."
}
func (b *BatchEditFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "File path"},
			"edits": map[string]any{
				"type":        "array",
				"description": "Replacements to apply in order. All must match exactly once.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_text": map[string]any{"type": "string", "description": "Existing text to replace"},
						"new_text": map[string]any{"type": "string", "description": "Replacement text"},
					},
					"required": []string{"old_text", "new_text"},
				},
			},
		},
		"required": []string{"path", "edits"},
	}
}

func (b *BatchEditFileTool) Translate(args map[string]any) (operation.Spec, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	rawEdits, _ := args["edits"].([]any)
	if len(rawEdits) == 0 {
		return operation.Spec{}, fmt.Errorf("edits is required and must not be empty")
	}
	edits := make([]map[string]any, 0, len(rawEdits))
	for i, entry := range rawEdits {
		pair, ok := entry.(map[string]any)
		if !ok {
			return operation.Spec{}, fmt.Errorf("edit %d is not an object with old_text and new_text", i)
		}
		oldText, _ := pair["old_text"].(string)
		newText, _ := pair["new_text"].(string)
		if oldText == "" {
			return operation.Spec{}, fmt.Errorf("edit %d has no old_text", i)
		}
		edits = append(edits, map[string]any{"old_text": oldText, "new_text": newText})
	}
	return operation.Spec{Type: "batch_edit_file", Args: map[string]any{"path": path, "edits": edits}}, nil
}
func (b *BatchEditFileTool) TranslateResult(op operation.Operation) string { return "edited" }
func (b *BatchEditFileTool) Sequential() bool                              { return true }
func (b *BatchEditFileTool) MutationKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return path
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
	r.Register(&BatchEditFileTool{})
	r.Register(&RunCommandWithSecretTool{})
	r.Register(&ListSecretsTool{})

	r.Register(&WebFetchTool{})
	// Registered without a client and attached later by whoever knows the active
	// provider, so the tool layer stays free of configuration.
	r.Register(&ViewImageTool{})
	// One manager per registry, so a run keeps a single browser and a sequence of
	// actions continues where the last one stopped.
	r.Register(NewBrowserTool())
	r.Register(&FTSSearchTool{})
	r.Register(&QuestionTool{})
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

// ResolvePathFrom resolves a path against an explicit base directory, falling
// back to the process working directory.
//
// A relative path from a model means "relative to where my commands run", and a
// shell tool runs in the session's workdir, which is often not the process
// working directory. Resolving against the process directory instead makes a
// file the model just created look missing, while the very same relative path
// worked moments earlier through bash.
func ResolvePathFrom(base, p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	root := strings.TrimSpace(base)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = cwd
	}
	return filepath.Join(root, p), nil
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

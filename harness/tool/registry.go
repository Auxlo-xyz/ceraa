package tool

import (
	"ceraa/internal/envs"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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

// SequentialQueueTool names the lane a sequential tool runs on.
//
// The default lane serialises a sequential tool against every other one, which
// is safe but coarse: it meant a batch holding one edit and one browser session
// ran them one after the other even though they share nothing. Naming a lane
// lets a batch carry an edit and a browser action at once, while two edits
// still never interleave.
//
// It is optional, and a tool that does not implement it behaves exactly as it
// did before this existed.
type SequentialQueueTool interface {
	SequentialQueue() string
}

type MutationKeyer interface {
	MutationKey(args map[string]any) string
}

// Registry holds available tools.
type Registry struct {
	tools map[string]Tool
	// hidden names tools that still execute but are not offered to the model.
	//
	// This exists for the tool a set of narrow ones replaced. Sessions recorded
	// before the split still contain calls to it, and those calls have to run
	// when the history is replayed, but showing the model both shapes teaches it
	// the shape that was the problem. Get resolves hidden tools, All includes
	// them for internal consumers, and only the catalogue the model reads leaves
	// them out.
	hidden map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool), hidden: make(map[string]bool)}
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Register(t Tool) {
	// Registering is the only step a new tool needs for name resolution,
	// including garbled names: every call funnels through canonicalToolName
	// in harness/coordinator, which sanitizes before this lookup. Do not add
	// name handling here.
	r.tools[t.Name()] = t
	delete(r.hidden, t.Name())
}

// RegisterHidden makes a tool executable without offering it to the model.
func (r *Registry) RegisterHidden(t Tool) {
	r.tools[t.Name()] = t
	r.hidden[t.Name()] = true
}

// Remove drops a tool by name, reporting whether it was there.
//
// A child agent must not be able to delegate further, and a child tool set that
// still advertised the task tool would invite exactly that. Rebuilding a registry
// to exclude one tool is heavier than removing it, and the registry is not
// shared: a child builds its own.
func (r *Registry) Remove(name string) bool {
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	delete(r.hidden, name)
	return true
}

func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Offered is the catalogue the model is shown: every tool except the hidden
// ones. All is unchanged and still returns everything, because the script tool
// and the inventories walk it and have no reason to know a tool was ever hidden.
func (r *Registry) Offered() []Tool {
	all := r.All()
	if len(r.hidden) == 0 {
		return all
	}
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		if !r.hidden[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

// Tool definitions are converted directly by the coordinator.
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
			"command": map[string]any{"type": "string"},
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
			"path":   map[string]any{"type": "string", "description": "Absolute or relative path"},
			"offset": map[string]any{"type": "integer", "description": "First line to return, counting from 1."},
			"limit":  map[string]any{"type": "integer", "description": "How many lines to return from offset."},
		},
		"required": []string{"path"},
	}
}
func (r *ReadTool) Translate(args map[string]any) (operation.Spec, error) {
	p, ok := args["path"].(string)
	if !ok || p == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	spec := map[string]any{"path": p}
	// Carried only when given, so a read without a range keeps the exact spec
	// shape it has always had and an older session replays unchanged.
	offset, hasOffset := positiveIntArg(args["offset"])
	limit, hasLimit := positiveIntArg(args["limit"])
	if hasOffset {
		spec["offset"] = offset
	}
	if hasLimit {
		spec["limit"] = limit
	}
	// A limit with no offset is a read of the first N lines, which is a
	// reasonable thing to ask for. Naming it in the spec is what makes the
	// operation able to tell it from a whole-file read.
	return operation.Spec{Type: "read", Args: spec}, nil
}

// positiveIntArg narrows a JSON-decoded number to a positive int, treating
// anything else as absent.
//
// A zero or negative offset would otherwise become a slice bound that reads
// nothing or panics, and passing one through is worse than ignoring it.
func positiveIntArg(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if n := int(typed); n > 0 && float64(n) == typed {
			return n, true
		}
	case int:
		if typed > 0 {
			return typed, true
		}
	}
	return 0, false
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
			"path":    map[string]any{"type": "string"},
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

func (w *WriteTool) SequentialQueue() string { return "file" }
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
	return "Replace one exact region in a file. The region may span multiple lines. " +
		"The match must be unique unless occurrence, 1-based, picks a repeat."
}
func (e *EditTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string"},
			"old_text":   map[string]any{"type": "string", "description": "Exact region to replace"},
			"new_text":   map[string]any{"type": "string", "description": "Replacement text"},
			"occurrence": map[string]any{"type": "integer", "description": "1-based match index, optional"},
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
	spec := map[string]any{"path": p, "old_text": oldText, "new_text": newText}
	// Carried only when given, so a spec written before this field keeps its
	// original shape and an older session replays unchanged.
	if n := occurrenceFromAny(args["occurrence"]); n > 0 {
		spec["occurrence"] = n
	}
	return operation.Spec{Type: "edit_line", Args: spec}, nil
}
func (e *EditTool) TranslateResult(op operation.Operation) string { return "edited" }
func (e *EditTool) Sequential() bool                              { return true }
func (e *EditTool) SequentialQueue() string                       { return "file" }
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
	return "Apply multiple exact region replacements to one file atomically. " +
		"Nothing is written if any replacement fails."
}
func (b *BatchEditFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
			"edits": map[string]any{
				"type":        "array",
				"description": "applied in order, all or nothing",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_text":   map[string]any{"type": "string", "description": "Exact region to replace"},
						"new_text":   map[string]any{"type": "string", "description": "Replacement text"},
						"occurrence": map[string]any{"type": "integer", "description": "1-based match index, optional"},
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
			return operation.Spec{}, fmt.Errorf("edit %d has no old_text", i+1)
		}
		normalized := map[string]any{"old_text": oldText, "new_text": newText}
		if n := occurrenceFromAny(pair["occurrence"]); n > 0 {
			normalized["occurrence"] = n
		}
		edits = append(edits, normalized)
	}
	return operation.Spec{Type: "batch_edit_file", Args: map[string]any{"path": path, "edits": edits}}, nil
}
func (b *BatchEditFileTool) TranslateResult(op operation.Operation) string { return "edited" }
func (b *BatchEditFileTool) Sequential() bool                              { return true }
func (b *BatchEditFileTool) SequentialQueue() string                       { return "file" }
func (b *BatchEditFileTool) MutationKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return path
}

// occurrenceFromAny narrows a JSON-decoded occurrence to a positive int, treating
// anything else as absent.
//
// A zero, a negative or a non-number is not a request for a particular match, and
// forwarding one would quietly turn "old_text must be unique" into a replacement
// of whichever match happened to sit at that position.
func occurrenceFromAny(value any) int {
	switch typed := value.(type) {
	case float64:
		if n := int(typed); n > 0 {
			return n
		}
	case int:
		if typed > 0 {
			return typed
		}
	}
	return 0
}

// DefaultRegistry returns a registry with all built-in tools.
func DefaultRegistry() *Registry {
	return DefaultRegistryWithStores(nil)
}

func DefaultRegistryWithStores(skillStore *skills.Store) *Registry {
	r := NewRegistry()
	r.Register(&BashTool{})
	r.Register(&ReadTool{})
	r.Register(&WriteTool{})
	r.Register(&EditTool{})
	r.Register(&BatchEditFileTool{})
	r.Register(&RunCommandWithSecretTool{})
	r.Register(&ListSecretsTool{})

	r.Register(&WebFetchTool{})
	// Recon pair: one probe engine (observations + receipts) and one secret
	// scanner over what the probe saw. Registered together because a scan
	// without a probe behind it has nothing to scan, and both are inert
	// until an engagement is active: without one the probe returns
	// ErrNoMission and the scanner returns "no engagement active".
	r.Register(&ProbeTool{})
	r.Register(&ScanSecretsTool{})
	// One manager per registry, so a run keeps a single browser and a sequence of
	// actions continues where the last one stopped.
	//
	// Registered hidden. The model is offered one tool per action instead, because
	// it kept bundling arguments belonging to different actions into one call and
	// this is the only tool of the twenty-one that was not a single verb with a
	// single set of properties. It stays registered and executable because
	// sessions recorded before the split still call it, and hiding it from the
	// catalogue is what stops the model being taught both shapes.
	browser := NewBrowserTool()
	r.RegisterHidden(browser)
	for _, actionTool := range BrowserActionTools(browser) {
		r.Register(actionTool)
	}
	// Registered without a factory and attached later by whoever knows how to build a
	// runner: the tool layer cannot import the coordinator, which imports this one.
	r.Register(NewTaskTool())
	r.Register(&QuestionTool{})
	// Registered hidden, not dropped. The plan it carries moved into
	// update_goal's todos argument and the catalogue no longer offers it, but
	// sessions recorded before the merge still call todowrite and the replay
	// path resolves calls by name. Hiding it is what stops the model being
	// taught a shape that no longer exists.
	r.RegisterHidden(&TodoWriteTool{})
	r.Register(&CreateGoalTool{})
	r.Register(&GetGoalTool{})
	r.Register(&UpdateGoalTool{})
	// Read-only recovery for what compaction summarized away: the archive is
	// pulled like a fetched page, never injected, so listing it beside the
	// goals it resembles is honest about what it is.
	r.Register(&RecallArchiveTool{})

	// WebSearch with config from env
	provider := envs.Get("WEBSEARCH_PROVIDER")
	apiKey := os.Getenv("EXA_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	r.Register(NewWebSearchTool(apiKey, provider))

	if skillStore != nil {
		r.Register(&SkillProposalTool{Store: skillStore})
		// The catalogue names these skills; this is how the model reaches the
		// one it needs. Registered together with the proposer because a store
		// that can be written to and not read back is a dead end.
		r.Register(&SkillUseTool{Store: skillStore})
	}

	return r
}

// DefaultRegistryForUser varies the registry by what the user just said.
//
// Only proposing is gated. The gate exists because proposing a skill is an offer
// to rewrite how the agent behaves, and it should not appear because a message
// happened to contain the word "feedback".
//
// Reading an approved skill is not gated, and cannot be. The catalogue is
// rendered into the prompt unconditionally, so gating its reader on the user's
// wording would leave the model told a skill exists and unable to ask for it,
// which is worse than not listing it. So the store is threaded through every
// branch and only the proposer is withdrawn.
//
// The agent has no memory tool. The store is written by the extractor after a
// run and read back as the sidekick, so a live write from the agent would be a
// second writer to a store one background job owns.
func DefaultRegistryForUser(user string, skillStore *skills.Store) *Registry {
	lower := strings.ToLower(user)
	skillEnabled := strings.Contains(lower, "wrong") || strings.Contains(lower, "incorrect") || strings.Contains(lower, "not right") || strings.Contains(lower, "feedback") || strings.Contains(lower, "skill")
	registry := DefaultRegistry()
	if skillStore != nil {
		registry = DefaultRegistryWithStores(skillStore)
	}
	if !skillEnabled {
		registry.Remove(SkillProposalName)
	}
	return registry
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

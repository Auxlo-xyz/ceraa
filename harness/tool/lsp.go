package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"ceraa/harness/lsp"
	"ceraa/harness/operation"
)

// The language server answers questions text search cannot: where a symbol is
// defined, where it is used, what the compiler says about a file. Like the
// browser split, the action becomes the tool: one verb with one set of
// properties each, so a call cannot bundle arguments that belong to different
// actions. The dispatch is one driver: every action translates into the same
// `lsp` operation spec, executed by the shared base tool against one manager
// per project root.

// LspTool is the shared driver behind the per-action tools. It holds no server
// of its own; managers live one per project root, because a language server
// answers for the project it was started in and a second root needs a second
// server.
type LspTool struct {
	mu         sync.Mutex
	managers   map[string]*lsp.Manager
	newManager func(rootDir string) (*lsp.Manager, error)
}

// NewLspTool returns a driver that starts gopls on demand, resolving the binary
// the way language tooling is normally found: project-local bins first, then
// PATH. A host without gopls still constructs: the first call fails naming the
// missing binary instead of the registry failing at startup.
func NewLspTool() *LspTool {
	return &LspTool{
		managers: map[string]*lsp.Manager{},
		newManager: func(rootDir string) (*lsp.Manager, error) {
			path, err := lsp.ResolveServer(rootDir)
			if err != nil {
				return nil, err
			}
			return lsp.NewManager(path, rootDir)
		},
	}
}

func (t *LspTool) Name() string { return "lsp" }

func (t *LspTool) Description() string {
	return "Ask the project's language server about Go code. Never called directly; the lsp_* tools are the calls."
}

func (t *LspTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *LspTool) Translate(args map[string]any) (operation.Spec, error) {
	action, _ := args["action"].(string)
	known := false
	for _, def := range lspActionDefs {
		if def.action == action {
			known = true
			break
		}
	}
	if !known || action == "" {
		return operation.Spec{}, fmt.Errorf("unknown lsp action %q", action)
	}
	translated := map[string]any{"action": action}
	for _, key := range []string{"path", "line", "character", "symbol"} {
		if value, present := args[key]; present && value != nil {
			translated[key] = value
		}
	}
	return operation.Spec{Type: "lsp", Args: translated}, nil
}

func (t *LspTool) TranslateResult(op operation.Operation) string {
	if op.Error != "" && op.Output == "" {
		return "Error: " + op.Error
	}
	return op.Output
}

func (t *LspTool) managerFor(rootDir string) (*lsp.Manager, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.managers[rootDir]; ok {
		return m, nil
	}
	m, err := t.newManager(rootDir)
	if err != nil {
		return nil, err
	}
	t.managers[rootDir] = m
	return m, nil
}

// Run satisfies tool.Runner. A language server is stateful, so the action runs
// in the process and not through a shell.
func (t *LspTool) Run(spec operation.Spec) (string, error) {
	return t.RunContext(context.Background(), spec)
}

// RunContext performs one language-server action. The path is resolved here,
// not in Translate, because the workdir arrives on the spec after translation
// and a relative path means "where my commands run".
func (t *LspTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	action, _ := spec.Args["action"].(string)
	rawPath, _ := spec.Args["path"].(string)
	workDir, _ := spec.Args["workdir"].(string)
	path, err := ResolvePathFrom(workDir, rawPath)
	if err != nil {
		return "", err
	}
	rootDir := strings.TrimSpace(workDir)
	if rootDir == "" {
		rootDir = filepath.Dir(path)
	}
	manager, err := t.managerFor(rootDir)
	if err != nil {
		return "", err
	}
	switch action {
	case "diagnostics":
		items, err := manager.Diagnostics(ctx, path)
		if err != nil {
			return "", err
		}
		return formatDiagnostics(path, items), nil
	case "definition", "references":
		line, err := lspLineArg(spec.Args["line"])
		if err != nil {
			return "", err
		}
		character, err := lspCharacterArg(path, line, spec.Args)
		if err != nil {
			return "", err
		}
		var locations []lsp.Location
		if action == "definition" {
			locations, err = manager.Definition(ctx, path, line-1, character)
		} else {
			locations, err = manager.References(ctx, path, line-1, character)
		}
		if err != nil {
			return "", err
		}
		return formatLocations(locations, 50), nil
	case "hover":
		line, err := lspLineArg(spec.Args["line"])
		if err != nil {
			return "", err
		}
		character, err := lspCharacterArg(path, line, spec.Args)
		if err != nil {
			return "", err
		}
		text, err := manager.Hover(ctx, path, line-1, character)
		if err != nil {
			return "", err
		}
		if text == "" {
			return "no hover information at that position", nil
		}
		return truncateRunes(text, 4000), nil
	case "symbols":
		symbols, err := manager.DocumentSymbols(ctx, path)
		if err != nil {
			return "", err
		}
		return formatSymbols(symbols, 200), nil
	default:
		return "", fmt.Errorf("unknown lsp action %q", action)
	}
}

// Sequential for the same reason the browser is: one server per root, and a
// batch is a pipeline the model expects in order.
func (t *LspTool) Sequential() bool        { return true }
func (t *LspTool) SequentialQueue() string { return "lsp" }

// lspLineArg narrows the model's 1-based line to a positive int. JSON numbers
// arrive as float64; anything else is absent, and absent is a caller error
// because every position action needs a line.
func lspLineArg(value any) (int, error) {
	switch typed := value.(type) {
	case float64:
		if n := int(typed); n > 0 && float64(n) == typed {
			return n, nil
		}
	case int:
		if typed > 0 {
			return typed, nil
		}
	}
	return 0, fmt.Errorf("line is required and must be a positive integer")
}

// lspCharacterArg resolves the model's column addressing to the protocol's
// 0-based offset, via the shared resolver so the rules are stated once.
func lspCharacterArg(path string, line int, args map[string]any) (int, error) {
	character, hasCharacter := args["character"]
	symbol, _ := args["symbol"].(string)
	char := -1
	if hasCharacter {
		switch typed := character.(type) {
		case float64:
			if n := int(typed); n >= 0 && float64(n) == typed {
				char = n
			}
		case int:
			if typed >= 0 {
				char = typed
			}
		}
		if char < 0 {
			return 0, fmt.Errorf("character must be 0 or greater")
		}
		return lsp.ResolveCharacter("", "", char, true)
	}
	lineText, err := lsp.ReadLine(path, line)
	if err != nil {
		return 0, err
	}
	return lsp.ResolveCharacter(lineText, symbol, 0, false)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "\n…truncated"
}

func formatLocations(locations []lsp.Location, cap int) string {
	if len(locations) == 0 {
		return "no results"
	}
	lines := make([]string, 0, len(locations))
	for i, loc := range locations {
		if i >= cap {
			break
		}
		lines = append(lines, fmt.Sprintf("%s:%d", lsp.StripFileURI(loc.URI), loc.Range.Start.Line+1))
	}
	if len(locations) > cap {
		lines = append(lines, fmt.Sprintf("…and %d more (%d total)", len(locations)-cap, len(locations)))
	}
	return strings.Join(lines, "\n")
}

func formatDiagnostics(path string, items []lsp.Diagnostic) string {
	if len(items) == 0 {
		return "no diagnostics for " + path
	}
	const cap = 100
	lines := make([]string, 0, len(items))
	for i, item := range items {
		if i >= cap {
			break
		}
		lines = append(lines, fmt.Sprintf("%s:%d:%d %s %s",
			path, item.Range.Start.Line+1, item.Range.Start.Character+1,
			diagnosticSeverity(item.Severity), strings.TrimSpace(item.Message)))
	}
	if len(items) > cap {
		lines = append(lines, fmt.Sprintf("…and %d more (%d total)", len(items)-cap, len(items)))
	}
	return strings.Join(lines, "\n")
}

func diagnosticSeverity(severity int) string {
	switch severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "unknown"
	}
}

func formatSymbols(symbols []lsp.Symbol, cap int) string {
	if len(symbols) == 0 {
		return "no symbols"
	}
	lines := make([]string, 0, len(symbols))
	for i, sym := range symbols {
		if i >= cap {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s %s:%d",
			lsp.SymbolKindName(sym.Kind), sym.Name, sym.Path, sym.Line))
	}
	if len(symbols) > cap {
		lines = append(lines, fmt.Sprintf("…and %d more (%d total)", len(symbols)-cap, len(symbols)))
	}
	return strings.Join(lines, "\n")
}

// lspArgDef is one tool: the action it performs, the name it is called by, and
// the arguments it accepts.
type lspArgDef struct {
	action      string
	tool        string
	required    []string
	optional    []string
	description string
}

// lspArgSpec is the shared argument vocabulary, described once so the five
// schemas cannot drift apart. Line is 1-based, matching read; character is
// 0-based, matching the protocol; symbol names text on the line when the
// column is unknown.
var lspArgDefs = map[string]struct {
	kind        string
	description string
}{
	"path":      {"string", "File to ask about. A relative path resolves against the session workdir."},
	"line":      {"integer", "1-based line number, matching read."},
	"character": {"integer", "0-based column on the line. Give this or symbol."},
	"symbol":    {"string", "Symbol text on the line, used to find the column when character is absent."},
}

// The descriptions say what each tool returns and nothing about when to reach
// for it. That split is enforced by TestDescriptionsCarryNoUsageGuidance:
// usage guidance lives in the prompt's toolrules section.
var lspActionDefs = []lspArgDef{
	{
		action:      "diagnostics",
		tool:        "diagnostics",
		required:    []string{"path"},
		description: "Report the language server's diagnostics for one file as path:line:col severity message lines.",
	},
	{
		action:      "definition",
		tool:        "definition",
		required:    []string{"path", "line"},
		optional:    []string{"character", "symbol"},
		description: "Locate where the symbol at a file position is defined, as path:line entries.",
	},
	{
		action:      "references",
		tool:        "references",
		required:    []string{"path", "line"},
		optional:    []string{"character", "symbol"},
		description: "List where the symbol at a file position is used, as path:line entries.",
	},
	{
		action:      "hover",
		tool:        "hover",
		required:    []string{"path", "line"},
		optional:    []string{"character", "symbol"},
		description: "Return the language server's hover text for a file position.",
	},
	{
		action:      "symbols",
		tool:        "symbols",
		required:    []string{"path"},
		description: "List the symbols declared in one file as kind name path:line entries.",
	},
}

// LspActionTool is one language-server action presented as its own tool.
//
// It holds no server of its own. Every instance shares the one LspTool, so the
// managers outlive any single call, which is the entire reason the actions are
// a sequence rather than independent tools.
type LspActionTool struct {
	base *LspTool
	def  lspArgDef
}

func (t *LspActionTool) Name() string { return "lsp_" + t.def.tool }

func (t *LspActionTool) Description() string { return t.def.description }

func (t *LspActionTool) Schema() map[string]any {
	properties := map[string]any{}
	for _, key := range append(append([]string{}, t.def.required...), t.def.optional...) {
		def, known := lspArgDefs[key]
		if !known {
			continue
		}
		properties[key] = map[string]any{"type": def.kind, "description": def.description}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(t.def.required) > 0 {
		schema["required"] = t.def.required
	}
	return schema
}

// Translate pins the action and defers the rest to the shared driver, so the
// spec shape is built once. The argument set is checked here, against this
// tool's own rules, because a shared check would let one action accept
// another's arguments, which is the mistake the split exists to remove.
func (t *LspActionTool) Translate(args map[string]any) (operation.Spec, error) {
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, t.def.required...), t.def.optional...) {
		allowed[key] = true
	}
	merged := map[string]any{"action": t.def.action}
	for key, value := range args {
		if key == "action" {
			// A model that names an action anyway has guessed, and the guess is
			// more useful as a rejection than as something quietly overridden.
			return operation.Spec{}, fmt.Errorf(
				"%s is a single-action tool and takes no %q argument; drop it", t.Name(), "action")
		}
		if !allowed[key] {
			return operation.Spec{}, fmt.Errorf("%s does not accept %q", t.Name(), key)
		}
		merged[key] = value
	}
	var missing []string
	for _, key := range t.def.required {
		if _, present := merged[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return operation.Spec{}, fmt.Errorf("%s requires %s", t.Name(), strings.Join(missing, " and "))
	}
	return t.base.Translate(merged)
}

func (t *LspActionTool) TranslateResult(op operation.Operation) string {
	return t.base.TranslateResult(op)
}

func (t *LspActionTool) Run(spec operation.Spec) (string, error) {
	return t.base.Run(spec)
}

func (t *LspActionTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	return t.base.RunContext(ctx, spec)
}

// Sequential for the same reason the driver is: one server per root, and a
// batch is a pipeline the model expects in order.
func (t *LspActionTool) Sequential() bool        { return true }
func (t *LspActionTool) SequentialQueue() string { return "lsp" }

// LspActionTools returns the per-action tools over one shared driver.
func LspActionTools(base *LspTool) []Tool {
	tools := make([]Tool, 0, len(lspActionDefs))
	for _, def := range lspActionDefs {
		tools = append(tools, &LspActionTool{base: base, def: def})
	}
	return tools
}

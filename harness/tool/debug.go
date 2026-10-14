package tool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ceraa/harness/dap"
	"ceraa/harness/operation"
)

// The debugger answers questions the source cannot: what the process is
// actually holding right now. Like the browser and the language server, the
// action becomes the tool, so a call cannot bundle arguments belonging to
// different actions.
//
// The gate is the whole design and it is not decorative. There is no attach,
// so the agent cannot seize a process it did not start, and there is no
// evaluate, so it cannot run an expression inside the debuggee. Launch,
// breakpoints, stepping and inspection only: the powers an analysis needs and
// none of the ones a mistake could abuse.

const (
	debugLaunchTimeout = 180 * time.Second
	debugActionTimeout = 60 * time.Second
)

// DebugTool drives a real debugger through the Debug Adapter Protocol.
//
// It holds the session, because a breakpoint only means something to the
// program that was launched with it. One session at a time, and terminating it
// is explicit.
type DebugTool struct {
	mu         sync.Mutex
	manager    *dap.Manager
	newManager func(rootDir string) (*dap.Manager, error)
	timeout    time.Duration
}

// NewDebugTool returns a driver that starts dlv on demand, resolving the binary
// project-local first and then PATH. A host without dlv still constructs; the
// first call names the missing binary rather than the registry failing at
// startup.
func NewDebugTool() *DebugTool {
	return &DebugTool{
		newManager: func(rootDir string) (*dap.Manager, error) {
			path, err := dap.ResolveServer(rootDir)
			if err != nil {
				return nil, err
			}
			return dap.NewManager(path, rootDir)
		},
		timeout: debugActionTimeout,
	}
}

func (t *DebugTool) Name() string { return "debug" }

func (t *DebugTool) Description() string {
	return "Inspect a running Go program under the debugger. Session state is shared across calls; " +
		"only a program this session launched can be inspected."
}

func (t *DebugTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *DebugTool) Translate(args map[string]any) (operation.Spec, error) {
	action, _ := args["action"].(string)
	if action == "" {
		return operation.Spec{}, fmt.Errorf("debug action is required")
	}
	translated := map[string]any{"action": action}
	for _, key := range []string{"mode", "path", "line", "depth", "reference", "program", "args"} {
		if value, present := args[key]; present && value != nil {
			translated[key] = value
		}
	}
	return operation.Spec{Type: "debug", Args: translated}, nil
}

// resolveMode reads the mode argument, defaulting to the tool's first action.
func resolveMode(spec operation.Spec, fallback string) (string, error) {
	mode, _ := spec.Args["mode"].(string)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return fallback, nil
	}
	return mode, nil
}

func (t *DebugTool) TranslateResult(op operation.Operation) string {
	if op.Error != "" && op.Output == "" {
		return "Error: " + op.Error
	}
	return op.Output
}

func (t *DebugTool) Run(spec operation.Spec) (string, error) {
	return t.RunContext(context.Background(), spec)
}

// session returns the live manager, or an error naming what to do first.
func (t *DebugTool) session() (*dap.Manager, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.manager == nil {
		return nil, fmt.Errorf("debug: no session; launch a program first")
	}
	return t.manager, nil
}

func (t *DebugTool) setSession(m *dap.Manager) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.manager != nil && t.manager != m {
		t.manager.Close()
	}
	t.manager = m
}

func (t *DebugTool) clearSession() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.manager != nil {
		t.manager.Close()
		t.manager = nil
	}
}

// RunContext performs one debugger action. Paths are resolved here, not in
// Translate, because the workdir arrives on the spec after translation.
func (t *DebugTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	action, _ := spec.Args["action"].(string)
	if action == "launch" {
		return t.runLaunch(ctx, spec)
	}
	mode, err := resolveMode(spec, action)
	if err != nil {
		return "", err
	}
	switch mode {
	case "status":
		// Status is the question asked precisely when there is no session, so it
		// answers rather than refusing. Every other action needs a live session
		// and says so.
		t.mu.Lock()
		manager := t.manager
		t.mu.Unlock()
		if manager == nil {
			return "no debug session is running", nil
		}
		return formatDebugStatus(manager.Status()), nil
	case "terminate":
		manager, err := t.session()
		if err != nil {
			return "", err
		}
		if err := manager.Terminate(ctx); err != nil {
			return "", err
		}
		t.clearSession()
		return "debug session terminated", nil
	case "breakpoint":
		return t.runBreakpointAndRun(ctx, spec)
	case "continue", "step_over", "step_in", "step_out":
		return t.runStep(ctx, spec, mode)
	case "stack", "variables":
		return t.runInspect(ctx, spec, mode)
	default:
		return "", fmt.Errorf("unknown debug mode %q", mode)
	}
}

// runBreakpointAndRun sets a breakpoint, declares the set, and waits for the
// program to stop there. Declaring the set is what starts the program, so the
// first breakpoint is reached without a separate resume.
func (t *DebugTool) runBreakpointAndRun(ctx context.Context, spec operation.Spec) (string, error) {
	manager, err := t.session()
	if err != nil {
		return "", err
	}
	path, _ := spec.Args["path"].(string)
	line, err := debugPositiveInt("line", spec.Args["line"])
	if err != nil {
		return "", err
	}
	resolved, err := ResolvePathFrom(debugWorkdir(spec), path)
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	bound, verified, err := manager.SetBreakpoint(callCtx, resolved, line)
	if err != nil {
		return "", err
	}
	// The breakpoint set is only complete once declared, and the adapter will
	// not run the program until it is, so this runs on every set rather than
	// relying on the caller to remember.
	if err := manager.ConfigurationDone(callCtx); err != nil {
		return "", err
	}
	if !verified {
		return fmt.Sprintf("breakpoint requested at %s:%d, adapter bound %d but did not verify it; %s",
			resolved, line, bound, formatDebugStatus(manager.Status())), nil
	}
	return fmt.Sprintf("breakpoint bound at %s:%d\n%s", resolved, bound, formatDebugStatus(manager.Status())), nil
}

func (t *DebugTool) runStep(ctx context.Context, spec operation.Spec, mode string) (string, error) {
	manager, err := t.session()
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	switch mode {
	case "continue":
		err = manager.Continue(callCtx)
	case "step_over":
		err = manager.StepOver(callCtx)
	case "step_in":
		err = manager.StepIn(callCtx)
	case "step_out":
		err = manager.StepOut(callCtx)
	}
	if err != nil {
		return "", err
	}
	return formatDebugStatus(manager.Status()), nil
}

// runInspect reads the stopped program. A reference expands variables; without
// one it returns the stack, which is how a frame reference to expand is found.
func (t *DebugTool) runInspect(ctx context.Context, spec operation.Spec, mode string) (string, error) {
	manager, err := t.session()
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	if mode == "variables" {
		ref, err := debugPositiveInt("reference", spec.Args["reference"])
		if err != nil {
			return "", err
		}
		vars, err := manager.Variables(callCtx, ref)
		if err != nil {
			return "", err
		}
		return formatVariables(vars), nil
	}
	depth := 20
	if raw, ok := spec.Args["depth"]; ok {
		if n, err := debugPositiveInt("depth", raw); err == nil {
			depth = n
		}
	}
	frames, err := manager.StackTrace(callCtx, depth)
	if err != nil {
		return "", err
	}
	return formatStack(frames), nil
}

func (t *DebugTool) step(ctx context.Context, fn func(context.Context) error) (string, error) {
	if err := fn(ctx); err != nil {
		return "", err
	}
	manager, err := t.session()
	if err != nil {
		return "", err
	}
	return formatDebugStatus(manager.Status()), nil
}

func (t *DebugTool) runLaunch(ctx context.Context, spec operation.Spec) (string, error) {
	program, _ := spec.Args["program"].(string)
	if strings.TrimSpace(program) == "" {
		return "", fmt.Errorf("debug_launch requires program")
	}
	workdir := debugWorkdir(spec)
	resolved, err := ResolvePathFrom(workdir, program)
	if err != nil {
		return "", err
	}
	rawArgs, _ := spec.Args["args"].(string)
	var programArgs []string
	if strings.TrimSpace(rawArgs) != "" {
		programArgs = strings.Fields(rawArgs)
	}
	// A second launch replaces the session rather than failing: a model that
	// launches the same program twice after a failed inspection is making a
	// correction, not attacking a guard.
	t.clearSession()
	manager, err := t.newManager(workdir)
	if err != nil {
		return "", err
	}
	launchCtx, cancel := context.WithTimeout(ctx, debugLaunchTimeout)
	defer cancel()
	if err := manager.Launch(launchCtx, resolved, programArgs); err != nil {
		manager.Close()
		return "", err
	}
	t.setSession(manager)
	return fmt.Sprintf("launched %s; set a breakpoint then continue", resolved), nil
}

func debugWorkdir(spec operation.Spec) string {
	workdir, _ := spec.Args["workdir"].(string)
	return strings.TrimSpace(workdir)
}

// debugPositiveInt narrows a JSON-decoded number to a positive int.
func debugPositiveInt(name string, value any) (int, error) {
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
	return 0, fmt.Errorf("%s is required and must be a positive integer", name)
}

func formatDebugStatus(s dap.Status) string {
	if !s.Running {
		return "no debug session is running"
	}
	if s.Exited {
		return fmt.Sprintf("the program exited with code %d", s.ExitCode)
	}
	if s.Stopped {
		return fmt.Sprintf("stopped in thread %d; read the stack and variables", s.Thread)
	}
	return "the program is running"
}

func formatStack(frames []dap.StackFrame) string {
	if len(frames) == 0 {
		return "no stack frames"
	}
	lines := make([]string, 0, len(frames))
	for i, frame := range frames {
		lines = append(lines, fmt.Sprintf("#%d %s at %s:%d", i, frame.Name, frame.File, frame.Line))
	}
	return strings.Join(lines, "\n")
}

func formatVariables(vars []dap.Variable) string {
	if len(vars) == 0 {
		return "no variables"
	}
	lines := make([]string, 0, len(vars))
	for _, v := range vars {
		if v.Type == "scope" {
			lines = append(lines, fmt.Sprintf("%s (%s)", v.Name, v.Value))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s = %s", v.Type, v.Name, v.Value))
	}
	return strings.Join(lines, "\n")
}

// Sequential for the same reason the browser and the language server are: one
// session, and a batch is a pipeline the model expects in order.
func (t *DebugTool) Sequential() bool        { return true }
func (t *DebugTool) SequentialQueue() string { return "debug" }

// debugArgDef is one tool: the name it is called by, the actions it performs,
// and the arguments it accepts.
//
// Three tools, not ten. The per-action split solved a real problem on the
// browser - the model bundled arguments belonging to different actions into one
// call - but it was applied here mechanically and ten schemas is a permanent
// per-request cost to avoid a mistake a schema already prevents. Merging the
// actions keeps every capability and drops two thirds of the schema.
type debugArgDef struct {
	tool        string
	actions     []string
	required    []string
	optional    []string
	description string
}

// debugActionDefs describes each tool. Usage guidance is deliberately absent
// (enforced by TestDescriptionsCarryNoUsageGuidance); the prompt carries it.
var debugActionDefs = []debugArgDef{
	{tool: "launch", actions: []string{"launch"}, required: []string{"program"}, optional: []string{"args"},
		description: "Start a Go program under the debugger, replacing any current session. Args is a space-separated argument string."},
	{tool: "step", actions: []string{"breakpoint", "continue", "step_over", "step_in", "step_out"},
		optional:    []string{"mode", "path", "line"},
		description: "Control a stopped program: resume it (mode continue), advance a line (step_over), descend into a call (step_in), or return (step_out). Supplying path and line sets a breakpoint and runs the program to it."},
	{tool: "inspect", actions: []string{"stack", "variables", "status", "terminate"},
		optional:    []string{"mode", "reference", "depth"},
		description: "Read a stopped program: the call stack, the variables behind a scope reference, or the session state. Mode terminate stops the debuggee and ends the session."},
}

var debugArgDefs = map[string]struct {
	kind        string
	description string
}{
	"mode":      {"string", "Which action to take. debug_step: continue, step_over, step_in, step_out, or breakpoint (default). debug_inspect: stack, variables, status, or terminate (default stack)."},
	"program":   {"string", "Path to the Go program to launch. Relative paths resolve against the session workdir."},
	"args":      {"string", "Space-separated command-line arguments for the program."},
	"path":      {"string", "Source file for the breakpoint. Relative paths resolve against the session workdir."},
	"line":      {"integer", "1-based line number, matching read."},
	"depth":     {"integer", "How many stack frames to return, default 20."},
	"reference": {"integer", "A reference returned by a scope or variables listing."},
}

// DebugActionTool is one debugger tool: a name, a small set of actions it owns,
// and the arguments those actions accept. It holds no session of its own:
// every instance shares one DebugTool, so the program a breakpoint binds to is
// the program continue resumes.
type DebugActionTool struct {
	base *DebugTool
	def  debugArgDef
}

func (t *DebugActionTool) Name() string { return "debug_" + t.def.tool }

func (t *DebugActionTool) Description() string { return t.def.description }

func (t *DebugActionTool) Schema() map[string]any {
	properties := map[string]any{}
	for _, key := range append(append([]string{}, t.def.required...), t.def.optional...) {
		if def, known := debugArgDefs[key]; known {
			properties[key] = map[string]any{"type": def.kind, "description": def.description}
		}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(t.def.required) > 0 {
		schema["required"] = t.def.required
	}
	return schema
}

// Translate pins the action to the tool's first action and defers to the shared
// driver, so the spec shape is built once. The argument set is checked here
// against this tool's own rules.
func (t *DebugActionTool) Translate(args map[string]any) (operation.Spec, error) {
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, t.def.required...), t.def.optional...) {
		allowed[key] = true
	}
	merged := map[string]any{"action": t.def.actions[0]}
	for key, value := range args {
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

func (t *DebugActionTool) TranslateResult(op operation.Operation) string {
	return t.base.TranslateResult(op)
}

func (t *DebugActionTool) Run(spec operation.Spec) (string, error) {
	return t.base.Run(spec)
}

func (t *DebugActionTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	return t.base.RunContext(ctx, spec)
}

func (t *DebugActionTool) Sequential() bool        { return true }
func (t *DebugActionTool) SequentialQueue() string { return "debug" }

// DebugActionTools returns the three debugger tools over one shared session.
func DebugActionTools(base *DebugTool) []Tool {
	tools := make([]Tool, 0, len(debugActionDefs))
	for _, def := range debugActionDefs {
		tools = append(tools, &DebugActionTool{base: base, def: def})
	}
	return tools
}

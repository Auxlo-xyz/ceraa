package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dop251/goja"

	"ceraa/harness/operation"
)

// scriptDeadline bounds one script by default.
//
// A script exists to do in one round trip what would otherwise be a burst of
// calls, so the default is generous enough to cover a few shell commands but
// short enough that a runaway loop returns control to the model quickly.
const scriptDeadline = 30 * time.Second

// maxScriptDeadline caps what a timeout argument may ask for, so one bad
// argument cannot pin a run open.
const maxScriptDeadline = 300 * time.Second

// scriptExcluded lists tools a script may not reach.
//
// script itself would recurse without bound. question is excluded because it
// completes by answering a request the client is waiting on, and inside a script
// there is no request id the client knows about, so the call would wait forever
// with nobody able to end it.
var scriptExcluded = map[string]bool{"script": true, "question": true}

// ScriptTool runs model-written JavaScript that calls the other tools.
//
// It exists to collapse a burst. Measured over four sessions, a single turn
// issues a median of nineteen tool results, and twenty-seven of twenty-nine
// bursts are calls the model decided on before seeing any of them: the plan is
// stated in prose, then four files are read, then the answer is written. Each of
// those results costs context whether or not the answer needed all of it. One
// script that does the same work returns one value.
type ScriptTool struct {
	// Reg supplies the sibling tools, both for the declarations in the
	// description and for the lookup a nested call makes.
	Reg *Registry
	// Dispatch runs one nested operation. Set by the coordinator, which owns the
	// operation manager; the tool layer cannot reach it on its own.
	Dispatch func(ctx context.Context, spec operation.Spec) (string, error)
	// Scope carries the coordinator's per-call spec arguments, so a nested call
	// runs in the same workdir, goal dir and session as the call owning the
	// script. Without it a nested read would resolve relative paths against the
	// process directory and miss a file the model had just written.
	Scope map[string]any
	// RequiresApproval mirrors the coordinator's own gate. The coordinator asks
	// before a tool call reaches dispatch, and a nested call never passes through
	// it, so without this a script would write a file or run a destructive command
	// with nobody asked. A script cannot prompt, so it refuses and says so.
	RequiresApproval func(name string, args map[string]any) bool
	// Deadline overrides the default. Zero means scriptDeadline.
	Deadline time.Duration
}

// Attach wires the registry, the dispatcher and the scope. Called once, after
// the registry exists, because the tool needs a registry it lives in.
func (s *ScriptTool) Attach(reg *Registry, dispatch func(context.Context, operation.Spec) (string, error), scope map[string]any) {
	s.Reg = reg
	s.Dispatch = dispatch
	s.Scope = scope
}

func (s *ScriptTool) Name() string { return "script" }

// Description states the contract only. When to reach for it is guidance, and
// guidance belongs in the prompt's toolrules section; TestDescriptionsCarryNoUsageGuidance
// pins that split.
func (s *ScriptTool) Description() string {
	return "Run JavaScript and return a value. code is the body " +
		"of an async function, so await and Promise.all work. tools.NAME(args) " +
		"takes NAME's own arguments and resolves to its result text; a call that " +
		"fails throws, and the message says what to change. A tool needing your " +
		"approval cannot be called from a script. Tools: " + s.exposedNames() + "."
}

func (s *ScriptTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code":    map[string]any{"type": "string", "description": "JavaScript to run. The body of an async function."},
			"timeout": map[string]any{"type": "integer", "description": "Seconds to allow (default 30, max 300)."},
		},
		"required": []string{"code"},
	}
}

func (s *ScriptTool) Translate(args map[string]any) (operation.Spec, error) {
	raw, _ := args["code"].(string)
	if strings.TrimSpace(raw) == "" {
		return operation.Spec{}, fmt.Errorf("code is required")
	}
	spec := map[string]any{"code": raw}
	if seconds, ok := positiveIntArg(args["timeout"]); ok {
		spec["timeout"] = seconds
	}
	return operation.Spec{Type: "script", Args: spec}, nil
}

func (s *ScriptTool) TranslateResult(op operation.Operation) string { return op.Output }

// Sequential keeps a script from running alongside another operation that
// writes. The manager holds the serial lock for the whole run, so nested writes
// made from inside the script are already exclusive with respect to every other
// sequential operation.
func (s *ScriptTool) Sequential() bool { return true }

func (s *ScriptTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	if s.Reg == nil || s.Dispatch == nil {
		return "", fmt.Errorf("script tool is not wired: registry or dispatcher missing")
	}
	raw, _ := spec.Args["code"].(string)
	code := normalizeScript(raw)
	if code == "" {
		return "", fmt.Errorf("code is required")
	}

	deadline := s.Deadline
	if deadline <= 0 {
		deadline = scriptDeadline
	}
	if seconds, ok := positiveIntArg(spec.Args["timeout"]); ok {
		deadline = time.Duration(seconds) * time.Second
	}
	if deadline > maxScriptDeadline {
		deadline = maxScriptDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	rt := goja.New()
	if err := s.exposeTools(rt, ctx); err != nil {
		return "", err
	}

	program, _, err := compileScript("script", code)
	if err != nil {
		return "", fmt.Errorf("the code does not parse: %s", relocateSyntaxError(err))
	}

	// Interrupt is what makes the deadline real. goja runs the script on this
	// goroutine, so a cancel would otherwise never be observed by a loop that
	// never yields.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			rt.Interrupt("script deadline exceeded after " + deadline.String())
		case <-stopped:
		}
	}()

	value, err := rt.RunProgram(program)
	if err != nil {
		return "", fmt.Errorf("the script failed: %s", s.explainToolCallError(err))
	}
	if err := waitSettled(value); err != nil {
		return "", fmt.Errorf("the script failed: %s", s.explainToolCallError(err))
	}
	out := formatScriptValue(settledResult(value))
	if out == "" {
		return "Script finished and returned nothing. Return a string, or the " +
			"answer will not reach the next step.", nil
	}
	return out, nil
}

// exposedNames lists the tools the script can reach, sorted by the registry.
//
// A family of tools that differs only by a suffix is written once in brace form.
// Splitting the browser into one tool per action added nine names here, which
// pushed the script description past its size budget on its own. The suffix list
// is kept rather than elided, because `browser_{...}` would tell the model
// nothing it could call.
func (s *ScriptTool) exposedNames() string {
	if s.Reg == nil {
		return ""
	}
	var names []string
	familyEmitted := map[string]bool{}
	for _, t := range s.Reg.Offered() {
		if scriptExcluded[t.Name()] {
			continue
		}
		family, ok := exposedFamily(t.Name())
		if !ok {
			names = append(names, t.Name())
			continue
		}
		if familyEmitted[family] {
			continue
		}
		familyEmitted[family] = true
		names = append(names, family+"_{"+strings.Join(exposedFamilySuffixes(s.Reg, family), ",")+"}")
	}
	return strings.Join(names, ", ")
}

// exposedFamily is the shared prefix of a group of tools called the same way,
// or "" when the name belongs to no group.
func exposedFamily(name string) (string, bool) {
	const prefix = "browser_"
	if !strings.HasPrefix(name, prefix) || name == prefix {
		return "", false
	}
	return "browser", true
}

func exposedFamilySuffixes(reg *Registry, family string) []string {
	prefix := family + "_"
	var suffixes []string
	for _, t := range reg.Offered() {
		if scriptExcluded[t.Name()] {
			continue
		}
		if strings.HasPrefix(t.Name(), prefix) && len(t.Name()) > len(prefix) {
			suffixes = append(suffixes, strings.TrimPrefix(t.Name(), prefix))
		}
	}
	return suffixes
}

// exposeTools publishes one host function per sibling tool.
//
// Each call translates through the sibling's own Translate, so a script cannot
// reach an operation the tool itself would have refused, and the arguments are
// validated by the same code the coordinator would have run.
func (s *ScriptTool) exposeTools(rt *goja.Runtime, ctx context.Context) error {
	tools := rt.NewObject()
	var attached int
	// Offered, not All, so the surface a script can reach is exactly the one the
	// model was told about. A hidden tool reachable from a script would be a tool
	// the model uses without having been offered it.
	for _, sibling := range s.Reg.Offered() {
		if scriptExcluded[sibling.Name()] {
			continue
		}
		attached++
		name := sibling.Name()
		if err := tools.Set(name, s.hostFunc(rt, ctx, name, sibling)); err != nil {
			return fmt.Errorf("expose %s: %w", name, err)
		}
	}
	if attached == 0 {
		return fmt.Errorf("script has no tools to call")
	}
	return rt.Set("tools", tools)
}

func (s *ScriptTool) hostFunc(rt *goja.Runtime, ctx context.Context, name string, sibling Tool) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(rt.ToValue("tools." + name + "() needs an object of arguments; see the schema for " + name))
		}
		args := map[string]any{}
		if err := rt.ExportTo(call.Arguments[0], &args); err != nil {
			// A bare string is the common mistake, so the message says what was
			// received rather than only what was wanted.
			panic(rt.ToValue(fmt.Sprintf("tools.%s was given %s; it takes an object of arguments such as {%q: ...}",
				name, jsonTypeName(call.Arguments[0]), firstRequiredArg(sibling.Schema()))))
		}
		if s.RequiresApproval != nil && s.RequiresApproval(name, args) {
			panic(rt.ToValue("tools." + name + " needs permission, and a script cannot " +
				"ask for it; call " + name + " on its own so the request can be approved"))
		}
		spec, err := sibling.Translate(args)
		if err != nil {
			panic(rt.ToValue("tools." + name + " rejected the arguments: " + err.Error()))
		}
		if s.Scope != nil {
			if spec.Args == nil {
				spec.Args = map[string]any{}
			}
			for key, value := range s.Scope {
				if _, present := spec.Args[key]; !present {
					spec.Args[key] = value
				}
			}
		}
		out, err := s.Dispatch(ctx, spec)
		if err != nil {
			panic(rt.ToValue("tools." + name + " failed: " + err.Error()))
		}
		return rt.ToValue(sibling.TranslateResult(operation.Operation{
			ID: "script", Spec: spec, Status: operation.StatusCompleted, Output: out,
		}))
	}
}

// firstRequiredArg names one required argument, so an error message can show the
// shape it expected instead of a bare "object expected".
func firstRequiredArg(schema map[string]any) string {
	required, _ := schema["required"].([]string)
	if len(required) == 0 {
		if anyRequired, ok := schema["required"].([]any); ok {
			for _, entry := range anyRequired {
				if key, ok := entry.(string); ok {
					return key
				}
			}
		}
		return "argument"
	}
	return required[0]
}

func jsonTypeName(v goja.Value) string {
	switch {
	case v == nil, goja.IsUndefined(v):
		return "nothing"
	case goja.IsNull(v):
		return "null"
	}
	switch exported := v.Export().(type) {
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case map[string]any, []any:
		return "an object"
	default:
		return fmt.Sprintf("%T", exported)
	}
}

var (
	fenceStartRe    = regexp.MustCompile("^```[a-zA-Z0-9_+-]*\\s*$")
	functionWrapRe  = regexp.MustCompile(`^(async\s+)?function\s*[\w$]*\s*\(|^(async\s+)?\([^)]*\)\s*=>|^(async\s+)?[\w$]+\s*=>`)
	syntaxLineRe    = regexp.MustCompile(`Line (\d+):`)
	restAfterBodyRe = regexp.MustCompile(`^[\s();]*$`)
)

// normalizeScript accepts what models actually send.
//
// A model asked for a string will sometimes emit a fenced block, and sometimes
// emit the whole argument object back as JSON. Both parse in a browser console
// and both fail here, and the failure reads as the tool being broken rather than
// the shape being wrong. Unwrapping costs a few lines and removes a whole class
// of "models could not call it properly".
func normalizeScript(raw string) string {
	code := strings.TrimSpace(raw)

	if strings.HasPrefix(code, "```") {
		lines := strings.Split(code, "\n")
		if fenceStartRe.MatchString(lines[0]) {
			lines = lines[1:]
		}
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			lines = lines[:len(lines)-1]
		}
		code = strings.TrimSpace(strings.Join(lines, "\n"))
	}

	if strings.HasPrefix(code, "{") {
		var wrapped struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(code), &wrapped); err == nil && strings.TrimSpace(wrapped.Code) != "" {
			code = strings.TrimSpace(wrapped.Code)
		}
	}
	return code
}

// extractFunctionBody unwraps a whole function, when the model sent one instead
// of a body. It is tried only after the code as given fails to parse, so a
// legitimate top level function declaration is never torn apart.
func extractFunctionBody(code string) (string, bool) {
	trimmed := strings.TrimSpace(code)
	if !functionWrapRe.MatchString(trimmed) {
		return "", false
	}
	open := strings.IndexByte(trimmed, '{')
	if open < 0 {
		return "", false
	}
	depth := 0
	for i := open; i < len(trimmed); i++ {
		switch trimmed[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				rest := strings.TrimSpace(trimmed[i+1:])
				if rest != "" && !restAfterBodyRe.MatchString(rest) {
					return "", false
				}
				return strings.TrimSpace(trimmed[open+1 : i]), true
			}
		}
	}
	return "", false
}

// wrapAsync puts the code where the contract says it is: the body of an async
// function. One newline at the front, so syntax line numbers shift by one and
// relocateSyntaxError can put them back.
func wrapAsync(code string) string {
	return "(async function(){\n" + code + "\n})()"
}

// compileScript parses the code as given, then repairs the shapes models send.
// The first error is the one reported, because it describes the code the model
// actually wrote rather than a repair it never attempted.
func compileScript(name, code string) (*goja.Program, string, error) {
	first, firstErr := goja.Compile(name, wrapAsync(code), false)
	if firstErr == nil {
		return first, code, nil
	}
	if body, ok := extractFunctionBody(code); ok {
		if program, err := goja.Compile(name, wrapAsync(body), false); err == nil {
			return program, body, nil
		}
	}
	return nil, "", firstErr
}

// relocateSyntaxError moves reported line numbers back onto the model's code.
// The wrapper adds a line before it, and a line number that is one out sends the
// model looking at the wrong statement.
func relocateSyntaxError(err error) string {
	if err == nil {
		return ""
	}
	return syntaxLineRe.ReplaceAllStringFunc(err.Error(), func(match string) string {
		groups := syntaxLineRe.FindStringSubmatch(match)
		if len(groups) != 2 {
			return match
		}
		line := 0
		if _, scanErr := fmt.Sscanf(groups[1], "%d", &line); scanErr != nil {
			return match
		}
		if line < 2 {
			return match
		}
		return fmt.Sprintf("Line %d:", line-1)
	})
}

// missingToolRe matches what the engine says when a name is not on the object.
// The bare form names the tool and nothing else, which leaves the model to guess
// what is there instead of being told.
var missingToolRe = regexp.MustCompile(`tools\.([\w$]+) is not a function|Object has no member '([\w$]+)'`)

// explainToolCallError repairs the one runtime failure worth repairing. Every
// other failure already says what went wrong at the line it went wrong on; this
// one says only that a property is missing, and the fix is a different name.
func (s *ScriptTool) explainToolCallError(err error) string {
	message := relocateSyntaxError(err)
	if groups := missingToolRe.FindStringSubmatch(message); groups != nil {
		missing := groups[1]
		if missing == "" {
			missing = groups[2]
		}
		return fmt.Sprintf("tools.%s does not exist. Available: %s", missing, s.exposedNames())
	}
	return message
}

// waitSettled drains a returned promise. Host calls are synchronous, so the
// promise is already settled by the time RunProgram returns; this covers a
// script that resolves through Promise.all or .then.
func waitSettled(value goja.Value) error {
	promise, ok := value.Export().(*goja.Promise)
	if !ok {
		return nil
	}
	if promise.State() == goja.PromiseStateRejected {
		// A thrown value is rendered by its own toString, not by exporting it:
		// an engine TypeError exports as an empty map, which reads as "{}" and
		// tells the model nothing about what went wrong.
		return errors.New(promise.Result().String())
	}
	return nil
}

func settledResult(value goja.Value) goja.Value {
	if promise, ok := value.Export().(*goja.Promise); ok {
		return promise.Result()
	}
	return value
}

// formatScriptValue renders the return value the way the model can use it.
// Strings pass through untouched, because that is the answer; anything else is
// JSON, because a Go fmt of a struct is not a shape a model can parse reliably.
func formatScriptValue(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	exported := v.Export()
	if text, ok := exported.(string); ok {
		return text
	}
	encoded, err := json.MarshalIndent(exported, "", "  ")
	if err != nil {
		return fmt.Sprint(exported)
	}
	return string(encoded)
}

package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ceraa/harness/operation"
)

// scriptStub stands in for a sibling tool. The dispatcher is injected, so only
// Translate has to be real: it is the gate a nested call passes through, and a
// script must not reach an operation the tool itself would have refused.
type scriptStub struct {
	name     string
	required []string
	received map[string]any
	refuse   string
}

func (s *scriptStub) Name() string        { return s.name }
func (s *scriptStub) Description() string { return "stub " + s.name }
func (s *scriptStub) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   s.required,
	}
}
func (s *scriptStub) Translate(args map[string]any) (operation.Spec, error) {
	if s.refuse != "" {
		return operation.Spec{}, errors.New(s.refuse)
	}
	s.received = args
	return operation.Spec{Type: s.name, Args: args}, nil
}
func (s *scriptStub) TranslateResult(op operation.Operation) string { return op.Output }

// scriptHarness wires a ScriptTool to stub siblings and a dispatcher that
// records what a nested call asked for.
type scriptHarness struct {
	tool   *ScriptTool
	stub   *scriptStub
	asked  operation.Spec
	output string
	fail   error
}

func newScriptHarness(t *testing.T) *scriptHarness {
	t.Helper()
	h := &scriptHarness{stub: &scriptStub{name: "probe", required: []string{"value"}}}
	reg := NewRegistry()
	reg.Register(h.stub)
	script := &ScriptTool{}
	reg.Register(script)
	script.Reg = reg
	script.Dispatch = func(_ context.Context, spec operation.Spec) (string, error) {
		h.asked = spec
		return h.output, h.fail
	}
	h.tool = script
	return h
}

func (h *scriptHarness) run(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	spec, err := h.tool.Translate(args)
	if err != nil {
		return "", err
	}
	return h.tool.RunContext(context.Background(), spec)
}

func TestScriptCallsASiblingAndReturnsItsText(t *testing.T) {
	h := newScriptHarness(t)
	h.output = "42 lines"
	out, err := h.run(t, map[string]any{
		"code": `const r = await tools.probe({value: "x"}); return r;`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "42 lines" {
		t.Fatalf("got %q", out)
	}
	if h.asked.Type != "probe" {
		t.Fatalf("nested spec type %q", h.asked.Type)
	}
	if h.asked.Args["value"] != "x" {
		t.Fatalf("nested args %v", h.asked.Args)
	}
}

// The scope is what makes a nested call behave like the call that owns the
// script. Without workdir, a relative path inside a script resolves somewhere
// else than the same path outside one, and the file looks missing.
func TestScriptScopeReachesANestedCall(t *testing.T) {
	h := newScriptHarness(t)
	h.tool.Scope = map[string]any{"workdir": "/srv/proj", "session_id": "ses_1"}
	h.output = "ok"
	if _, err := h.run(t, map[string]any{"code": `return await tools.probe({value: "v"});`}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.asked.Args["workdir"] != "/srv/proj" || h.asked.Args["session_id"] != "ses_1" {
		t.Fatalf("scope did not reach the nested call: %v", h.asked.Args)
	}
}

// A sibling that sets its own workdir keeps it: the scope fills what is absent,
// and overwriting would let a script relocate a call the tool had anchored.
func TestScriptScopeDoesNotOverwriteAChoiceTheToolMade(t *testing.T) {
	h := newScriptHarness(t)
	h.tool.Scope = map[string]any{"workdir": "/srv/elsewhere"}
	h.output = "ok"
	if _, err := h.run(t, map[string]any{
		"code": `return await tools.probe({value: "v", workdir: "/srv/chosen"});`,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.asked.Args["workdir"] != "/srv/chosen" {
		t.Fatalf("workdir %v, want the one the tool was given", h.asked.Args["workdir"])
	}
}

func TestScriptThrowsWithTheToolNameWhenACallFails(t *testing.T) {
	h := newScriptHarness(t)
	h.fail = errors.New("no such file")
	_, err := h.run(t, map[string]any{"code": `return await tools.probe({value: "x"});`})
	if err == nil {
		t.Fatal("expected the failure to surface")
	}
	if !strings.Contains(err.Error(), "tools.probe failed: no such file") {
		t.Fatalf("message %q does not name the tool and the cause", err.Error())
	}
}

// Translate is the gate. A script must not reach an operation the sibling would
// itself have refused, or it becomes a way around a validation the coordinator
// applies on every other path.
func TestScriptCannotRouteAroundARefusal(t *testing.T) {
	h := newScriptHarness(t)
	h.stub.refuse = "value is required"
	_, err := h.run(t, map[string]any{"code": `return await tools.probe({});`})
	if err == nil {
		t.Fatal("expected the refusal")
	}
	if !strings.Contains(err.Error(), "rejected the arguments: value is required") {
		t.Fatalf("message %q", err.Error())
	}
}

func TestScriptReachesNoToolItCannotName(t *testing.T) {
	h := newScriptHarness(t)
	h.output = "ok"
	_, err := h.run(t, map[string]any{"code": `return await tools.absent({});`})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "tools.absent does not exist. Available: probe") {
		t.Fatalf("message %q does not list what exists", err.Error())
	}
}

// question completes by answering a request the client is waiting on, and inside
// a script there is no request id the client knows, so it would wait forever.
func TestScriptRefusesToReachItselfOrQuestion(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&scriptStub{name: "probe"})
	reg.Register(&scriptStub{name: "question"})
	script := &ScriptTool{}
	reg.Register(script)
	script.Reg = reg

	names := script.exposedNames()
	if strings.Contains(names, "question") {
		t.Errorf("question is reachable from a script: %s", names)
	}
	if strings.Contains(names, "script") {
		t.Errorf("script can reach itself: %s", names)
	}
	if !strings.Contains(names, "probe") {
		t.Errorf("a sibling is missing: %s", names)
	}
}

func TestScriptIsNotRegisteredWhenDisabled(t *testing.T) {
	t.Setenv("CERAA_SCRIPT", "off")
	if _, ok := DefaultRegistry().Get("script"); ok {
		t.Fatal("script is registered while CERAA_SCRIPT=off")
	}
}

// A model asked for a string will emit a fenced block or the argument object
// back as JSON. Both work in a console and both fail here, and the failure reads
// as the tool being broken rather than the shape being wrong.
func TestScriptNormalizesWhatModelsSend(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", "return 1;", "return 1;"},
		{"fenced", "```js\nreturn 1;\n```", "return 1;"},
		{"fenced bare", "```\nreturn 1;\n```", "return 1;"},
		{"argument object", `{"code":"return 1;"}`, "return 1;"},
		{"surrounding space", "  \n return 1; \n ", "return 1;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeScript(tc.in); got != tc.want {
				t.Errorf("normalizeScript(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Only after the code as given fails to parse, so a top level function
// declaration the script actually needs is never torn apart.
func TestScriptUnwrapsAWholeFunctionOnlyWhenTheBodyWontParse(t *testing.T) {
	body, ok := extractFunctionBody("async function main() { return 1; }")
	if !ok || body != "return 1;" {
		t.Fatalf("body %q ok=%v", body, ok)
	}
	if _, ok := extractFunctionBody("function helper() { return 1; }\nreturn helper();"); ok {
		t.Fatal("a top level declaration was treated as a wrapper")
	}
	program, _, err := compileScript("script", "function helper() { return 1; }\nreturn helper();")
	if err != nil || program == nil {
		t.Fatalf("a valid program was not accepted as given: %v", err)
	}
}

// A line number that is one out sends the model looking at the wrong statement.
func TestScriptReportsSyntaxErrorsOnTheModelsOwnLine(t *testing.T) {
	_, _, err := compileScript("script", "\n\nconst = ;")
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	message := relocateSyntaxError(err)
	if !strings.Contains(message, "Line 3:") {
		t.Fatalf("message %q is not on the model's line", message)
	}
	if strings.Contains(message, "Line 4:") {
		t.Fatalf("message %q still carries the wrapper's line", message)
	}
}

func TestScriptRefusesAnEmptyProgram(t *testing.T) {
	h := newScriptHarness(t)
	_, err := h.run(t, map[string]any{"code": "   "})
	if err == nil || !strings.Contains(err.Error(), "code is required") {
		t.Fatalf("err = %v", err)
	}
}

// The deadline is what stops a runaway loop, and goja runs the script on this
// goroutine, so it has to be an interrupt rather than a cancelled context the
// script never observes.
func TestScriptStopsAtItsDeadline(t *testing.T) {
	h := newScriptHarness(t)
	h.tool.Deadline = 120 * time.Millisecond
	start := time.Now()
	_, err := h.run(t, map[string]any{"code": "for (;;) {}"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the deadline to fire")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("message %q", err.Error())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s, the interrupt did not stop the loop", elapsed)
	}
}

// Returning nothing is not an answer, and a silent success would leave the model
// believing it had reported something.
func TestScriptSaysSoWhenTheProgramReturnsNothing(t *testing.T) {
	h := newScriptHarness(t)
	out, err := h.run(t, map[string]any{"code": "const x = 1;"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "returned nothing") {
		t.Fatalf("got %q", out)
	}
}

// A non-string return is rendered as JSON, because a Go format of a struct is
// not a shape a model parses reliably.
func TestScriptRendersANonStringReturnAsJSON(t *testing.T) {
	h := newScriptHarness(t)
	out, err := h.run(t, map[string]any{"code": "return {a: 1, b: [2, 3]};"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, `"a": 1`) || !strings.Contains(out, "[") {
		t.Fatalf("got %q", out)
	}
}

// Promise.all is the fan-out the feature exists for, and it has to settle.
func TestScriptFanOutWithPromiseAll(t *testing.T) {
	h := newScriptHarness(t)
	h.output = "r"
	out, err := h.run(t, map[string]any{
		"code": `const all = await Promise.all([
			tools.probe({value: "a"}),
			tools.probe({value: "b"}),
		]);
		return all.join("|");`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "r|r" {
		t.Fatalf("got %q", out)
	}
}

func TestScriptDescriptionNamesItsContract(t *testing.T) {
	h := newScriptHarness(t)
	description := h.tool.Description()
	for _, want := range []string{"async function", "tools.NAME", "Tools exposed: probe"} {
		if !strings.Contains(description, want) {
			t.Errorf("description is missing %q", want)
		}
	}
}

func TestScriptSchemaHasOneRequiredString(t *testing.T) {
	h := newScriptHarness(t)
	schema := h.tool.Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "code" {
		t.Fatalf("required = %v, want exactly [code]", required)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["code"].(map[string]any)["type"].(string); !ok {
		t.Fatal("code has no type")
	}
}

func TestScriptSequential(t *testing.T) {
	h := newScriptHarness(t)
	if !h.tool.Sequential() {
		t.Fatal("a script must not run alongside another operation that writes")
	}
}

func TestScriptNotWiredIsAClearFailure(t *testing.T) {
	bare := &ScriptTool{}
	_, err := bare.RunContext(context.Background(), operation.Spec{
		Type: "script",
		Args: map[string]any{"code": "return 1;"},
	})
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("err = %v", err)
	}
}

package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/harness/operation"
)

func reproduceArgs() map[string]any {
	return map[string]any{
		"claim":         "tainted input reaches exec",
		"source_path":   "source.go",
		"sink_path":     "sink.go",
		"source_line":   2,
		"sink_line":     2,
		"source_symbol": "userInput",
		"sink_symbol":   "exec.Command",
	}
}

func TestReproduceRejectsMissingClaim(t *testing.T) {
	args := reproduceArgs()
	delete(args, "claim")
	if _, err := (&ReproduceTool{}).Translate(args); err == nil {
		t.Error("accepted a call with no claim")
	}
}

func TestReproduceRejectsMissingEndpointField(t *testing.T) {
	args := reproduceArgs()
	delete(args, "sink_symbol")
	_, err := (&ReproduceTool{}).Translate(args)
	if err == nil {
		t.Fatal("accepted a call with no sink_symbol")
	}
	if !strings.Contains(err.Error(), "sink_symbol") {
		t.Errorf("message = %q, want it to name the missing field", err)
	}
}

func TestReproduceRejectsNonPositiveLine(t *testing.T) {
	args := reproduceArgs()
	args["source_line"] = 0
	if _, err := (&ReproduceTool{}).Translate(args); err == nil {
		t.Error("accepted a zero source_line")
	}
	args = reproduceArgs()
	args["sink_line"] = float64(-3)
	if _, err := (&ReproduceTool{}).Translate(args); err == nil {
		t.Error("accepted a negative sink_line")
	}
}

func TestReproduceRejectsUnknownArg(t *testing.T) {
	args := reproduceArgs()
	args["character"] = 4
	_, err := (&ReproduceTool{}).Translate(args)
	if err == nil {
		t.Fatal("accepted an argument it cannot use")
	}
	if !strings.Contains(err.Error(), "does not accept") {
		t.Errorf("message = %q, want the refusal", err)
	}
}

// A model that names an action on a single-action tool has guessed. Saying so
// is better than quietly overriding it.
func TestReproduceRejectsAnActionArgument(t *testing.T) {
	args := reproduceArgs()
	args["action"] = "reproduce"
	_, err := (&ReproduceTool{}).Translate(args)
	if err == nil {
		t.Fatal("expected the call to be rejected")
	}
	if !strings.Contains(err.Error(), "single-action tool") {
		t.Errorf("message = %q, want it to explain the tool takes no action argument", err)
	}
}

func reproduceSeamRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(&ReproduceTool{})
	return reg
}

func reproduceSeamFixture(t *testing.T, sinkLine string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	source := "package main\nuserInput := r.URL.Query().Get(\"q\")\n"
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	sink := "package main\n" + sinkLine + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sink.go"), []byte(sink), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return dir
}

func reproduceSeamRun(t *testing.T, reg *Registry, dir string, args map[string]any) string {
	t.Helper()
	tool, ok := reg.Get("reproduce")
	if !ok {
		t.Fatal("reproduce not registered, so specs of type reproduce cannot execute")
	}
	spec, err := tool.Translate(args)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if spec.Type != "reproduce" {
		t.Fatalf("spec type = %q, want reproduce", spec.Type)
	}
	spec.Args["workdir"] = dir
	opMgr := operation.NewWithBuffer(&seamRunner{reg: reg}, 8)
	opMgr.Add(operation.Operation{ID: "op1", Spec: spec})
	select {
	case result := <-opMgr.Results():
		if result.Error != "" {
			t.Fatalf("result error: %s", result.Error)
		}
		return result.Output
	case <-time.After(30 * time.Second):
		t.Fatal("no result from the operation manager")
		return ""
	}
}

// TestReproduceSeamTranslateDispatchExecute drives a claim from Translate
// through a real operation manager to the file reads, reading the verdict off
// Results(). If the spec type ever stops resolving to the registered tool,
// this fails: a tool that is only registered is offered to the model and then
// fails on every call.
func TestReproduceSeamTranslateDispatchExecute(t *testing.T) {
	dir := reproduceSeamFixture(t, "cmd := exec.Command(userInput)")
	reg := reproduceSeamRegistry()
	output := reproduceSeamRun(t, reg, dir, reproduceArgs())
	if !strings.HasPrefix(output, "VERDICT: PROVEN") {
		t.Errorf("output = %q, want it to start with VERDICT: PROVEN", output)
	}
}

// A sink line that never carried the sink symbol is a stale endpoint, and the
// claim is refuted rather than confirmed.
func TestReproduceSeamRefutesAnAbsentSinkSymbol(t *testing.T) {
	dir := reproduceSeamFixture(t, "fmt.Println(userInput)")
	reg := reproduceSeamRegistry()
	output := reproduceSeamRun(t, reg, dir, reproduceArgs())
	if !strings.HasPrefix(output, "VERDICT: REFUTED") {
		t.Errorf("output = %q, want it to start with VERDICT: REFUTED", output)
	}
}

// Gated on the problem, not the tool: the catalogue carries reproduce only
// while the conversation is about vulnerabilities, so steady-state requests
// pay no schema for a job that does not exist yet.
func TestReproduceIsGatedOnVulnIntent(t *testing.T) {
	withIntent := DefaultRegistryForUser("trace this taint to the vuln sink", nil)
	if _, ok := withIntent.Get(ReproduceName); !ok {
		t.Error("reproduce missing despite vuln intent")
	}
	plain := DefaultRegistryForUser("list the files in this project", nil)
	if _, ok := plain.Get(ReproduceName); ok {
		t.Error("reproduce offered without vuln intent, taxing the steady-state schema")
	}
	for _, tool := range plain.Offered() {
		if tool.Name() == ReproduceName {
			t.Error("reproduce offered without vuln intent")
		}
	}
}

// seamRunner resolves specs through a registry exactly like the coordinator's
// toolRunner, so the test above proves the seam: Translate to operation manager
// to executor to tool. It moved here from the language-server tests when those
// were retired, because this is the only remaining test that drives the whole
// path.
type seamRunner struct {
	reg *Registry
}

func (r *seamRunner) RunTool(ctx context.Context, spec operation.Spec) (string, error) {
	t, ok := r.reg.Get(spec.Type)
	if !ok {
		return "", fmt.Errorf("unknown tool type: %s", spec.Type)
	}
	if runner, ok := t.(operation.ContextRunner); ok {
		return runner.RunContext(ctx, spec)
	}
	runner, ok := t.(Runner)
	if !ok {
		return "", fmt.Errorf("tool %s does not support Run", spec.Type)
	}
	return runner.Run(spec)
}

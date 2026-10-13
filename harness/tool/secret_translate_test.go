package tool

import (
	"testing"
	"time"

	"ceraa/harness/operation"
)

// The timeout has to be declared in the schema (validation rejects unknown
// properties) and carried into the spec raw, exactly as bash carries it, so
// SpecTimeout honors it the same way it does for the shell.
func TestRunCommandWithSecretTranslatesTheTimeout(t *testing.T) {
	tool := &RunCommandWithSecretTool{}

	props, _ := tool.Schema()["properties"].(map[string]any)
	timeout, ok := props["timeout"].(map[string]any)
	if !ok {
		t.Fatal("the schema declares no timeout property")
	}
	if timeout["type"] != "integer" {
		t.Errorf("timeout type = %v, want integer", timeout["type"])
	}
	if want := "Timeout in seconds (default 900, max 3600)."; timeout["description"] != want {
		t.Errorf("timeout description = %v, want %q", timeout["description"], want)
	}

	args := map[string]any{
		"command": "echo hi", "secret": "gh_token", "env_var": "TOKEN", "timeout": 30,
	}
	spec, err := tool.Translate(args)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := spec.Args["timeout"]; got != 30 {
		t.Errorf("spec timeout = %v, want 30", got)
	}
	if d := operation.SpecTimeout(spec); d != 30*time.Second {
		t.Errorf("SpecTimeout = %v, want 30s", d)
	}

	// Without the argument the spec carries nil and SpecTimeout falls back to
	// the shell default rather than a zero wait.
	delete(args, "timeout")
	spec, err = tool.Translate(args)
	if err != nil {
		t.Fatalf("translate without timeout: %v", err)
	}
	if spec.Args["timeout"] != nil {
		t.Errorf("spec timeout = %v, want nil", spec.Args["timeout"])
	}
	if d := operation.SpecTimeout(spec); d <= 0 {
		t.Errorf("SpecTimeout = %v, want the shell default", d)
	}
}

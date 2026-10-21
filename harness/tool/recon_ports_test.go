package tool

import (
	"strings"
	"testing"

	"ceraa/harness/operation"
	"ceraa/harness/scan"
)

// TestPortsTranslateRequiresHost: without a host there is no operation, and
// the error says so rather than failing later at the scan.
func TestPortsTranslateRequiresHost(t *testing.T) {
	tl := &PortsTool{}
	for _, args := range []map[string]any{
		{},
		{"host": ""},
		{"host": 42},
	} {
		if spec, err := tl.Translate(args); err == nil {
			t.Errorf("Translate(%v) accepted, spec = %v", args, spec)
		}
	}
}

// TestPortsTranslateBuildsTheSpec: the operation type is the registered
// tool name, because the runner resolves the executor by it.
func TestPortsTranslateBuildsTheSpec(t *testing.T) {
	tl := &PortsTool{}

	spec, err := tl.Translate(map[string]any{"host": "example.com"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if spec.Type != "ports" {
		t.Errorf("spec.Type = %q, want ports", spec.Type)
	}
	if spec.Args["host"] != "example.com" {
		t.Errorf("spec.Args[host] = %v", spec.Args["host"])
	}
	if _, ok := spec.Args["ports"]; ok {
		t.Errorf("an absent list was put in the spec: %v", spec.Args)
	}
}

// TestPortsTranslateReadsEveryArgument: the canonical argument map carries
// what the caller asked for.
func TestPortsTranslateReadsEveryArgument(t *testing.T) {
	tl := &PortsTool{}
	spec, err := tl.Translate(map[string]any{
		"host":        "example.com",
		"ports":       []any{float64(80), float64(443)},
		"top":         float64(50),
		"timeout":     float64(3),
		"concurrency": float64(8),
		"max_ports":   float64(20),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if ports, _ := spec.Args["ports"].([]int); len(ports) != 2 || ports[0] != 80 {
		t.Errorf("spec.Args[ports] = %v", spec.Args["ports"])
	}
	if top, _ := spec.Args["top"].(int); top != 50 {
		t.Errorf("spec.Args[top] = %v", spec.Args["top"])
	}
	if timeout, _ := spec.Args["timeout"].(int); timeout != 3 {
		t.Errorf("spec.Args[timeout] = %v", spec.Args["timeout"])
	}
	if concurrency, _ := spec.Args["concurrency"].(int); concurrency != 8 {
		t.Errorf("spec.Args[concurrency] = %v", spec.Args["concurrency"])
	}
	if maxPorts, _ := spec.Args["max_ports"].(int); maxPorts != 20 {
		t.Errorf("spec.Args[max_ports] = %v", spec.Args["max_ports"])
	}
}

// TestPortsName pins the registry name, which is also the operation type.
func TestPortsName(t *testing.T) {
	tl := &PortsTool{}
	if tl.Name() != "ports" {
		t.Errorf("Name = %q, want ports", tl.Name())
	}
	spec, err := tl.Translate(map[string]any{"host": "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Type != tl.Name() {
		t.Errorf("spec.Type = %q and Name = %q; the runner resolves the executor by the type", spec.Type, tl.Name())
	}
}

// TestPortsDescriptionNamesTheContract: the model reads this, so it has to
// say that the wall applies and what the three states mean.
func TestPortsDescriptionNamesTheContract(t *testing.T) {
	description := (&PortsTool{}).Description()
	for _, want := range []string{"open", "closed", "filtered"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q: %s", want, description)
		}
	}
	if !strings.Contains(description, "refused") {
		t.Errorf("the description does not say the wall refuses: %s", description)
	}
}

// TestPortsSchemaCoversTheArguments: a property the tool reads but the
// schema does not declare is refused by the harness before the tool runs.
func TestPortsSchemaCoversTheArguments(t *testing.T) {
	tl := &PortsTool{}
	schema := tl.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"host", "ports", "top", "timeout", "concurrency", "max_ports"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("the schema does not declare %q", name)
		}
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "host" {
		t.Errorf("required = %v, want [host]", required)
	}
	args := map[string]any{
		"host":        "example.com",
		"ports":       []any{float64(80)},
		"top":         float64(50),
		"timeout":     float64(3),
		"concurrency": float64(8),
		"max_ports":   float64(20),
	}
	if err := ValidateArguments(schema, args); err != nil {
		t.Errorf("the harness refuses the declared arguments: %v", err)
	}
}

// TestPortsTranslateResultPassesOutput: result translation is a pass
// through of the recorded output.
func TestPortsTranslateResultPassesOutput(t *testing.T) {
	tl := &PortsTool{}
	op := operation.Operation{Output: "ports: example.com - 2 ports attempted, 1 open, 1 closed"}
	if got := tl.TranslateResult(op); got != op.Output {
		t.Errorf("TranslateResult = %q, want passthrough", got)
	}
}

// TestFormatPortsResultShowsTheStates: the three states have to survive into
// the output, because a filtered port is not a closed one.
func TestFormatPortsResultShowsTheStates(t *testing.T) {
	result := scan.PortsResult{
		Ports: []scan.PortResult{
			{Port: 443, State: "open", Service: "https"},
			{Port: 80, State: "closed", Service: "http"},
			{Port: 8080, State: "filtered", Service: "http-proxy", Error: "i/o timeout"},
		},
		Attempted: 3,
		Open:      1,
		Closed:    1,
		Filtered:  1,
	}
	out := formatPortsResult(result, "example.com")
	for _, want := range []string{"443 open https", "80 closed http", "8080 filtered", "1 open, 1 closed, 1 filtered"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
	if len(out) > 2000 {
		t.Errorf("the output is %d bytes for three ports, want it bounded", len(out))
	}
}

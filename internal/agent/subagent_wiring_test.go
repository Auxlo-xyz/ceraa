package agent

import (
	"testing"

	"ceraa/harness/tool"
	"ceraa/internal/config"
)

// A registered tool that cannot do anything is the failure this feature had in
// the CLI: the registry builds the task tool with no factory, and nothing in the
// CLI attached one, so every task call failed with "no factory is attached" --
// a tool the model can see and use that cannot do anything. Assert the wiring
// itself, not a whole run, so the check cannot fail for an unrelated reason.
func TestCLIWiresASubagentFactoryIntoItsRegistry(t *testing.T) {
	registry := tool.DefaultRegistryForUser("u", nil)
	task, ok := registry.Get("task")
	if !ok {
		t.Fatal("the task tool is not registered")
	}
	wired, ok := task.(*tool.TaskTool)
	if !ok {
		t.Fatalf("the task tool is %T, want *tool.TaskTool", task)
	}
	if wired.Factory != nil {
		t.Fatal("a fresh registry already has a factory; the tool is meant to be inert until a run attaches one")
	}
	attachSubagents(registry, &config.Config{}, "u", config.LLMProvider{}, nil, nil, "", nil, nil)
	if wired.Factory == nil {
		t.Error("attachSubagents left the CLI's task tool without a factory, so every task call would fail")
	}
}

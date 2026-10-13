package runtime

import (
	"testing"

	"ceraa/harness/coordinator"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
	"ceraa/harness/tool"
)

func TestNewAssemblesSharedCoreDependencies(t *testing.T) {
	store, err := sessionstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	rt := New(coordinator.RunnerConfig{SessionID: "runtime", Model: llm.DefaultConfig()}, store, registry)
	if rt.Sessions != store || rt.Tools != registry {
		t.Fatal("runtime did not retain supplied dependencies")
	}
	if rt.Coordinator == nil || rt.Operations == nil {
		t.Fatal("runtime did not assemble coordinator and operation manager")
	}
	if rt.Operations != rt.Coordinator.Operations() {
		t.Fatal("runtime exposed a different operation manager than its coordinator")
	}
}

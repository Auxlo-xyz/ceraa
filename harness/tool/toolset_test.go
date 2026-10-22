package tool

import "testing"

// TestTheFullSetIsTheDefault pins the decision: the full catalogue is the
// edge, and the switch is for the sessions that want less of it.
func TestTheFullSetIsTheDefault(t *testing.T) {
	t.Setenv(ToolsetEnv, "")
	if got := Toolset(); got != ToolsetFull {
		t.Errorf("an unset toolset is %q, want the full set", got)
	}
	t.Setenv(ToolsetEnv, "nonsense")
	if got := Toolset(); got != ToolsetFull {
		t.Errorf("an unrecognised toolset is %q, want the full set", got)
	}
	t.Setenv(ToolsetEnv, "core")
	if got := Toolset(); got != ToolsetCore {
		t.Errorf("an explicit core toolset is %q", got)
	}
	t.Setenv(ToolsetEnv, "full")
	if got := Toolset(); got != ToolsetFull {
		t.Errorf("an explicit full toolset is %q", got)
	}
}

// TestTheSpecialistsAreOfferedByDefault is the other half: the specialists
// are in the catalogue unless the operator asks for the smaller set.
func TestTheSpecialistsAreOfferedByDefault(t *testing.T) {
	t.Setenv(ToolsetEnv, "")
	offered := map[string]bool{}
	for _, item := range DefaultRegistry().Offered() {
		offered[item.Name()] = true
	}
	for _, name := range []string{"ports", "enum", "ldap", "kerb"} {
		if !offered[name] {
			t.Errorf("%s is not offered by default", name)
		}
	}

	t.Setenv(ToolsetEnv, "core")
	core := map[string]bool{}
	for _, item := range DefaultRegistry().Offered() {
		core[item.Name()] = true
	}
	for _, name := range []string{"ports", "enum", "ldap", "kerb"} {
		if core[name] {
			t.Errorf("%s is offered in the core set", name)
		}
	}
	// A hidden tool is still executable: the registry resolves it by name
	// whether or not it is offered.
	reg := DefaultRegistry()
	if _, ok := reg.Get("ldap"); !ok {
		t.Error("the directory tool is not executable in the core set")
	}
}

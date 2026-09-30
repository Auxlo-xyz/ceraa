package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceRootEnvOverridesConfig(t *testing.T) {
	// An environment override is a deliberate per-invocation setting, so it has
	// to beat the config file rather than the other way round. Getting this
	// backwards means an operator pinning a mounted volume is silently ignored.
	t.Setenv(WorkspaceRootEnv, "/mnt/agent")
	cfg := &Config{WorkspaceRoot: "/from/config"}
	if got := cfg.WorkspaceRootPath(); got != "/mnt/agent" {
		t.Errorf("env override ignored: got %q, want /mnt/agent", got)
	}
}

func TestWorkspaceRootEnvEmptyFallsThroughToConfig(t *testing.T) {
	// An empty variable is not a setting. Treating it as one would put the
	// workspace at the current directory.
	t.Setenv(WorkspaceRootEnv, "   ")
	cfg := &Config{WorkspaceRoot: "/from/config"}
	if got := cfg.WorkspaceRootPath(); got != "/from/config" {
		t.Errorf("blank override was honoured: got %q, want /from/config", got)
	}
}

func TestWorkspaceRootIsAbsolute(t *testing.T) {
	t.Setenv(WorkspaceRootEnv, "")
	cfg := &Config{WorkspaceRoot: "relative/place"}
	got := cfg.WorkspaceRootPath()
	if !filepath.IsAbs(got) {
		t.Errorf("workspace root %q is not absolute; a relative root resolves against "+
			"whatever the process happens to be started in", got)
	}
}

// A workspace the agent cannot write to makes every relative path it resolves
// fail against a directory that does not exist. The symptom is the agent
// reporting files it can plainly see, so this has to be decided before the first
// command rather than after.
func TestCreatableRejectsAnUnwritableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, so an unwritable parent is still writable")
	}
	parent := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(parent, "workspace")
	if creatable(candidate) {
		t.Error("a directory inside a read-only parent was reported as creatable")
	}
}

func TestCreatableAcceptsAWritableParent(t *testing.T) {
	parent := t.TempDir()
	if !creatable(filepath.Join(parent, "workspace")) {
		t.Error("a missing directory inside a writable parent was reported as unusable; " +
			"every first run creates the workspace, so this would reject fresh installs")
	}
}

func TestCreatableAcceptsAnExistingWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if !creatable(dir) {
		t.Error("an existing writable directory was reported as unusable")
	}
}

// The temp-directory guard exists because a Go test binary under /tmp resolves to
// a workspace path that stops existing the moment the build ends. It must still
// hold, and the fallback must be a real path rather than a relative one.
func TestWorkspaceRootNeverSitsUnderTheTempDirectory(t *testing.T) {
	t.Setenv(WorkspaceRootEnv, "")
	temp := filepath.Clean(os.TempDir())
	got := (&Config{}).WorkspaceRootPath()
	if underTempDir(got) {
		t.Errorf("workspace root %q is inside the temp directory %q; it will not survive "+
			"a restart", got, temp)
	}
}

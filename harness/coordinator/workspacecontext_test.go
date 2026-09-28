package coordinator

import (
	"strings"
	"testing"
)

// The model is not told anywhere in prompt/system.txt where its commands run, and
// it guesses. A guess that looks plausible is the worst kind: asked to review a
// project, MiniMax reached for /home/assistant/auxlo-neo/… because that home
// directory exists, and the project was at /workspace/auxlo-neo. Every answer was
// "no such file or directory".
func TestTheModelIsToldWhereItsCommandsRun(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt: "you are a coding agent",
		WorkDir:      "/workspace/auxlo-neo",
	}}
	got := r.workspaceContext()
	if got == "" {
		t.Fatal("no workspace context; the model has no idea where relative paths resolve")
	}
	if !strings.Contains(got, "/workspace/auxlo-neo") {
		t.Errorf("the workspace block does not name the directory: %s", got)
	}
	// Naming it is not the whole fix. The model has a strong prior that files
	// live under $HOME, so the block has to say so.
	for _, want := range []string{"relative", "absolute", "home"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("the workspace block never mentions %q: %s", want, got)
		}
	}
}

// It has to reach the assembled prompt, not just exist as a helper.
func TestTheWorkspaceBlockIsInTheAssembledPrompt(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt: "you are a coding agent",
		WorkDir:      "/srv/project",
	}}
	if !strings.Contains(r.systemPrompt(), "/srv/project") {
		t.Errorf("the assembled prompt omits the workspace:\n%s", r.systemPrompt())
	}
}

// With no workdir there is nothing true to say, and inventing a directory would be
// worse than silence.
func TestNoWorkDirMeansNoWorkspaceBlock(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{SystemPrompt: "you are a coding agent"}}
	if got := r.workspaceContext(); got != "" {
		t.Errorf("workspaceContext = %q, want empty", got)
	}
	if strings.Contains(r.systemPrompt(), "<workspace>") {
		t.Error("the assembled prompt has a workspace block with no workdir configured")
	}
}

// It sits after the static prompt so the cached prefix survives it, and like the
// date it is not counted as stable.
func TestTheWorkspaceBlockIsNotCountedAsStable(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt: "you are a coding agent",
		WorkDir:      "/srv/project",
	}}
	parts, stable := r.systemPromptParts()
	if stable > len(parts) {
		t.Fatalf("stable count %d exceeds %d parts", stable, len(parts))
	}
	for _, part := range parts[:stable] {
		if strings.Contains(part, "<workspace>") {
			t.Error("the workspace block is inside the cached prefix; switching project would invalidate the whole prompt")
		}
	}
}

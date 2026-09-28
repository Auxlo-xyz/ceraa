package coordinator

import (
	"strings"
	"testing"
)

// The model is not told anywhere in prompt/system.txt where its commands run, and
// it guesses. A guess that looks plausible is the worst kind: asked to review a
// project, MiniMax reached for /home/assistant/auxlo-neo/… because that home
// directory exists, and the project was at /srv/projects/betas/atlas. Every answer was
// "no such file or directory".
func TestTheModelIsToldWhereItsCommandsRun(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt: "you are a coding agent",
		WorkDir:      "/srv/projects/betas/atlas",
	}}
	got := r.workspaceContext()
	if got == "" {
		t.Fatal("no workspace context; the model has no idea where relative paths resolve")
	}
	if !strings.Contains(got, "/srv/projects/betas/atlas") {
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
		WorkDir:      "/srv/projects/beta",
	}}
	if !strings.Contains(r.systemPrompt(), "/srv/projects/beta") {
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
		WorkDir:      "/srv/projects/beta",
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

// More than one user will share a deployment, and each may have selected a
// different project. The block has to follow the session's own directory rather
// than any one repository, or the second user is told about the first one's files.
func TestTheWorkspaceBlockFollowsEachSessionsOwnProject(t *testing.T) {
	users := map[string]string{
		"atlas":              "/srv/projects/atlas",
		"boreal":             "/srv/projects/boreal",
		"a path with spaces": "/srv/projects/team two/relay",
	}
	for user, dir := range users {
		r := &Runner{cfg: RunnerConfig{SystemPrompt: "p", WorkDir: dir}}
		got := r.workspaceContext()
		if !strings.Contains(got, dir) {
			t.Errorf("%s: block does not name its own project %q: %s", user, dir, got)
		}
		for other, otherDir := range users {
			if other != user && strings.Contains(got, otherDir) {
				t.Errorf("%s: block leaks %s's project %q", user, other, otherDir)
			}
		}
	}

	// The same runner pointed somewhere else must report the new directory, so a
	// project switch cannot leave the previous one in the prompt.
	r := &Runner{cfg: RunnerConfig{SystemPrompt: "p", WorkDir: "/srv/projects/atlas"}}
	before := r.systemPrompt()
	r.cfg.WorkDir = "/srv/projects/boreal"
	after := r.systemPrompt()
	if strings.Contains(after, "atlas") {
		t.Errorf("after switching project the prompt still names the old one:\n%s", after)
	}
	if before == after {
		t.Error("switching project did not change the prompt at all")
	}
}

package coordinator

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
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

// It changed from sitting past the cache seam to sitting inside the cached
// prefix, and that is the point: past the seam is not "cheap", it is "never
// cached", so an 82-token block was being re-read on every request while
// changing only when the user switches project.
func TestTheWorkspaceBlockIsInsideTheCachedPrefix(t *testing.T) {
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt: "you are a coding agent",
		WorkDir:      "/srv/projects/beta",
	}}
	parts, stable := r.systemPromptParts()
	if stable > len(parts) {
		t.Fatalf("stable count %d exceeds %d parts", stable, len(parts))
	}
	found := false
	for _, part := range parts[:stable] {
		if strings.Contains(part, "<workspace>") {
			found = true
		}
	}
	if !found {
		t.Error("the workspace block is past the cache seam, so it is re-read on every request")
	}
	if r.systemCacheSeam() < len("/srv/projects/beta") {
		t.Errorf("the seam does not reach the workspace directory: %d", r.systemCacheSeam())
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

// The seam is a hard boundary, so what is on each side of it is worth pinning.
// Everything rare belongs inside and everything that changes mid-turn belongs
// outside, or the caching either does nothing or thrashes.
func TestTheCacheSeamSplitsStableFromVolatile(t *testing.T) {
	memory := strings.Repeat("remembered. ", 300)
	r := &Runner{cfg: RunnerConfig{
		SystemPrompt:      "you are a coding agent",
		SkillContext:      "a skill",
		WorkDir:           "/srv/projects/beta",
		Model:             llm.ProviderConfig{Model: "test-model", BaseURL: "host.example"},
		MemoryContextFunc: func() (string, error) { return memory, nil },
	}}
	parts, stable := r.systemPromptParts()
	if stable >= len(parts) {
		t.Fatalf("stable %d of %d: nothing is volatile, so nothing is cached per part", stable, len(parts))
	}

	cached, uncached := strings.Join(parts[:stable], "\n\n"), strings.Join(parts[stable:], "\n\n")
	// The preamble is asserted alongside the catalogue on purpose. They are one
	// part, so the instruction to read a skill and the list of skills it applies
	// to cannot be cached apart: a prefix that carried the list but not the
	// instruction would advertise tools the model has never been told about.
	for _, want := range []string{"<workspace>", "skill_use", "a skill", "/srv/projects/beta"} {
		if !strings.Contains(cached, want) {
			t.Errorf("%s is not in the cached prefix", want)
		}
	}
	// The date and memory change daily and mid-turn respectively. Caching them
	// would re-read the whole prefix whenever the agent writes a memory.
	for _, unwanted := range []string{"<current-date>", "remembered."} {
		if strings.Contains(cached, unwanted) {
			t.Errorf("%s is inside the cached prefix", unwanted)
		}
	}
	if !strings.Contains(uncached, "<current-date>") || !strings.Contains(uncached, "remembered.") {
		t.Errorf("the volatile tail is missing a part:\n%s", uncached)
	}
}

// An absent workspace or skill must not be counted as a stable part, or the seam
// would point into the wrong section and cache a prefix that has since shifted.
func TestAnAbsentBlockIsNotCountedInTheSeam(t *testing.T) {
	full := &Runner{cfg: RunnerConfig{
		SystemPrompt: "p", SkillContext: "s", WorkDir: "/w",
		Model: llm.ProviderConfig{Model: "test-model", BaseURL: "host.example"},
	}}
	bare := &Runner{cfg: RunnerConfig{
		SystemPrompt: "p", Model: llm.ProviderConfig{Model: "test-model", BaseURL: "host.example"},
	}}
	fullSeam, bareSeam := full.systemCacheSeam(), bare.systemCacheSeam()
	if fullSeam <= bareSeam {
		t.Errorf("adding a workspace and a skill did not move the seam: %d vs %d", fullSeam, bareSeam)
	}
	// The bare prompt still caches the identity and the vendored prompt.
	if bareSeam < len("p") {
		t.Errorf("bare seam %d does not cover the vendored prompt", bareSeam)
	}
	if strings.Contains(bare.systemPrompt(), "<workspace>") {
		t.Error("a workspace block appeared with no workdir configured")
	}
}

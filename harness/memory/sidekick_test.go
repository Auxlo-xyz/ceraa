package memory

import (
	"strings"
	"testing"
	"time"
)

func brief(content, applies, why string, seen int, ageDays float64, now time.Time) Entry {
	return Entry{
		Content: content, Applies: applies, Why: why, Seen: seen,
		SeenAt: now.Add(-time.Duration(ageDays*24) * time.Hour),
	}
}

// A memory with a path is delivered only when the work touches that path.
//
// This is the whole point of the trigger. A benchmark of four production memory
// systems found the dominant failure was injecting records that did not bear on
// the work - at a solver already solving the task, an injected record can only
// lose. Relocating memory into tool output solves nothing if it then fires on
// every call.
func TestTargetedMemoryOnlyMatchesItsPath(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		brief("the build needs JAVA_HOME set", "/srv/api", "gradle picked the wrong jdk", 6, 1, now),
	}

	if got := Select(entries, []string{"/srv/api/build.gradle"}, now, 4); len(got) != 1 {
		t.Errorf("a path under the memory's directory did not match: %#v", got)
	}
	if got := Select(entries, []string{"/srv/web/package.json"}, now, 4); len(got) != 0 {
		t.Errorf("an unrelated path was annotated: %#v", got)
	}
	if got := Select(entries, []string{""}, now, 4); len(got) != 0 {
		t.Errorf("an operation with no path was annotated: %#v", got)
	}
}

// A glob has to work across path separators, which filepath.Match does not do on
// its own, and a bare filename has to reach a full path.
func TestGlobsAndBareFilenamesMatch(t *testing.T) {
	now := time.Now()
	cases := []struct {
		applies string
		target  string
		want    bool
	}{
		{"*.go", "/srv/app/main.go", true},
		{"*.go", "/srv/app/main.rs", false},
		{"Makefile", "/srv/app/Makefile", true},
		{"Makefile", "/srv/app/other/Makefile", true},
		{"src/*", "/srv/app/src/main.go", true},
		{"/srv/api/", "/srv/api/main.go", true},
		{"/srv/api/", "/srv/apix/main.go", false},
	}
	for _, tc := range cases {
		entries := []Entry{brief("x", tc.applies, "", 6, 1, now)}
		got := len(Select(entries, []string{tc.target}, now, 4)) == 1
		if got != tc.want {
			t.Errorf("applies=%q target=%q matched=%v, want %v", tc.applies, tc.target, got, tc.want)
		}
	}
}

// A general memory has no path, so it must not be delivered per-operation. It is
// delivered once per run, or it would be written into the transcript once per
// tool call and every copy would sit in the context for the rest of the session.
func TestGeneralMemoriesAreSelectedSeparately(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		brief("prefers dark mode", "", "", 6, 1, now),
		brief("JAVA_HOME is set here", "/srv/api", "", 6, 1, now),
	}

	if got := Select(entries, []string{"/srv/api/main.go"}, now, 4); len(got) != 1 || got[0].Content != "JAVA_HOME is set here" {
		t.Errorf("per-operation select returned the wrong set: %#v", got)
	}
	general := SelectGeneral(entries, now, 3)
	if len(general) != 1 || general[0].Content != "prefers dark mode" {
		t.Errorf("general select returned %#v, want only the pathless memory", general)
	}
}

// The gate is above the render floor, because a tool result is a worse place to
// spend attention than a prompt line: it competes with the output the agent
// actually asked for.
func TestTheGateHoldsBackWeakMemories(t *testing.T) {
	now := time.Now()
	weak := []Entry{brief("barely confirmed", "/srv/api", "", 1, 40, now)}
	if got := Select(weak, []string{"/srv/api/main.go"}, now, 4); len(got) != 0 {
		t.Errorf("a below-gate memory was injected: %#v", got)
	}
	strong := []Entry{brief("well confirmed", "/srv/api", "", 8, 1, now)}
	if got := Select(strong, []string{"/srv/api/main.go"}, now, 4); len(got) != 1 {
		t.Errorf("a strong memory was held back: %#v", got)
	}
}

// Ordering has to be by strength, so the strongest memory survives the limit.
func TestSelectionIsOrderedByWeightAndBounded(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		brief("weak", "/srv/api", "", 2, 5, now),
		brief("strong", "/srv/api", "", 20, 1, now),
		brief("middle", "/srv/api", "", 8, 1, now),
	}
	got := Select(entries, []string{"/srv/api/main.go"}, now, 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Content != "strong" || got[1].Content != "middle" {
		t.Errorf("order = %q, %q; want strongest first", got[0].Content, got[1].Content)
	}
}

// The block has to announce itself, twice over.
//
// It must be marked so the model reads it as guidance rather than as something
// the tool returned, since placing instruction next to the work it concerns is the
// entire mechanism. And it must be findable, so the agent can confirm or
// contradict something it has just been shown.
func TestTheBlockIsMarkedAndBounded(t *testing.T) {
	note := Annotate([]string{"/srv/api/main.go"},
		[]Brief{{Content: "JAVA_HOME is set here", Why: "gradle picked the wrong jdk", Score: 0.9, Seen: 6}},
		[]Brief{{Content: "prefers dark mode", Score: 0.8, Seen: 5}})

	if !strings.Contains(note, "<sidekick") || !strings.Contains(note, "</sidekick>") {
		t.Fatalf("the block is not marked: %q", note)
	}
	if !strings.Contains(note, "/srv/api/main.go") {
		t.Errorf("the block does not say what it is about: %q", note)
	}
	if !strings.Contains(note, "JAVA_HOME is set here") || !strings.Contains(note, "prefers dark mode") {
		t.Errorf("the block dropped a memory: %q", note)
	}
	if !strings.Contains(note, "gradle picked the wrong jdk") {
		t.Errorf("the rationale was dropped, which is the field the benchmark says matters: %q", note)
	}
	if !strings.Contains(note, "not new instructions") {
		t.Errorf("the block does not tell the model these are established facts: %q", note)
	}
	if strings.Contains(Annotate([]string{"/x"}, nil, nil), "sidekick") {
		t.Error("an empty selection still produced a block")
	}
}

// A rationale is bounded at render time, not trusted to be short, or it becomes a
// way to put a paragraph on every matching result.
func TestTheRationaleIsBounded(t *testing.T) {
	long := strings.Repeat("word ", 500)
	note := Annotate([]string{"/srv/api"}, []Brief{{Content: "x", Why: long, Score: 0.9}}, nil)
	if len(note) > sidekickWhyBytes+400 {
		t.Errorf("the note is %d bytes; the rationale was not bounded", len(note))
	}
}

// A missing store must read as "nothing to say", and an unreadable one must not
// be reported as an empty store. The caller distinguishes the two.
func TestParseDeliveryDefaultsToCache(t *testing.T) {
	// Cache changed from "an override you had to ask for" to the default on the
	// same day the extractor moved memory writes to compaction. Putting the
	// block in the prefix only thrashes the cache while writes are frequent, so
	// the safe default before extraction existed was "prompt". Now writes are
	// rare, which is what makes the prefix the cheap default.
	if got := ParseDelivery(""); got != DeliveryCached {
		t.Errorf("unset delivery = %q, want cached", got)
	}
	if got := ParseDelivery("nonsense"); got != DeliveryCached {
		t.Errorf("a misspelt value = %q, want cached rather than a silent change of where memory goes", got)
	}
	for _, spelling := range []string{"sidekick", "SIDEKICK", " tool ", "tool_output"} {
		if got := ParseDelivery(spelling); got != DeliverySidekick {
			t.Errorf("%q = %q, want sidekick", spelling, got)
		}
	}
	for _, spelling := range []string{"prompt", "tail"} {
		if got := ParseDelivery(spelling); got != DeliveryPrompt {
			t.Errorf("%q = %q, want prompt", spelling, got)
		}
	}
	for _, spelling := range []string{"cached", "cache", "prefix"} {
		if got := ParseDelivery(spelling); got != DeliveryCached {
			t.Errorf("%q = %q, want cached", spelling, got)
		}
	}
}

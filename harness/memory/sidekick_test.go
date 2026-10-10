package memory

import (
	"fmt"
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

	if got := Select(entries, []string{"/srv/api/build.gradle"}, now, 4, nil); len(got) != 1 {
		t.Errorf("a path under the memory's directory did not match: %#v", got)
	}
	if got := Select(entries, []string{"/srv/web/package.json"}, now, 4, nil); len(got) != 0 {
		t.Errorf("an unrelated path was annotated: %#v", got)
	}
	if got := Select(entries, []string{""}, now, 4, nil); len(got) != 0 {
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
		got := len(Select(entries, []string{tc.target}, now, 4, nil)) == 1
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

	if got := Select(entries, []string{"/srv/api/main.go"}, now, 4, nil); len(got) != 1 || got[0].Content != "JAVA_HOME is set here" {
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
	if got := Select(weak, []string{"/srv/api/main.go"}, now, 4, nil); len(got) != 0 {
		t.Errorf("a below-gate memory was injected: %#v", got)
	}
	strong := []Entry{brief("well confirmed", "/srv/api", "", 8, 1, now)}
	if got := Select(strong, []string{"/srv/api/main.go"}, now, 4, nil); len(got) != 1 {
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
	got := Select(entries, []string{"/srv/api/main.go"}, now, 2, nil)
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

// Only the strongest couple keep their rationale. A fourth line of explanation per
// memory is the part of the note that stops being read, and it is paid for in the
// most expensive place in the request.
func TestOnlyTheTopMemoriesKeepTheirRationale(t *testing.T) {
	targeted := make([]Brief, 0, 4)
	for i, why := range []string{"because one", "because two", "because three", "because four"} {
		targeted = append(targeted, Brief{
			Content: fmt.Sprintf("memory %d", i), Why: why, Score: 0.9 - float64(i)*0.1,
		})
	}
	note := Annotate([]string{"/srv/api"}, targeted, nil)
	for i, why := range []string{"because one", "because two"} {
		if !strings.Contains(note, why) {
			t.Errorf("the %d strongest memory lost its rationale: %q", i+1, note)
		}
	}
	for _, why := range []string{"because three", "because four"} {
		if strings.Contains(note, why) {
			t.Errorf("a rationale past the top %d was rendered: %q", sidekickWhyTopN, note)
		}
	}
}

// The flood is the case the ceiling exists for: a directory that happens to match
// forty memories must not turn one file read into a page. The bound has to hold,
// and it has to be visible, because a list that stops without saying so reads as
// the whole set.
func TestAFloodOfMatchingMemoriesIsCappedAndSaysSo(t *testing.T) {
	targeted := make([]Brief, 0, 40)
	for i := 0; i < 40; i++ {
		targeted = append(targeted, Brief{
			ID:      fmt.Sprintf("id%02d", i),
			Content: fmt.Sprintf("memory number %d about this path", i),
			Why:     strings.Repeat("reason ", 60),
			Score:   0.9 - float64(i)*0.01,
		})
	}
	note := Annotate([]string{"/srv/api/main.go"}, targeted, nil)
	if len(note) > SidekickBlockBytes {
		t.Errorf("the note is %d bytes, over the %d ceiling", len(note), SidekickBlockBytes)
	}
	if !strings.Contains(note, fmt.Sprint(SidekickBlockBytes)) {
		t.Errorf("the note does not name its own ceiling: %q", note)
	}
	if !strings.Contains(note, "not shown") {
		t.Errorf("a truncated note does not admit what it left out: %q", note)
	}
	if !strings.HasSuffix(note, "</sidekick>") {
		t.Errorf("the note was cut inside its own markup: %q", note)
	}
	// The strongest survive: this is a truncation of the tail, not of the ranking.
	for _, keep := range []string{"memory number 0", "memory number 1"} {
		if !strings.Contains(note, keep) {
			t.Errorf("capping dropped %q rather than the weakest memories: %q", keep, note)
		}
	}
	if strings.Contains(note, "memory number 39") {
		t.Errorf("the weakest memory was kept: %q", note)
	}
}

// One memory always fits whole. Only the tail is ever dropped, so the strongest
// claim on a path cannot be the one that gets cut in half.
func TestASingleMemoryIsNeverCutDownToFit(t *testing.T) {
	content := strings.Repeat("word ", 60)
	why := strings.Repeat("reason ", 60)
	note := Annotate([]string{"/srv/api/main.go"}, []Brief{{Content: content, Why: why, Score: 0.9}}, nil)
	if !strings.Contains(note, "word") || !strings.Contains(note, "reason") {
		t.Fatalf("the only memory was dropped or mangled: %q", note)
	}
	if !strings.HasSuffix(note, "</sidekick>") {
		t.Errorf("the note was cut inside its own markup: %q", note)
	}
	if len(note) > SidekickBlockBytes {
		t.Errorf("the note is %d bytes, over the %d ceiling", len(note), SidekickBlockBytes)
	}
}

// The dedup key is the id, and it has to be: the rendered line is truncated, so two
// memories that agree past the truncation point render identically and would merge
// silently - the second suppressed as a repeat of the first while saying something
// else entirely.
func TestDeliveredMemoriesAreMatchedByIDNotByRenderedText(t *testing.T) {
	now := time.Now()
	shared := strings.Repeat("shared prefix ", 40)
	first := brief(shared+"the backend pins jdk 21", "/srv/api", "", 6, 1, now)
	first.ID = "id-first"
	second := brief(shared+"the backend pins jdk 21 for gradle", "/srv/api", "", 6, 1, now)
	second.ID = "id-second"

	// The two render as the same line, which is exactly the collision to avoid.
	if oneLine(Brief{Content: first.Content}) != oneLine(Brief{Content: second.Content}) {
		t.Fatal("the fixture does not collide on rendered text, so it proves nothing")
	}
	if len(oneLine(Brief{Content: first.Content})) >= len(first.Content) {
		t.Fatal("the fixture is not long enough to be truncated, so it proves nothing")
	}

	seen := map[string]struct{}{"id-first": {}}
	got := Select([]Entry{first, second}, []string{"/srv/api"}, now, 4, func(id string) bool {
		_, ok := seen[id]
		return ok
	})
	if len(got) != 1 || got[0].ID != "id-second" {
		t.Errorf("select returned %#v, want only the undelivered memory", got)
	}
}

// An entry with no id - a hand-written memory.json, a fixture - still has to
// dedup, and it has to do so on the wording the store itself dedups on rather than
// on the rendered line.
func TestAnEntryWithNoIDStillDedups(t *testing.T) {
	now := time.Now()
	entry := brief("JAVA_HOME is set here", "/srv/api", "", 6, 1, now)
	if entry.ID != "" {
		t.Fatal("the fixture is supposed to have no id")
	}
	id := DeliveryID(entry)
	if id == "" || !strings.Contains(id, "JAVA_HOME") {
		t.Errorf("DeliveryID = %q", id)
	}
	if DeliveryID(Entry{ID: "abc123", Content: "anything"}) != "abc123" {
		t.Error("a stored entry's id was not used verbatim")
	}
	if DeliveryID(entry) == DeliveryID(brief("a different claim", "/srv/api", "", 6, 1, now)) {
		t.Error("two different id-less memories share a delivery key")
	}
}

// Excluded before the limit, not after. Filtering afterwards lets four memories
// that were all delivered earlier hide a fifth that never was, and the weaker
// memory is then never delivered at all - dedup that silently retires the tail.
func TestExclusionHappensBeforeTheLimit(t *testing.T) {
	now := time.Now()
	entries := []Entry{
		brief("first", "/srv/api", "", 20, 1, now),
		brief("second", "/srv/api", "", 18, 1, now),
		brief("third", "/srv/api", "", 16, 1, now),
	}
	got := Select(entries, []string{"/srv/api"}, now, 1, func(string) bool { return true })
	if len(got) != 0 {
		t.Errorf("an all-excluded selection returned %#v", got)
	}
	delivered := map[string]bool{DeliveryID(entries[0]): true, DeliveryID(entries[1]): true}
	got = Select(entries, []string{"/srv/api"}, now, 1, func(id string) bool { return delivered[id] })
	if len(got) != 1 || got[0].Content != "third" {
		t.Errorf("the undelivered memory was hidden by two delivered ones: %#v", got)
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

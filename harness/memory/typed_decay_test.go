package memory

import (
	"math"
	"testing"
	"time"
)

// The spread is the point. A profile and a request are not the same kind of claim
// and must not share a lifetime: one is a standing fact about a person and the other
// is something asked for once.
//
// Ordering is by how often the underlying claim stops being true, not by importance.
// That distinction is what keeps a goal from being treated as durable and a
// preference from being treated as disposable.
func TestEveryTypeHasItsOwnLifetime(t *testing.T) {
	halfLives := make(map[MemoryType]float64, len(halfLifeDaysByType))
	for memoryType, halfLife := range halfLifeDaysByType {
		halfLives[memoryType] = halfLife
	}
	if len(halfLives) < 8 {
		t.Fatalf("only %d types have a lifetime: %v", len(halfLives), halfLives)
	}
	// The longest must outlive the shortest by more than an order of magnitude, or
	// "typed decay" is one constant with different names on it.
	longest, shortest := 0.0, math.Inf(1)
	var longestName, shortestName MemoryType
	for name, halfLife := range halfLives {
		if halfLife > longest {
			longest, longestName = halfLife, name
		}
		if halfLife < shortest {
			shortest, shortestName = halfLife, name
		}
	}
	if longest < 10*shortest {
		t.Errorf("%s lasts %.0f days and %s lasts %.0f: the spread is too narrow to matter",
			longestName, longest, shortestName, shortest)
	}
	// An untyped entry keeps the old single constant, so upgrading changes nothing
	// about how a store in the field decays.
	if got := halfLifeDays(Entry{}); got != memoryHalfLifeDays {
		t.Errorf("an untyped entry half-lives in %.0f days, want the old %.0f", got, memoryHalfLifeDays)
	}
	if got := halfLifeDays(Entry{Type: "not-a-type"}); got != memoryHalfLifeDays {
		t.Errorf("an unknown type half-lives in %.0f days, want the old %.0f", got, memoryHalfLifeDays)
	}
}

// Two memories asserted once, at the same moment, twenty years apart in how long
// they should survive. Under one shared half-life they would be scored identically,
// which is the whole fault this fixes.
func TestAPreferenceOutlivesARequestByTheMarginTypedDecayPromises(t *testing.T) {
	now := time.Now()
	old := now.Add(-400 * 24 * time.Hour)

	profile := score(Entry{Seen: 1, Veracity: Stated, Type: Profile, SeenAt: old}, now)
	preference := score(Entry{Seen: 1, Veracity: Stated, Type: Preference, SeenAt: old}, now)
	request := score(Entry{Seen: 1, Veracity: Stated, Type: Request, SeenAt: old}, now)

	if profile <= preference {
		t.Errorf("a profile (%.4f) does not outlive a preference (%.4f) after 400 days", profile, preference)
	}
	if preference <= request {
		t.Errorf("a preference (%.4f) does not outlive a request (%.4f) after 400 days", preference, request)
	}
	// The request should be gone by now. This is the behaviour a single constant
	// got wrong in the direction that matters: it kept the disposable thing.
	if request >= memoryScoreFloor {
		t.Errorf("a request asserted 400 days ago still scores %.4f, above the floor %.4f",
			request, memoryScoreFloor)
	}
	// And the profile should be comfortably readable, because it is still true.
	if profile < memoryScoreFloor {
		t.Errorf("a profile asserted 400 days ago scores %.4f, below the floor: the durable thing faded", profile)
	}
}

// Days until an entry falls below the render floor, for a type.
func daysUntilFloorFor(weight, halfLife float64) float64 {
	if weight <= memoryScoreFloor {
		return 0
	}
	return math.Log(weight/memoryScoreFloor) / math.Ln2 * halfLife
}

// The two halves of the fix have to compose: corroboration buys longevity, and typed
// decay sets how much longevity there is to buy. A heavily confirmed request must
// still expire, or corroboration has become permanent.
func TestCorroborationBuysLongevityWithoutMakingAnyTypePermanent(t *testing.T) {
	for _, memoryType := range []MemoryType{Profile, Preference, Request} {
		halfLife := halfLifeDays(Entry{Type: memoryType})
		once := daysUntilFloorFor(corroborationWeight(1), halfLife)
		many := daysUntilFloorFor(corroborationWeight(20), halfLife)
		if many <= once {
			t.Errorf("%s: asserted 20 times lasts %.0f days against %.0f for once", memoryType, many, once)
		}
		if many > 4*halfLife {
			t.Errorf("%s: a corroborated memory lasts %.0f days, over 4 half-lives: decay cannot reach it",
				memoryType, many)
		}
	}
	// And a corroborated request still expires well inside a year.
	requestMany := daysUntilFloorFor(corroborationWeight(20), halfLifeDays(Entry{Type: Request}))
	if requestMany > 365 {
		t.Errorf("a request asserted 20 times lasts %.0f days; it should still be forgotten", requestMany)
	}
}

// The type comes from the statement's own wording, because the extractor is asked
// for a preference every time and would oblige with a type for everything.
func TestTheClassifierReadsTheClaimFromItsWording(t *testing.T) {
	cases := []struct {
		content string
		want    MemoryType
	}{
		{"always run gofmt before committing", Instruction},
		{"never force push to main", Instruction},
		{"I am a backend engineer working in Go", Profile},
		{"the user is working on a payments integration", Profile},
		{"I want to ship the Mini App this month", Goal},
		{"we use Postgres because it is already deployed", Decision},
		{"that was a mistake, the release went out early", Error},
		{"do not use the vendored SDK", Constraint},
		{"just for now, skip the docs check", Request},
		{"the repo builds with go 1.27", Context},
		{"prefer small commits", Preference},
		{"", ""},
	}
	for _, tc := range cases {
		if got := classifyMemoryType(tc.content); got != tc.want {
			t.Errorf("classifyMemoryType(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}

// A rule phrased as a one-off keeps the long life. Getting this backwards would
// forget a standing preference, which is the failure that costs.
func TestARequestPhrasedAsARuleKeepsTheLongLife(t *testing.T) {
	if got := classifyMemoryType("for this task, always run the gate before pushing"); got == Request {
		t.Error("a standing rule lost its lifetime to a one-off phrase")
	}
}

// An explicit type from a caller who knows beats the classifier. An unrecognised one
// falls through to inference rather than silently becoming the shortest life.
func TestAnExplicitTypeWinsAndAnUnknownOneIsInferred(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTyped("p", "s", "prefer small commits", "", "", Stated, Request); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTyped("p", "s", "always run gofmt", "", "", Stated, "prefrence"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	byContent := map[string]Entry{}
	for _, entry := range entries {
		byContent[entry.Content] = entry
	}
	if got := byContent["prefer small commits"].Type; got != Request {
		t.Errorf("an explicit type was overridden by the classifier: got %q, want %q", got, Request)
	}
	// A typo must not cost a standing rule the shortest life in the table.
	if got := byContent["always run gofmt"].Type; got != Instruction {
		t.Errorf("a typo'd explicit type gave %q, want the inferred %q", got, Instruction)
	}
}

// A repeat re-asserts the same statement, so it must not re-derive the type and
// silently change the lifetime of an established memory.
func TestARepeatNeverChangesAnEstablishedType(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const statement = "always run gofmt"
	for i := 0; i < 5; i++ {
		if _, err := store.AddTyped("p", "s", statement, "", "", Stated, Profile); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("store holds %d entries, want 1", len(entries))
	}
	if entries[0].Type != Profile {
		t.Errorf("type became %q after re-assertion, want the established %q", entries[0].Type, Profile)
	}
	if entries[0].Seen != 5 {
		t.Errorf("Seen = %d, want 5", entries[0].Seen)
	}
}

// The extractor is asked for the type, because the kind of claim is semantic and
// wording is not always enough. "we went with SQLite" is a decision to a reader and
// a preference to a pattern matcher, and the difference is a two-year lifetime
// against a three-year one.
func TestAModelNamedTypeIsUsedOverTheClassifier(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Reads as a preference to the classifier on wording alone.
	if _, err := store.AddTyped("p", "s", "we went with SQLite because it is already deployed", "", "", Stated, MemoryTypeFromName("decision")); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Type; got != Decision {
		t.Errorf("type = %q, want the model's %q", got, Decision)
	}
}

// A type the model invented is discarded, not guessed at. The failure this prevents
// is the quiet one: an unrecognised value assigned some other type would mislabel
// the memory's lifetime with nothing in the log to show for it.
func TestAnInventedTypeFallsBackToTheClassifier(t *testing.T) {
	for _, invented := range []string{"important", "fact", "note", "", "PREFERENCE!"} {
		if got := MemoryTypeFromName(invented); got != "" && got != Preference {
			t.Errorf("MemoryTypeFromName(%q) = %q, want it discarded or the one real alias", invented, got)
		}
	}
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTyped("p", "s", "never force push to main", "", "", Stated, MemoryTypeFromName("important")); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	// An invented type must not cost a standing rule the shortest life in the table.
	if got := entries[0].Type; got != Instruction {
		t.Errorf("type = %q after an invented value, want the inferred %q", got, Instruction)
	}
}

// The aliases a model will actually reach for, since a small free model asked for
// one word will use the nearest one it knows.
func TestTypeNamesAcceptTheSpellingsAModelReachesFor(t *testing.T) {
	cases := map[string]MemoryType{
		"rule": Instruction, "directive": Instruction, "instruction": Instruction,
		"preference": Preference, "identity": Profile, "about": Profile,
		"goal": Goal, "choice": Decision, "decision": Decision,
		"limit": Constraint, "boundary": Constraint, "constraint": Constraint,
		"task": Request, "request": Request, "project": Context, "context": Context,
		"mistake": Error, "lesson": Error, "error": Error,
		"observation":    Observation,
		"  PREFERENCE  ": Preference,
	}
	for name, want := range cases {
		if got := MemoryTypeFromName(name); got != want {
			t.Errorf("MemoryTypeFromName(%q) = %q, want %q", name, got, want)
		}
	}
}

// The prompt has to actually ask, or nothing above changes. It previously asked for
// four fields and described itself as a preference extractor, so no other type could
// ever be named.
func TestThePromptAsksForTheTypeItNowUses(t *testing.T) {
	for _, required := range []string{`"type"`, "instruction", "profile", "decision", "constraint"} {
		if !contains(promptBody(extractorPrompt), required) {
			t.Errorf("the extractor prompt no longer mentions %q, so the field it is parsed for is never produced", required)
		}
	}
	// The field list must not claim to be exactly four any more.
	if contains(extractorPrompt, "exactly these string fields") && !contains(extractorPrompt, `"type"`) {
		t.Error("the prompt pins a field list without the type field in it")
	}
}

func promptBody(prompt string) string { return prompt }
func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// The dashboard lists every memory across projects, newest first. The menu
// keeps the scoped List for its per-chat view; this is the global one, and it
// must not silently drop a project.
func TestListAllSeesEveryProjectNewestFirst(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTyped("alpha", "s1", "first memory", "", "", Stated, Preference); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTyped("beta", "s2", "second memory", "", "", Stated, Preference); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListAll = %d entries, want 2", len(entries))
	}
	if entries[0].Content != "second memory" || entries[1].Content != "first memory" {
		t.Errorf("ListAll is not newest-first: %+v", entries)
	}
}

// The decay table the dashboard renders comes from here, not from a copy
// quoted in the page. A type missing from this list is a lifetime the page
// cannot explain.
func TestTypeHalfLivesNamesEveryClassifiedType(t *testing.T) {
	seen := map[MemoryType]bool{}
	for _, entry := range TypeHalfLives() {
		seen[entry.Type] = true
		if entry.HalfLife <= 0 {
			t.Errorf("type %q has no positive half-life", entry.Type)
		}
	}
	for _, typ := range []MemoryType{Profile, Instruction, Preference, Decision, Constraint, Goal, Context, Error, Observation, Request} {
		if !seen[typ] {
			t.Errorf("type %q missing from the rendered decay table", typ)
		}
	}
}

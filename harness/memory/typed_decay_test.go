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

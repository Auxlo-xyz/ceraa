package memory

import (
	"math"
	"strings"
	"testing"
	"time"
)

// A repeat assertion is evidence, not a duplicate to discard.
//
// Add returned the existing entry unchanged when the content matched exactly, so
// re-stating a preference the agent already knew was worth precisely nothing. The
// store could not tell a preference mentioned once from one confirmed many times,
// which is the only thing that distinguishes a load-bearing preference from a
// passing remark.
func TestReassertingAMemoryCorroboratesIt(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Add("p", "s", "prefers dark mode")
	if err != nil {
		t.Fatal(err)
	}
	if first.Seen != 1 {
		t.Fatalf("a new memory has Seen = %d, want 1", first.Seen)
	}

	second, err := store.Add("p", "s", "prefers dark mode")
	if err != nil {
		t.Fatal(err)
	}
	if second.Seen != 2 {
		t.Errorf("after re-asserting, Seen = %d, want 2", second.Seen)
	}
	if !second.SeenAt.After(first.SeenAt) && !second.SeenAt.Equal(first.SeenAt) {
		t.Error("re-asserting did not refresh SeenAt, so decay would run from the first mention")
	}

	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("re-asserting produced %d entries, want 1", len(entries))
	}
}

// Confirm strengthens by id, without needing the wording reproduced.
//
// Add dedups on exact content, so a model asked to confirm something it read out
// of the rendered context has to match the stored bytes to hit the same entry.
// Restating with different punctuation would create a near-duplicate instead,
// which is exactly what this action exists to make impossible.
func TestConfirmStrengthensByIDWithoutTheWording(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Add("p", "s", "prefers dark mode")
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately different wording, which would have created a second entry.
	if err := store.Confirm("p", "s", entry.ID); err != nil {
		t.Fatal(err)
	}

	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("confirm created %d entries, want 1", len(entries))
	}
	if entries[0].Seen != 2 {
		t.Errorf("Seen = %d after confirm, want 2", entries[0].Seen)
	}
	if entries[0].Content != "prefers dark mode" {
		t.Errorf("confirm rewrote the content to %q", entries[0].Content)
	}
}

// Confirm is scoped. An id from another project or session must not be
// corroborable, or a guess could strengthen a memory the caller cannot see.
func TestConfirmIsScopedLikeEveryOtherWrite(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Add("project-a", "session-a", "prefers dark mode")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ project, session string }{
		{"project-b", "session-a"},
		{"project-a", "session-b"},
	} {
		if err := store.Confirm(tc.project, tc.session, entry.ID); err == nil {
			t.Errorf("confirm from %s/%s succeeded on another scope's memory", tc.project, tc.session)
		}
	}
	if err := store.Confirm("project-a", "session-a", "no-such-id"); err == nil {
		t.Error("confirm accepted an id that does not exist")
	}
}

// The decay floor is the whole point of the change: a memory that cannot expire
// is a memory that accumulates forever and eventually crowds out everything.
//
// This is the assertion that fails without score-ordered rendering.
func TestAFadedMemoryStopsBeingRendered(t *testing.T) {
	now := time.Now()
	fresh := Entry{Content: "fresh", Seen: 1, SeenAt: now}
	ancient := Entry{Content: "ancient", Seen: 1, SeenAt: now.Add(-400 * 24 * time.Hour)}

	if score(fresh, now) < memoryScoreFloor {
		t.Fatalf("a freshly asserted memory scores %v, below the floor %v", score(fresh, now), memoryScoreFloor)
	}
	if score(ancient, now) >= memoryScoreFloor {
		t.Fatalf("a 400-day-old memory scores %v, which is still above the floor %v",
			score(ancient, now), memoryScoreFloor)
	}

	rendered := RenderContext([]Entry{ancient, fresh}, 8000)
	if strings.Contains(rendered, "ancient") {
		t.Errorf("a faded memory is still rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "fresh") {
		t.Errorf("a fresh memory was dropped:\n%s", rendered)
	}
}

// Corroboration has to beat age, or a preference confirmed once a week is worth
// the same as one confirmed daily, which is the whole reason for a count at all.
func TestCorroborationOutranksAge(t *testing.T) {
	now := time.Now()
	// Both asserted on the same day, 20 days ago. One seen twice, one once.
	seenTwice := Entry{Seen: 2, SeenAt: now.Add(-20 * 24 * time.Hour)}
	seenOnce := Entry{Seen: 1, SeenAt: now.Add(-20 * 24 * time.Hour)}
	if score(seenTwice, now) <= score(seenOnce, now) {
		t.Errorf("twice-asserted scores %v, once-asserted %v: the count is not doing anything",
			score(seenTwice, now), score(seenOnce, now))
	}
}

// An entry written before this field existed must keep working. Treating a zero
// Seen as zero corroboration would put every pre-existing memory below the floor
// and silently empty the store on upgrade.
func TestAnEntryWithoutASeenCountIsStillRendered(t *testing.T) {
	legacy := Entry{Content: "written before corroboration existed"}
	now := time.Now()
	if score(legacy, now) < memoryScoreFloor {
		t.Errorf("a legacy entry scores %v, below the floor: upgrading would empty the store", score(legacy, now))
	}
	if !strings.Contains(RenderContext([]Entry{legacy}, 8000), "written before") {
		t.Error("a legacy entry was dropped from the rendered context")
	}
}

// Ranking is by weight, not recency, and the order must be stable so two entries
// asserted in the same turn cannot swap between renders.
func TestRenderingRanksByWeightAndIsStable(t *testing.T) {
	now := time.Now()
	// Insertion order deliberately opposite to weight order.
	strong := Entry{Content: "strong", Seen: 6, SeenAt: now}
	weak := Entry{Content: "weak", Seen: 1, SeenAt: now}
	tieA := Entry{Content: "tie-a", Seen: 3, SeenAt: now}
	tieB := Entry{Content: "tie-b", Seen: 3, SeenAt: now}

	rendered := RenderContext([]Entry{weak, tieB, strong, tieA}, 8000)
	strongAt := strings.Index(rendered, "strong")
	weakAt := strings.Index(rendered, "weak")
	if strongAt < 0 || weakAt < 0 {
		t.Fatalf("entries missing from the render:\n%s", rendered)
	}
	if strongAt > weakAt {
		t.Errorf("a strongly asserted memory rendered after a weak one:\n%s", rendered)
	}
	// Insertion order is weak, tieB, strong, tieA, so tieB was inserted first and
	// a stable sort must keep it there.
	if strings.Index(rendered, "tie-b") > strings.Index(rendered, "tie-a") {
		t.Errorf("equal scores did not keep insertion order:\n%s", rendered)
	}
}

// The byte budget still holds once ranking is in play, or the whole prompt grows
// without bound.
func TestRankingStillRespectsTheByteBudget(t *testing.T) {
	now := time.Now()
	entries := make([]Entry, 0, 40)
	for i := 0; i < 40; i++ {
		entries = append(entries, Entry{
			Content: strings.Repeat("x", 80),
			Seen:    1,
			SeenAt:  now,
		})
	}
	rendered := RenderContext(entries, 400)
	if len(rendered) > 400 {
		t.Errorf("rendered %d bytes against a 400 byte budget", len(rendered))
	}
	if !strings.HasPrefix(rendered, "<memory>") {
		t.Errorf("budgeted render lost its prefix:\n%s", rendered)
	}
}

// A clock moving backwards must not produce a negative age and a score above 1.
func TestAFutureAssertionIsClamped(t *testing.T) {
	now := time.Now()
	future := Entry{Seen: 1, SeenAt: now.Add(time.Hour)}
	got := score(future, now)
	if got <= 0 || got > 1 {
		t.Errorf("a future assertion scored %v, want a value in (0,1]", got)
	}
}

// Corroboration has diminishing returns, and that is the design rather than an
// accident of the curve.
//
// A memory asserted once lives about 51 days before it falls below the floor;
// asserted ten times, about 107. Corroboration buys longevity, but a linear count
// would let a frequently-touched trivial note outlive a genuinely load-bearing
// preference, so the tenth confirmation adds far less than the second.
func TestCorroborationHasDiminishingReturns(t *testing.T) {
	if got := corroborationWeight(0); got != corroborationWeight(1) {
		t.Errorf("corroborationWeight(0) = %v but weight(1) = %v; an unconfirmed entry must still count as one",
			got, corroborationWeight(1))
	}
	if !monotonic(corroborationWeight) {
		t.Error("corroboration weight is not monotonic in the confirmation count")
	}
	if hundred := corroborationWeight(100); hundred >= 1 {
		t.Errorf("corroborationWeight(100) = %v, must stay below 1", hundred)
	}

	firstGain := corroborationWeight(2) - corroborationWeight(1)
	tenthGain := corroborationWeight(11) - corroborationWeight(10)
	if tenthGain >= firstGain {
		t.Errorf("the tenth confirmation gained %v against the second's %v: returns are not diminishing",
			tenthGain, firstGain)
	}
}

// The reason saturation is acceptable: corroboration is a shield against decay,
// so a well-asserted preference does not fade merely because time passed.
func TestCorroborationBuysLongevity(t *testing.T) {
	once := daysUntilFloor(corroborationWeight(1))
	many := daysUntilFloor(corroborationWeight(20))
	if many <= once {
		t.Errorf("a memory asserted 20 times lasts %.0f days against %.0f for one asserted once",
			many, once)
	}
	// And the bound that matters: it must not become permanent.
	if many > 4*memoryHalfLifeDays {
		t.Errorf("a corroborated memory lasts %.0f days, over %.0f half-lives: decay cannot reach it",
			many, memoryHalfLifeDays)
	}
}

// daysUntilFloor is how long an entry of this weight survives with no further
// confirmation.
func daysUntilFloor(weight float64) float64 {
	if weight <= memoryScoreFloor {
		return 0
	}
	return math.Log(weight/memoryScoreFloor) / math.Ln2 * memoryHalfLifeDays
}

func monotonic(f func(int) float64) bool {
	previous := f(0)
	for i := 1; i <= 200; i++ {
		got := f(i)
		if got < previous {
			return false
		}
		previous = got
	}
	return true
}

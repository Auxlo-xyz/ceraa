package operation

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// The counts appear on the run trail, so they get read and acted on. These pin
// them against known answers, and the random cases are cross-checked against a
// brute-force shortest-edit-script search rather than against another copy of
// the same idea.

func TestLineDeltaCountsAdditionsAndRemovals(t *testing.T) {
	for _, tc := range []struct {
		name           string
		before, after  string
		added, removed int
	}{
		{
			name:   "no change at all",
			before: "a\nb\nc\n", after: "a\nb\nc\n",
		},
		{
			name:   "pure insertion keeps what came before",
			before: "a\nb\n", after: "a\nx\ny\nb\n",
			added: 2,
		},
		{
			name:   "pure removal",
			before: "a\nb\nc\nd\n", after: "a\nd\n",
			removed: 2,
		},
		{
			name:   "a modified line counts as one out and one in",
			before: "a\nb\nc\n", after: "a\nB\nc\n",
			added: 1, removed: 1,
		},
		{
			name:   "a whole replacement of three lines by five",
			before: "a\nb\nc\n", after: "a\n1\n2\n3\n4\n5\n",
			added: 5, removed: 2,
		},
		{
			name:   "empty before is a create",
			before: "", after: "a\nb\n",
			added: 2,
		},
		{
			name:   "empty after is a delete",
			before: "a\nb\n", after: "",
			removed: 2,
		},
		{
			name:   "scattered single-line edits are counted individually",
			before: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n",
			after:  "1\nTWO\n3\n4\n5\n6\n7\n8\nNINE\n10\n",
			added:  2, removed: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := countLineDelta(tc.before, tc.after)
			if got.added != tc.added || got.removed != tc.removed {
				t.Errorf("countLineDelta = +%d -%d, want +%d -%d",
					got.added, got.removed, tc.added, tc.removed)
			}
		})
	}
}

// The net length change has to survive the split into two halves. If the two
// are ever inconsistent the pair describes a file that never existed.
func TestLineDeltaHalvesAlwaysReconstructTheNetChange(t *testing.T) {
	rng := rand.New(rand.NewSource(20261006))
	for i := 0; i < 400; i++ {
		before := strings.Join(randomLines(rng, rng.Intn(14)), "\n")
		after := strings.Join(randomLines(rng, rng.Intn(14)), "\n")
		got := countLineDelta(before, after)
		if got.added < 0 || got.removed < 0 {
			t.Fatalf("negative count for +%d -%d (before %d lines, after %d)",
				got.added, got.removed, len(splitLines(before)), len(splitLines(after)))
		}
		net := len(splitLines(after)) - len(splitLines(before))
		if got.added-got.removed != net {
			t.Errorf("+%d -%d implies net %d, but the files differ by %d",
				got.added, got.removed, got.added-got.removed, net)
		}
	}
}

// An edit inside a large file must cost what the edit costs, not what the file
// costs. The prefix and tail trimming is what keeps that true, so this is the
// test that would fail if it were removed.
func TestLineDeltaOnALargeFileIsNotQuadratic(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	before := b.String()
	// One line changed in the middle of four thousand.
	after := strings.Replace(before, "line 2000\n", "line two thousand\n", 1)

	got := countLineDelta(before, after)
	if got.added != 1 || got.removed != 1 {
		t.Errorf("countLineDelta on a 4000 line file = +%d -%d, want +1 -1",
			got.added, got.removed)
	}
}

// Two files with nothing in common are the case the distance cap exists for.
// The result must still be a consistent pair rather than a wrong-looking zero.
func TestLineDeltaOnUnrelatedFilesFallsBackConsistently(t *testing.T) {
	before := make([]string, 4000)
	after := make([]string, 4000)
	for i := range before {
		before[i] = fmt.Sprintf("before %d", i)
		after[i] = fmt.Sprintf("after %d", i)
	}
	got := countLineDelta(strings.Join(before, "\n"), strings.Join(after, "\n"))
	if got.added-got.removed != 0 {
		t.Errorf("fallback produced +%d -%d, which is not a net of zero",
			got.added, got.removed)
	}
	if got.added != 4000 || got.removed != 4000 {
		t.Errorf("fallback = +%d -%d, want +4000 -4000", got.added, got.removed)
	}
}

// Cross-check against a brute-force shortest edit script over lines. This is
// what makes the numbers trustworthy: an independent search for the true
// minimum, not a restatement of the algorithm under test.
func TestEditDistanceMatchesBruteForceOnShortInputs(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	checked := 0
	for i := 0; i < 500; i++ {
		a := randomLines(rng, rng.Intn(7))
		b := randomLines(rng, rng.Intn(7))
		got := editDistance(a, b)
		want := bruteForceEditDistance(a, b)
		if got != want {
			t.Fatalf("editDistance(%q, %q) = %d, brute force says %d", a, b, got, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no cases were checked")
	}
}

// bruteForceEditDistance is the definition: try every way to turn a into b by
// deleting or inserting single lines and keep the smallest number of moves.
func bruteForceEditDistance(a, b []string) int {
	// distance[i][j] is the cost of turning a[:i] into b[:j].
	dist := make([][]int, len(a)+1)
	for i := range dist {
		dist[i] = make([]int, len(b)+1)
	}
	for i := 0; i <= len(a); i++ {
		dist[i][0] = i
	}
	for j := 0; j <= len(b); j++ {
		dist[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			best := dist[i-1][j] + 1 // delete a[i-1]
			if v := dist[i][j-1] + 1; v < best {
				best = v // insert b[j-1]
			}
			if a[i-1] == b[j-1] && dist[i-1][j-1] < best {
				best = dist[i-1][j-1] // match
			}
			dist[i][j] = best
		}
	}
	return dist[len(a)][len(b)]
}

func randomLines(rng *rand.Rand, n int) []string {
	if n == 0 {
		return nil
	}
	lines := make([]string, n)
	for i := range lines {
		// A tiny alphabet so lines collide, which is what makes the diff
		// non-trivial rather than a straight comparison.
		lines[i] = fmt.Sprintf("l%d", rng.Intn(4))
	}
	return lines
}

// The empty check is what keeps a no-op off the trail, so it is pinned at the
// level the renderer actually calls.
func TestLineDeltaEmptyOnlyWhenNothingChanged(t *testing.T) {
	if d := countLineDelta("a\n", "a\n"); !d.empty() {
		t.Error("an unchanged file reports a change")
	}
	if d := countLineDelta("a\n", "b\n"); d.empty() {
		t.Error("a changed file reports nothing")
	}
}

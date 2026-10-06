package operation

// lineDelta is how many lines one mutation added to and removed from a file.
//
// This exists to be shown, not to be applied. Nothing reads it to reconstruct a
// file, so the only requirement is that it not lie: a number the run reports is
// read and acted on, and one that is quietly wrong is worse than none.
type lineDelta struct {
	added   int
	removed int
}

// empty reports whether the mutation changed nothing at all, which is how a
// no-op edit stays silent on the trail instead of announcing (+0 -0).
func (d lineDelta) empty() bool {
	return d.added == 0 && d.removed == 0
}

// countLineDelta reports the lines a mutation added and removed.
//
// It counts a real line diff, so the numbers match what `git diff --numstat`
// would say for the same change, including treating a modified line as one
// removal plus one addition. An approximate count would be much shorter to
// write and wrong in the cases that matter: trimming only the outermost
// unchanged run reports a batch of scattered single-line edits as one block
// spanning the gap between the first and the last.
//
// Both sides are already in memory when this is called, so nothing is re-read
// from disk.
func countLineDelta(before, after string) lineDelta {
	a := splitLines(before)
	b := splitLines(after)

	// Trim the shared head and tail before diffing. This is not an
	// approximation: Myers walks exactly these lines in its opening snake, so
	// taking them first yields the same distance while shrinking the problem to
	// the part that actually changed. Without it, editing one line of a
	// five-thousand-line file would diff all five thousand.
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head &&
		a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	middleA := a[head : len(a)-tail]
	middleB := b[head : len(b)-tail]
	if len(middleA) == 0 && len(middleB) == 0 {
		return lineDelta{}
	}

	distance := editDistance(middleA, middleB)
	if distance < 0 {
		// The two sides have almost nothing in common and the exact count would
		// cost more than a status line can spend. Treating the whole middle as
		// changed is what the diff would have concluded anyway; it just costs
		// less than proving it.
		return lineDelta{added: len(middleB), removed: len(middleA)}
	}

	// distance counts insertions plus deletions, and the length change is
	// insertions minus deletions, so the two together give each half. The
	// trimming above cancelled out, so the net over the whole file equals the
	// net over the middle.
	net := len(b) - len(a)
	return lineDelta{
		added:   (distance + net) / 2,
		removed: (distance - net) / 2,
	}
}

// splitLines comes from operation.go rather than being defined again here. That
// version drops the empty element a trailing newline leaves behind, for reasons
// a range read depends on, and counting lines any other way in the same package
// would be two definitions of the same thing that disagree by one.

// maxEditDistance bounds the work. The algorithm costs O(distance squared), so
// without a ceiling two unrelated files of a few thousand lines each would
// spend longer on a display counter than the edit itself took.
const maxEditDistance = 3000

// editDistance returns the fewest insertions and deletions that turn a into b,
// or -1 when that exceeds maxEditDistance.
//
// Myers' algorithm, counting only: this walks the diagonals until the whole
// pair is consumed and returns how many steps that took. The trace itself is
// never stored, so memory is one array over the diagonal range rather than the
// usual full grid. Only the total is needed here, since the split into added
// and removed falls out of it arithmetically.
func editDistance(a, b []string) int {
	n, m := len(a), len(b)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}

	limit := n + m
	if limit > maxEditDistance {
		limit = maxEditDistance
	}
	// Diagonals run from -limit to +limit, with room on each side for the
	// reads at k-1 and k+1 that the move decision makes.
	offset := limit + 1
	v := make([]int, 2*limit+3)
	v[offset+1] = 0

	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			// Even k moves down, which is an insertion from the line below;
			// odd k moves right, which is a deletion from the line above. The
			// branch keeps whichever origin reaches further.
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			// The diagonal origin guarantees y is never negative, but the snake
			// indexes b with it directly, so the bound is checked rather than
			// assumed.
			if y < 0 {
				continue
			}
			// The snake: matching lines cost nothing, so consume them all before
			// counting the next step.
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return d
			}
		}
	}
	return -1
}

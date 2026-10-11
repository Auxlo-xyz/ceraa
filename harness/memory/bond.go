package memory

import "strings"

// Combine chains renderers in order, appending each one's output.
//
// It is the only thing left in this file, and the reason is worth recording.
// Beside it sat LiveBond: the renderer that re-read BOND.md and turned it into
// a <bond> prompt block wrapped in a read-only law. That was the second of two
// renderers for one concept - the bond's content was maintained by in-place
// rewrites in harness/bond and read back here - and the duplication is exactly
// what this change removes. The renderer is deleted, along with the law string
// it wrapped and the <bond> tag it emitted.
//
// The bond's content is now one entry in the append-only Memory.md log, and its
// delivery is the sidekick's, out of the same log tail every other memory is
// read from. There is nothing here to render, so there is nothing here to keep
// in step with it.
//
// Combine survives because chaining prompt renderers is still what its callers
// do. The bond renderer that used to be its first argument is gone; those call
// sites drop that argument themselves, because they live outside this package.
//
// A failing source contributes nothing but does not blank the others; the first
// error is reported.
func Combine(renderers ...func() (string, error)) func() (string, error) {
	return func() (string, error) {
		var parts []string
		var firstErr error
		for _, render := range renderers {
			if render == nil {
				continue
			}
			rendered, err := render()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if rendered != "" {
				parts = append(parts, rendered)
			}
		}
		if len(parts) == 0 {
			return "", firstErr
		}
		return strings.Join(parts, "\n") + "\n", firstErr
	}
}

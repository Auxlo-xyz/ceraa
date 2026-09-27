package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// A mistyped subcommand used to be treated as a prompt.
//
// root has to accept arbitrary arguments, because `ceraa "do something"` is the
// documented one-shot form. That also means `ceraa stopp` was accepted, sent to
// the model as a prompt, and answered. The user got a nonsense reply and a real
// inference call was spent on it.
//
// Rejecting every single-word argument is not an option, because `ceraa ok` is a
// legitimate prompt. So the guard is narrow: only a near miss of a real
// subcommand is refused, and only when the input could plausibly have been one.
// Anything else is a prompt, as before.

// nearMiss reports the closest subcommand to input, when input is close enough to
// be a typo of it.
//
// Damerau-Levenshtein rather than plain Levenshtein, so a transposition counts as
// one edit: `strt` for `start` is the commonest typo of all and plain distance
// scores it as two, which would push it outside the threshold.
//
// The threshold widens with length because short words are dense in edit space:
// `ok` is two edits from `stop`, and refusing it would break a valid prompt.
func nearMiss(input string, commands []string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	// Anything with whitespace is unambiguously a prompt, never a command.
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\n") {
		return "", false
	}
	best, bestDistance := "", -1
	for _, command := range commands {
		distance := damerauLevenshtein(trimmed, command)
		if distance > toleranceFor(command) {
			continue
		}
		if bestDistance < 0 || distance < bestDistance ||
			(distance == bestDistance && len(command) > len(best)) {
			best, bestDistance = command, distance
		}
	}
	return best, best != ""
}

// toleranceFor is how many edits still count as a typo of this command.
func toleranceFor(command string) int {
	if len(command) >= 5 {
		return 2
	}
	return 1
}

// damerauLevenshtein is edit distance with adjacent transposition counted once.
func damerauLevenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	// previousPrevious tracks the diagonal two cells back, which is what makes a
	// transposition cost one rather than two.
	prevPrev := make([]int, len(br)+1)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := 0; j <= len(br); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				if swapped := prevPrev[j-2] + 1; swapped < curr[j] {
					curr[j] = swapped
				}
			}
		}
		copy(prevPrev, prev)
		copy(prev, curr)
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// knownSubcommands holds the root's subcommand names.
//
// Populated by registerSubcommands rather than read from root, because root's own
// definition refers to nearMiss: reaching back into root from there is an
// initialisation cycle, and Go rejects it at compile time. Recording the names as
// they are attached breaks the loop and keeps the list in step with what is
// actually registered.
var knownSubcommands []string

// recordSubcommand notes a name for the near-miss check.
//
// help and completion are excluded: they are cobra's own, and treating `ceraa hep`
// as a typo of `help` would be more annoying than helpful for a word that is
// almost certainly a prompt.
func recordSubcommand(command *cobra.Command) {
	switch command.Name() {
	case "help", "completion":
		return
	}
	knownSubcommands = append(knownSubcommands, command.Name())
}

// subcommandNames returns the recorded names.
func subcommandNames() []string { return knownSubcommands }

// errNearMiss is the error for a first argument that is clearly a mistyped
// subcommand.
func errNearMiss(input, suggestion string) error {
	return fmt.Errorf("unknown command %q; did you mean %q? (a prompt is anything else, e.g. %q)",
		input, suggestion, "ceraa \"summarise this repo\"")
}

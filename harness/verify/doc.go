// Package verify is the policy for "is this bug real".
//
// It exists because the alternative is the model confirming its own finding:
// the same transcript, the same tools, and a prior that a bug was requested.
// XBOW reached the top of HackerOne's US leaderboard and concluded from their
// own runs that models are good at finding and bad at validating, so they used
// non-AI code for the second half.
//
// The policy is deliberately small and deliberately total. Decide reads one
// recorded exchange and a list of controls and returns one of four verdicts.
// It performs no I/O, consults no clock and keeps no state, so it can be read
// as the definition of the policy rather than as an implementation of it, and
// so it can be tested exhaustively instead of by example.
//
// Three ideas carry the package:
//
//   - A control asks what a naive check would have accepted that is not this
//     bug. A 200 is what a public page returns and a reflected parameter is
//     what a web server does, so neither is evidence on its own.
//   - An obligation resolves three ways, not two. Established, Contradicted
//     and Missing are different claims: "I looked and it did not fire" is
//     not the same claim as "nothing was instrumented".
//   - A marker has to be anchored. A validator that finds its own canary in
//     a fresh response has proved that the server echoes input, which is
//     true of essentially every web server and is not a vulnerability. The
//     marker must be absent from the baseline as well, and that check is
//     never dropped to make a finding pass.
//
// The package fetches nothing. Feeding it live exchanges later is a separate
// decision, and the shape is meant to allow that without changing any of the
// logic above.
package verify

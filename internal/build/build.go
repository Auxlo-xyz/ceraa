// Package build carries identity and terms into the binary.
//
// The version and the licence notice are compiled in rather than read from a file
// beside the executable, because a licence file does not travel: the single binary
// is what gets copied and redistributed, and a redistributor who never saw LICENSE
// cannot have been told what the terms were.
//
// Nothing here is meant to be unremovable. A string in a binary can be stripped by
// anyone determined, and the executable would still run. Its value is evidential: it
// fixes what the terms were in the artifact someone is holding, which is what a
// takedown or a dispute turns on.
package build

const (
	// Owner is the copyright holder.
	Owner = "emperormk01"

	// Notice is the one-line statement of terms, printed by `ceraa version`.
	Notice = "Copyright (c) 2026 " + Owner + ". All rights reserved."

	// LicenseURL points at the terms.
	LicenseURL = "See the LICENSE file distributed with this software."
)

// Version is the release version, injected at link time.
//
// It was a constant, which meant every release reported 0.1.0 forever: the tag
// named one version and the binary named another, and nothing failed. A tester
// installing v0.1.1 was told it had 0.1.0. The release build now sets it with
// -ldflags "-X", and the default below is what a plain `go build` from the tree
// reports, which is a development build rather than pretending to be a release.
//
// The value is a var, not a const, because -X can only set a string variable.
var Version = "0.4.6-dev"

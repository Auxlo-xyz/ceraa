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
	// Version is the release version.
	Version = "0.1.0"

	// Owner is the copyright holder.
	Owner = "emperormk01"

	// Notice is the one-line statement of terms, printed by `ceraa version`.
	Notice = "Copyright (c) 2026 " + Owner + ". All rights reserved."

	// LicenseURL points at the terms.
	LicenseURL = "See the LICENSE file distributed with this software."
)

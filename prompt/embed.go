package prompt

import _ "embed"

// System is the prompt compiled into released binaries.
//
// The source remains in prompt/system.txt so it can be reviewed and changed,
// but runtime no longer depends on a sidecar file.
//
//go:embed system.txt
var System []byte

package coordinator

import (
	"ceraa/harness/compaction"
	"ceraa/harness/contextbuilder"
	"ceraa/harness/sessionstore"
)

// renderForTest exercises the production render path, which is where a tool
// result must be truncated.
func renderForTest(sess *sessionstore.Session) any {
	return contextbuilder.New(sess, "system").Build().Messages
}

func truncateForTest(s string) string { return compaction.TruncateToolOutput(s) }

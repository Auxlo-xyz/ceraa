package coordinator

import (
	"ceraa/harness/contextbuilder"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// renderForTest exercises the production render path, which is where a stored
// tool result must be passed through untouched.
func renderForTest(sess *sessionstore.Session) []llm.Item {
	return contextbuilder.New(sess, "system").Build().Messages
}

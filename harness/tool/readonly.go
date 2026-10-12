package tool

// ReadOnly reports whether running the named tool can change anything.
//
// It exists for one question: a tool call was interrupted and its outcome is
// unknown, so may the model run it again? For a tool that only observes, yes. For
// anything else, repeating it is how a file gets written twice.
//
// It is an opt-in list rather than a method on the Tool interface on purpose. The
// safe answer for a tool nobody has classified is no, and an absent name costs
// nothing but a slightly more cautious sentence. A method would force every tool
// to answer and a single careless true would be a safety bug, so the failure mode
// of the list is the conservative one.
//
// The list is pinned by a test. Adding a tool means deciding, deliberately,
// whether repeating it after an unknown outcome is safe.
//
// browser is deliberately absent: it can click and type, which changes state on
// somebody else's server.
var readOnly = map[string]bool{
	"read":      true,
	"webfetch":  true,
	"websearch": true,

	"list_secrets": true,
	"get_goal":     true,
	"question":     true,
}

// ReadOnly reports whether the named tool cannot change anything.
func ReadOnly(name string) bool { return readOnly[name] }

// ReadOnlyNames lists the classified tools, for a test that pins the set.
func ReadOnlyNames() []string {
	out := make([]string, 0, len(readOnly))
	for name := range readOnly {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

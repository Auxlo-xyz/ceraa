package coordinator

import (
	"path/filepath"
	"strings"
)

// MatchesScope answers "does this applies glob cover this path".
//
// It lived in harness/memory beside the memory renderer it was extracted from,
// and outlived the feature: instruction delivery is its only remaining caller.
// A scope question about a project rule is not a question about a memory, and
// keeping the grammar here leaves one implementation in the tree rather than a
// second thing to keep correct.
//
// Every clause below was a bug found once already, which is why they are all
// still here: a whole-path glob, a base-name glob, a directory prefix, and a
// relative pattern against the absolute target an operation actually carries.
// It is deliberately lexical, not semantic.
//
// An empty applies matches nothing: general is a delivery mode, not an empty
// scope.
func MatchesScope(applies, target string) bool {
	if applies == "" || target == "" {
		return false
	}
	// filepath.Match does not cross separators, so a glob is tried against both
	// the whole path and the base name. That is how "*.go" reaches
	// /srv/app/main.go rather than only a top-level main.go.
	if ok, err := filepath.Match(applies, target); err == nil && ok {
		return true
	}
	base := filepath.Base(target)
	if ok, err := filepath.Match(applies, base); err == nil && ok {
		return true
	}
	// A directory prefix should match everything beneath it, which Match will not
	// do across separators. A bare filename is treated as a base-name match, so a
	// rule written as "main.go" reaches a tool called on /srv/main.go.
	if filepath.Base(applies) == applies {
		return base == applies
	}
	prefix := strings.TrimSuffix(applies, string(filepath.Separator)) + string(filepath.Separator)
	if strings.HasPrefix(target, prefix) || target == applies {
		return true
	}
	// A relative pattern against an absolute target. Operations carry absolute
	// paths, so a rule written as "src/*" would otherwise match nothing at all,
	// which is a silent failure for the most natural way to write one. Each
	// trailing run of segments is tried, so a relative pattern is compared like
	// for like.
	if !filepath.IsAbs(applies) {
		segments := strings.Split(target, string(filepath.Separator))
		for i := 1; i < len(segments); i++ {
			suffix := strings.Join(segments[i:], string(filepath.Separator))
			if ok, err := filepath.Match(applies, suffix); err == nil && ok {
				return true
			}
			if strings.HasPrefix(suffix, prefix) || suffix == applies {
				return true
			}
		}
	}
	return false
}

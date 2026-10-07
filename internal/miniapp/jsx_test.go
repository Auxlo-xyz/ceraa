package miniapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The dashboard is JSX in an HTML file, compiled in the browser by Babel. Every
// Go test in this package reads that file as text, so none of them can tell a
// syntax error from a valid file: a duplicate const declaration, a stray brace
// from a bad splice, a component used before its declaration. Those all ship.
//
// esbuild parses JSX, and one is vendored on this host. When it is not present
// the test reports that it skipped rather than failing, because a release
// machine without esbuild is not a broken build.
// The regex cannot express "not a src script" the way Go's engine can, so it
// matches every block and the src ones are dropped by hand below. Go's RE2 has
// no lookahead, which is why this is not written the obvious way.
var anyScriptBlock = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

func TestTheDashboardCompiles(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	matches := anyScriptBlock.FindAllSubmatch(page, -1)
	if len(matches) == 0 {
		t.Fatal("no script block found in the dashboard")
	}
	longest := ""
	for _, m := range matches {
		// The tag attributes are not captured, so a block whose body is empty
		// is a src reference. The app script is by far the longest body, and
		// requiring a real length keeps a stray empty block from winning.
		if len(m[1]) > len(longest) {
			longest = string(m[1])
		}
	}
	if strings.TrimSpace(longest) == "" {
		t.Fatal("the dashboard has no inline script to check")
	}
	esbuild := findEsbuild()
	if esbuild == "" {
		t.Skip("esbuild not on PATH; skipping the dashboard compile check")
	}
	tmp := filepath.Join(t.TempDir(), "dashboard.js")
	if err := os.WriteFile(tmp, []byte(longest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(esbuild, "--loader:.js=jsx", "--outfile="+filepath.Join(t.TempDir(), "out.js"), tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the dashboard does not compile: %s\n%s", err, out)
	}
}

// findEsbuild looks for an esbuild binary near this package, then on PATH.
//
// It deliberately searches upwards rather than only checking PATH: the one
// esbuild on a Ceraa host belongs to an unrelated vendored project, so a
// PATH-only lookup skips the check in exactly the environment where the
// dashboard is being edited. A skip is still possible on a bare machine, which
// is why the skip says so rather than passing quietly.
func findEsbuild() string {
	var candidates []string
	if root, err := os.Getwd(); err == nil {
		for i := 0; i < 6; i++ {
			candidates = append(candidates,
				filepath.Join(root, "node_modules", ".bin", "esbuild"),
				filepath.Join(root, "node_modules", "esbuild", "bin", "esbuild"),
				filepath.Join(root, "node_modules", "@esbuild", "linux-x64", "bin", "esbuild"),
			)
			root = filepath.Dir(root)
		}
	}
	if p, err := exec.LookPath("esbuild"); err == nil {
		candidates = append(candidates, p)
	}
	// Sibling trees. An esbuild installed for another project on the same host
	// is still an esbuild, and searching only this tree's ancestors finds
	// nothing on a machine that has one - which is how this check skipped
	// silently on the box that needed it.
	for _, sibling := range []string{
		"/workspace/hermes-agent/node_modules/.bin/esbuild",
		"/workspace/hermes-agent/node_modules/@esbuild/linux-x64/bin/esbuild",
	} {
		candidates = append(candidates, sibling)
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// The provider dialog regressed twice in ways only a screenshot caught: the
// panel was anchored to the bottom, and the floating nav drew over it because
// the backdrop sat below the nav's z-index. Both are CSS facts, so they are
// asserted here rather than left to be re-found by eye.
func TestTheProviderDialogIsCentredAndAboveTheNav(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	css := pageCSS(t, page)

	backdrop := cssRule(t, css, "pick-backdrop")
	if !strings.Contains(backdrop, "align-items: center") {
		t.Error("the provider dialog is not vertically centred")
	}
	backdropZ := atoi(cssValue(backdrop, "z-index"))
	// Highest, not first: the nav's z-index is declared in a media query, so the
	// base rule does not carry it and reading only that compares against nothing.
	navZ := maxZIndex(css, "mobile-capsule-wrapper", "capsule-dock")
	if backdropZ == 0 || navZ == 0 {
		t.Fatalf("cannot compare z-index: backdrop %d, nav %d", backdropZ, navZ)
	}
	if backdropZ <= navZ {
		t.Errorf("the dialog is z-index %d, not above the nav's %d, so the nav draws over it", backdropZ, navZ)
	}
}

// The model and the endpoint ran together as one line because both are spans
// inside a button, so they are inline unless display:block is stated. The
// element type does not imply it, which is why this has to be asserted.
func TestTheProviderRowDetailsStack(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	css := pageCSS(t, page)
	for _, sel := range []string{"pick-model", "pick-meta"} {
		rule := cssRule(t, css, sel)
		if !strings.Contains(rule, "display: block") {
			t.Errorf(".%s is not display:block, so it sits inline and collides with its neighbours", sel)
		}
	}
}

// pageCSS returns the dashboard's first stylesheet.
func pageCSS(t *testing.T, page []byte) string {
	t.Helper()
	block := regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`).FindSubmatch(page)
	if block == nil {
		t.Fatal("the dashboard has no stylesheet")
	}
	return string(block[1])
}

// cssSelector returns the declaration body for a literal selector, so a
// compound one like .hero-band.flush is matched as written rather than escaped
// into something that cannot occur.
func cssSelector(t *testing.T, css, selector string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(selector) + `\s*\{(.*?)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("no rule for %s", selector)
	}
	return m[1]
}

// cssRule returns the declaration body for one class selector, following the
// base rule rather than any responsive override, because the alignment and the
// stacking are set on the base.
func cssRule(t *testing.T, css, class string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)\.` + regexp.QuoteMeta(class) + `\s*\{(.*?)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("no rule for .%s", class)
	}
	return m[1]
}

func cssValue(rule, property string) string {
	re := regexp.MustCompile(property + `\s*:\s*([^;]+)`)
	m := re.FindStringSubmatch(rule)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// maxZIndex is the largest z-index declared for any rule naming the class, so a
// value set inside a media query is compared rather than missed.
func maxZIndex(css string, classes ...string) int {
	highest := 0
	for _, class := range classes {
		re := regexp.MustCompile(`(?s)\.(?:` + regexp.QuoteMeta(class) + `)\s*\{([^}]*)\}`)
		for _, m := range re.FindAllStringSubmatch(css, -1) {
			if z := atoi(cssValue(m[1], "z-index")); z > highest {
				highest = z
			}
		}
	}
	return highest
}

// Every native select was the platform's own control in the middle of a page
// that is otherwise entirely custom, and each one opened a dark OS list over
// cream paper. The fix is the shared chooser, so the thing to hold is that no
// <select> comes back.
func TestTheDashboardHasNoNativeSelects(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	// The prose that explains the replacement still contains the word, so
	// comments are stripped first. Without that the test matches its own
	// explanation and reports a select that does not exist.
	markup := stripComments(string(page))
	element := regexp.MustCompile(`<select[\s>]`)
	if loc := element.FindStringIndex(markup); loc != nil {
		snippet := markup[loc[0]:]
		if len(snippet) > 90 {
			snippet = snippet[:90]
		}
		t.Errorf("a native select is back: %s", snippet)
	}
}

// stripComments removes HTML and CSS/JS block comments, so a search for an
// element cannot match the prose describing it.
func stripComments(s string) string {
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?s)/\*.*?\*/`),
		regexp.MustCompile(`(?s)<!--.*?-->`),
	} {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

// The band under a card header exists to separate a header from a body. A band
// that holds the whole card separates nothing, so the rule runs across the card
// with content on one side only and reads as a stray line; those bands are flush
// and the card's own border closes the shape. A band that does divide a body
// keeps its rule.
//
// Six bands hold a whole card and are flush: the four that open a tab (Overview,
// Context, Tools, Agents) plus the reasoning-effort lever and the secret store,
// which were bare section bands until they were given the tab-card layout.
//
// The flush count alone would not catch the dangerous half of this rule, so the
// card membership is checked too. flush on a band that is NOT inside a card is a
// section header that lost both its rule and its card, and it looks like nothing
// is wrong.
func TestABandThatHoldsTheWholeCardDropsTheRule(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	markup := stripComments(string(page))

	total := strings.Count(markup, `className="hero-band"`)
	flush := strings.Count(markup, `className="hero-band flush"`)
	// Twelve bands exist across the four tabs: six hold a whole card, six divide
	// a body beneath them.
	if flush != 6 {
		t.Errorf("%d bands are flush, want the 6 that hold a whole card", flush)
	}
	if total+flush != 12 {
		t.Errorf("found %d bands total (%d plain, %d flush), want 12; a card was added or lost", total, flush, total+flush)
	}

	lines := strings.Split(markup, "\n")
	for i, line := range lines {
		if !strings.Contains(line, `className="hero-band flush"`) {
			continue
		}
		if !opensACardAbove(lines, i) {
			t.Errorf("line %d flushes the band but no card opens above it; a section header would lose both its rule and its card", i+1)
		}
	}

	css := pageCSS(t, page)
	rule := cssRule(t, css, "hero-band")
	if !strings.Contains(rule, "border-bottom: 1px") {
		t.Error("the base band lost its rule, which the six dividing bands still need")
	}
	flushed := cssSelector(t, css, ".hero-band.flush")
	if !strings.Contains(flushed, "border-bottom: none") {
		t.Error("the flush variant does not remove the rule, so the six whole-card bands still draw it")
	}
}

// opensACardAbove reports whether line index sits inside a card, which is the
// band being the card's first child. It looks back a few non-blank lines for the
// card's opening div and stops at any closing div in between, so it survives
// reindentation without being fooled by a sibling card that has already closed.
func opensACardAbove(lines []string, index int) bool {
	seen := 0
	for i := index - 1; i >= 0 && seen < 3; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		seen++
		if strings.Contains(line, `className="card"`) && strings.HasPrefix(line, "<div") {
			return true
		}
		if strings.HasPrefix(line, "</") {
			return false
		}
	}
	return false
}

// A hairline-list that sits outside a card draws its top rule just under the
// card above and its last row's rule just above the card below, so one stray
// element put a line at both ends of the gap. Three lists sit outside a card:
// the cache dials, and the model-catalog and run-parameters lists, whose sections
// are each followed by a card. The rules are matched to the background rather
// than dropped, because removing them shifts everything above by a pixel.
func TestOnlyTheUncardedHairlineListIsBare(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	markup := stripComments(string(page))
	if got := strings.Count(markup, "hairline-list bare"); got != 3 {
		t.Errorf("%d lists are marked bare, want the 3 that sit outside a card", got)
	}
	css := pageCSS(t, page)
	bare := cssSelector(t, css, ".hairline-list.bare")
	if !strings.Contains(bare, "var(--ground)") {
		t.Error("the bare list does not match its dividers to the background")
	}
	last := cssSelector(t, css, ".hairline-list.bare > .h-row:last-child")
	if !strings.Contains(last, "var(--ground)") {
		t.Error("the bare list still rules a line under its last row")
	}
	// Every other list is inside a card, where those rules are suppressed and the
	// row dividers are what the reader relies on.
	base := cssRule(t, css, "hairline-list")
	if !strings.Contains(base, "border-top: 1px solid var(--rule)") {
		t.Error("the base list lost its divider, which the in-card lists still need")
	}
}

// The context window is deliberately left unset in config so the runner asks the
// model catalog, and the dashboard divided that unset value by a thousand and
// printed "0k tok". Nothing was broken except the claim: the agent's real budget
// was the resolved one. So the display must read the resolved value, and must say
// where it came from rather than implying the configured zero is the window.
func TestTheContextWindowIsNotReadRawFromConfig(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	markup := stripComments(string(page))

	if strings.Contains(markup, "runParams.contextLimit / 1000") {
		t.Error("the context window is divided by the raw configured value, which is zero by default and printed as 0k tok")
	}
	if !strings.Contains(markup, "runParams.contextLimitEffective") {
		t.Error("the context window is not read from the resolved limit the runner actually applies")
	}
	if !strings.Contains(markup, "function contextLimitNote(") {
		t.Fatal("the context window has no provenance note, so a resolved value is indistinguishable from a configured one")
	}
	for _, source := range []string{`"profile"`, `"catalog"`} {
		if !strings.Contains(markup, "case "+source+": return \"Auto") {
			t.Errorf("the context note does not cover the %s source", source)
		}
	}
	if !strings.Contains(markup, `default: return "Auto`) {
		t.Error("the context note has no fallback branch, so the common case says nothing about the floor it used")
	}
	// The configured value stays reported, because it is the row the operator
	// edits. Losing it would make the limit read-only while looking editable.
	if !strings.Contains(markup, "contextLimit: p.context_limit") {
		t.Error("the raw configured context_limit is no longer read, so the editable row lost its value")
	}
}

// The idle Status row printed the same sentence twice: "No active run in flight"
// as the row's own label and again as the placeholder step, so the card said one
// thing in two voices with the extra half of the phrase filling the space. Both
// are pinned because they are two strings in two places, and a copy edit that
// fixes one and not the other is exactly the failure.
func TestTheIdleRunRowSaysItOnceAndShortly(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("assets", "index.html"))
	if err != nil {
		t.Fatalf("read the dashboard: %s", err)
	}
	markup := stripComments(string(page))

	if got := strings.Count(markup, `"No active run in flight"`); got != 0 {
		t.Errorf("%d copies of the long idle wording remain; the row is meant to read \"No active run\"", got)
	}
	if got := strings.Count(markup, `"No active run"`); got != 2 {
		t.Errorf("%d copies of \"No active run\", want 2: the row label and the idle step placeholder", got)
	}
}

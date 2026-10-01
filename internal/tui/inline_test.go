package tui

import (
	"strings"
	"testing"
)

// The thing the screenshot showed: a tool catalogue with `**bold**`, `*italic*`
// and backticks printed exactly as written.
func TestInlineMarkersAreRenderedNotPrinted(t *testing.T) {
	got := noColorText(renderInline(Palette{color: true}, "Use `rg` and **careful** quoting"))
	if strings.Contains(got, "`") || strings.Contains(got, "**") {
		t.Errorf("markers survived: %q", got)
	}
	if got != "Use rg and careful quoting" {
		t.Errorf("read as %q, want the plain text", got)
	}
}

// Italic and struck have no safe rendering in a monochrome theme, so they lose
// their markers and stay plain. Leaving the asterisks on would be the worst of
// the three options.
func TestUnrenderableMarkersAreStrippedNotPrinted(t *testing.T) {
	for _, in := range []string{"*(via bash)*", "~~gone~~", "*emphasis*"} {
		got := noColorText(renderInline(Palette{color: true}, in))
		if strings.ContainsAny(got, "*~") {
			t.Errorf("%q kept its markers: %q", in, got)
		}
	}
}

// A single asterisk in front of a bold run must not swallow it. The two stray
// asterisks stay, because they are genuinely unmatched and pretending otherwise
// would invent an emphasis the model did not write. The bold is what matters.
func TestSingleAsteriskDoesNotStealABoldRun(t *testing.T) {
	got := noColorText(renderInline(Palette{color: true}, "*- **task** - delegate*"))
	if strings.Contains(got, "**") {
		t.Errorf("the bold run was left unrendered: %q", got)
	}
	if !strings.Contains(got, "task") || !strings.Contains(got, "delegate") {
		t.Errorf("content was lost: %q", got)
	}
}

// An odd asterisk count is prose with a stray one, not a run split across deltas.
// `2 * 3` is not an italic run. Markdown's own rule is that a delimiter followed
// by whitespace cannot open, and honouring it is what stops a multiplication
// sign eating everything between two stray asterisks.
func TestMultiplicationIsNotEmphasis(t *testing.T) {
	for _, in := range []string{"2 * 3", "a * b * c", "value: 4 * 2 = 8"} {
		got := noColorText(renderInline(Palette{color: true}, in))
		if got != in {
			t.Errorf("%q was read as %q", in, got)
		}
	}
}

func TestUnterminatedMarkerIsLeftAlone(t *testing.T) {
	got := noColorText(renderInline(Palette{color: true}, "2 * 3 and a lone * here"))
	if !strings.Contains(got, "*") {
		t.Errorf("a stray asterisk was consumed: %q", got)
	}
}

// snake_case everywhere in a tool answer. Stripping underscores as emphasis would
// rename every tool.
func TestInlineLeavesSnakeCaseAlone(t *testing.T) {
	got := noColorText(renderInline(Palette{color: true}, "**run_command_with_secret** and `fts_search`"))
	if got != "run_command_with_secret and fts_search" {
		t.Errorf("read as %q", got)
	}
}

// The continuation has to sit under the item, or the item stops looking like an
// item: three lines at the margin read as three unrelated sentences.
func TestListItemsHangTheirContinuationUnderTheText(t *testing.T) {
	rows := answerRows("- **fts_search** - Indexed full-text search across the workspace and returns ranked files", 40)
	if len(rows) < 2 {
		t.Fatalf("expected the item to wrap, got %q", rows)
	}
	// In cells, not bytes: the bullet glyph is three bytes wide and comparing byte
	// offsets says the continuation is two columns short when it is not.
	indent := len([]rune(rows[0][:strings.Index(rows[0], "fts_search")]))
	for i, r := range rows[1:] {
		if strings.TrimSpace(r) == "" {
			continue
		}
		lead := len([]rune(r[:len(r)-len(strings.TrimLeft(r, " "))]))
		if lead != indent {
			t.Errorf("continuation %d is at column %d, not under the text at %d: %q",
				i, lead, indent, r)
		}
		if n := len([]rune(r)); n > 40 {
			t.Errorf("continuation %d is %d cells: %q", i, n, r)
		}
	}
}

func TestListMarkerIsTheSameGlyphAsAToolLine(t *testing.T) {
	got := noColorText(answerRows("- one", 80)[0])
	if !strings.Contains(got, glyph("ok")) {
		t.Errorf("list item did not use the shared marker glyph: %q", got)
	}
}

func TestOrderedListKeepsItsNumber(t *testing.T) {
	got := noColorText(answerRows("1. first step", 80)[0])
	if !strings.HasPrefix(strings.TrimSpace(got), "1.") {
		t.Errorf("the number was dropped: %q", got)
	}
	// A decimal is not a list item.
	if _, _, ok := listItem("1.5 apples"); ok {
		t.Error("1.5 was taken for item one")
	}
}

func TestHeadingIsRenderedAsAHeading(t *testing.T) {
	for _, in := range []string{"**Search**", "## Browser", "### Deep"} {
		got := answerRows(in, 80)
		if len(got) != 1 {
			t.Fatalf("%q produced %d rows", in, len(got))
		}
		if strings.ContainsAny(got[0], "*#") {
			t.Errorf("%q kept its markers: %q", in, got[0])
		}
	}
	// A sentence with two runs in it is not a heading.
	if _, ok := headingText("**one** and **two**"); ok {
		t.Error("a bold sentence was taken for a heading")
	}
}

// A fence is a promise that what is inside is literal. Code is full of asterisks
// and underscores, and scanning it would mangle every line.
func TestCodeFenceIsPassedThroughVerbatim(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 40
	for _, line := range []string{
		"```sh", "grep -rn '**not bold**' src/   # literal", "  indented", "```", "**after** is bold",
	} {
		r.Content(line + "\n")
	}
	r.closeStream()
	got := b.String()
	if !strings.Contains(got, "grep -rn '**not bold**' src/   # literal") {
		t.Errorf("the fenced line was rewritten:\\n%s", got)
	}
	if !strings.Contains(got, "  indented") {
		t.Errorf("fence indentation was reflowed:\\n%s", got)
	}
	// The fence ends at its closing line, so what follows is rendered again.
	if !strings.Contains(noColorText(got), "after is bold") {
		t.Errorf("text after the fence was left literal:\\n%s", got)
	}
}

// Answer text is emitted as it is written, so the emphasis markers must be
// matched up at whatever boundary the newline falls.
func TestInlineIsAppliedAcrossArbitraryDeltaBoundaries(t *testing.T) {
	body := "- **task** - Delegate work (`explore` or `general`) and report back.\n**Done**\n"
	for _, size := range []int{1, 2, 3, 7, 16, 256} {
		var b strings.Builder
		r := NewRenderer(&b, Plain())
		r.cols = 100
		for i := 0; i < len(body); i += size {
			e := i + size
			if e > len(body) {
				e = len(body)
			}
			r.Content(body[i:e])
		}
		r.closeStream()
		got := noColorText(b.String())
		if strings.ContainsAny(got, "*`") {
			t.Errorf("delta size %d left markers: %q", size, got)
		}
		if !strings.Contains(got, "task - Delegate work (explore or general)") {
			t.Errorf("delta size %d lost content: %q", size, got)
		}
		if !strings.Contains(got, "Done") {
			t.Errorf("delta size %d lost the heading: %q", size, got)
		}
	}
}

// noColorText strips escape sequences so a test reads what a person sees.
func noColorText(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\x1b' {
			b.WriteRune(rs[i])
			continue
		}
		i = escapeEnd(rs, i) - 1
	}
	return b.String()
}

// answerRows renders one line of answer prose without a renderer.
func answerRows(line string, width int) []string {
	var rows []string
	for _, r := range Plain().answerBlock(line, width) {
		rows = append(rows, strings.TrimSuffix(noColorText(r), "\n"))
	}
	return rows
}

// A fenced line is longer than the terminal. Left alone it is cut at a cell by
// the terminal and the tail is gone, which on a narrow screen is most of a
// command. The indent is kept and the width respected instead.
func TestFencedLineIsWrappedNotTruncated(t *testing.T) {
	var b strings.Builder
	r := NewRenderer(&b, Plain())
	r.cols = 40
	r.Content("```sh\n")
	r.Content("    grep -rn --include='*.go' '**pattern**' ./internal/... \n")
	r.Content("```\n")
	r.closeStream()
	for _, row := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if n := len([]rune(row)); n > 40 {
			t.Errorf("fenced row is %d cells: %q", n, row)
		}
	}
	if !strings.Contains(b.String(), "grep -rn --include='*.go'") {
		t.Errorf("the start of the command was lost:\\n%s", b.String())
	}
	// Every row of a wrapped command keeps the indent, so the block still reads
	// as one command.
	// Every row of the wrapped command keeps the indent, so the block still reads
	// as one command rather than as three fragments.
	for _, row := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if strings.TrimSpace(row) == "" || strings.HasPrefix(row, "```") {
			continue
		}
		if !strings.HasPrefix(row, "    ") {
			t.Errorf("row did not keep the indent: %q", row)
		}
	}
}

// Tests for format.go: GitHub pipe tables become a monospace grid that lines up
// in WhatsApp, a table too wide for the grid becomes vertical rows, and nothing
// else is touched.
package whatsapp

import (
	"context"
	"strings"
	"testing"
)

func TestFormatTablesAlignsPipes(t *testing.T) {
	in := "Intro line.\n" +
		"| Name | Size |\n" +
		"|------|------|\n" +
		"| a | 12 |\n" +
		"| bb | 3 |\n" +
		"Outro line."
	want := "Intro line.\n" +
		"```\n" +
		"Name | Size\n" +
		"-----+-----\n" +
		"a    | 12\n" +
		"bb   | 3\n" +
		"```\n" +
		"Outro line."
	if got := formatTables(in); got != want {
		t.Errorf("formatTables mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesDropsOuterPipes(t *testing.T) {
	in := "| A | B |\n|---|---|\n| 1 | 2 |"
	want := "```\nA | B\n--+--\n1 | 2\n```"
	if got := formatTables(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesHonoursAlignmentColons(t *testing.T) {
	in := "| A | B |\n|:--|--:|\n| 1 | 2 |"
	want := "```\nA | B\n--+--\n1 | 2\n```"
	if got := formatTables(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesPadsShortRows(t *testing.T) {
	in := "| Name | Size |\n|------|------|\n| a |"
	want := "```\nName | Size\n-----+-----\na    |\n```"
	if got := formatTables(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesKeepsEscapedPipe(t *testing.T) {
	in := "| a \\| b | c |\n|--------|---|\n| x | y |"
	got := formatTables(in)
	if !strings.Contains(got, "a | b") {
		t.Fatalf("escaped pipe lost: %q", got)
	}
	lines := strings.Split(got, "\n")
	if len(lines) < 4 || lines[0] != "```" {
		t.Fatalf("no fenced block: %q", got)
	}
	header, sep := lines[1], lines[2]
	if strings.IndexByte(sep, '+') != strings.LastIndexByte(header, '|') {
		t.Errorf("separator not under the column bar: header=%q sep=%q", header, sep)
	}
}

func TestFormatTablesRendersWideTableAsVerticalRows(t *testing.T) {
	in := "| Ceraa component name | Resident memory use |\n" +
		"|----------------------|---------------------|\n" +
		"| ceraa start daemon | ~67 MB |\n" +
		"| whatsapp bridge | ~12 MB |"
	want := "ceraa start daemon\n" +
		"*Resident memory use*: ~67 MB\n" +
		"\n" +
		"whatsapp bridge\n" +
		"*Resident memory use*: ~12 MB"
	if got := formatTables(in); got != want {
		t.Errorf("wide table should fall back to vertical rows\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesKeepsAWideCellOnOneLine(t *testing.T) {
	// The bloat-table shape: a long path cell is the reason the grid does not fit,
	// and a vertical row must not pad or wrap it, only label the other columns.
	in := "| Path | Size |\n" +
		"|---|---|\n" +
		"| .gradle/jdks/OpenJDK17U-jdk_x64_linux_hotspot_17.0.15_6.tar.gz | 185M |"
	want := ".gradle/jdks/OpenJDK17U-jdk_x64_linux_hotspot_17.0.15_6.tar.gz\n" +
		"*Size*: 185M"
	if got := formatTables(in); got != want {
		t.Errorf("wide cell\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesSkipsFencedBlock(t *testing.T) {
	in := "```\n| A | B |\n|---|---|\n| 1 | 2 |\n```"
	if got := formatTables(in); got != in {
		t.Errorf("a table inside a fence must be left alone:\n%s", got)
	}
}

func TestFormatTablesLeavesOrdinaryText(t *testing.T) {
	cases := map[string]string{
		"prose":          "Just a line.\nAnd another.",
		"thematic break": "Above\n---\nBelow",
		"loose pipes":    "a | b\nc | d",
		"single blank":   "",
	}
	for name, in := range cases {
		if got := formatTables(in); got != in {
			t.Errorf("%s: changed text\n got:\n%s\nwant:\n%s", name, got, in)
		}
	}
}

func TestSendRendersTablesAsMonospace(t *testing.T) {
	b, ft := testBridge(t.TempDir(), &stubRun{})
	in := "| Name | Size |\n|------|------|\n| a | 12 |"
	if err := b.send(context.Background(), "2348012345678@s.whatsapp.net", in); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := ft.sentTexts()
	if len(got) != 1 {
		t.Fatalf("sent %d texts, want 1", len(got))
	}
	want := "```\nName | Size\n-----+-----\na    | 12\n```"
	if got[0].text != want {
		t.Errorf("outbound text\n got:\n%s\nwant:\n%s", got[0].text, want)
	}
}

func TestFormatEmphasisRewritesMarkdownToWhatsAppMarks(t *testing.T) {
	cases := map[string][2]string{
		"bold":                    {"**strong**", "*strong*"},
		"bold underscores":        {"__strong__", "*strong*"},
		"italic":                  {"*nice*", "_nice_"},
		"italic underscore":       {"_nice_", "_nice_"},
		"bold with italic inside": {"**bold with *italic* inside**", "*bold with _italic_ inside*"},
		"bold ending on italic":   {"**a *b***", "*a _b_*"},
		"bold italic":             {"***both***", "*_both_*"},
		"two spans":               {"**a** and *b*", "*a* and _b_"},
		"unmatched stays":         {"a lone ** star", "a lone ** star"},
	}
	for name, tc := range cases {
		in, want := tc[0], tc[1]
		if got := formatEmphasis(in); got != want {
			t.Errorf("%s:\n got: %q\nwant: %q", name, got, want)
		}
	}
}

// A bullet line opens with a star and no space rule would be a false
// positive on it, so "* item" must survive the pass untouched.
func TestFormatEmphasisLeavesBulletsAlone(t *testing.T) {
	cases := map[string]string{
		"bullet":      "* item one\n* item two",
		"star in use": "2 * 3 * 4 = 24",
		"hash note":   "see footnote * for details",
	}
	for name, in := range cases {
		if got := formatEmphasis(in); got != in {
			t.Errorf("%s changed:\n got: %q\nwant: %q", name, got, in)
		}
	}
}

// Emphasis inside code belongs to the program, not to the prose: a star in
// a glob or an underscore in a filename must reach the reader as written.
func TestFormatEmphasisSkipsCode(t *testing.T) {
	cases := map[string]string{
		"fenced": "```\n**not bold**\n*not italic*\n```",
		"inline": "run `my_file_test.go` please",
		"glob":   "matched `**/*.go` files",
	}
	for name, in := range cases {
		if got := formatEmphasis(in); got != in {
			t.Errorf("%s changed:\n got: %q\nwant: %q", name, got, in)
		}
	}
}

// The bridge speaks WhatsApp already: a tool line is bold by construction,
// and the vertical table renderer wraps its own label. Neither may be passed
// through the converter, which would turn that bold into italic.
func TestBridgeAuthoredBoldIsNeverConverted(t *testing.T) {
	if got := toolLine("bash", toolArgs(`{"command":"ls"}`)); got != "*bash* `ls`" {
		t.Errorf("toolLine = %q, want its own WhatsApp bold kept", got)
	}
}

// End to end: a header written as markdown bold reaches the vertical layout
// as one bold label, not as stars wrapped around stars.
func TestAVerticalTableLabelIsBoldExactlyOnce(t *testing.T) {
	// Wide enough that renderTable gives up on the grid and lays the rows
	// out vertically, which is the path that wraps its own label in bold.
	// The label has to sit in column two or more: the vertical layout uses
	// column one as the row title and never wraps it.
	in := "| Person | **Role** |\n" +
		"|--------|----------|\n" +
		"| Ada | principal research scientist |\n" +
		"| Grace | senior principal research scientist |\n"
	converted := formatEmphasis(in)
	if strings.Contains(converted, "**Role**") {
		t.Errorf("header should have been converted to single-star bold, got:\n%s", converted)
	}
	out := formatTables(converted)
	if !strings.Contains(out, "*Role*: ") {
		t.Errorf("vertical label lost its bold wrapper, got:\n%s", out)
	}
	if strings.Contains(out, "**Role**") {
		t.Errorf("label was wrapped twice, got:\n%s", out)
	}
}

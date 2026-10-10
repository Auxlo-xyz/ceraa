package instruction

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// readRepoAgentsMD compiles the repository's own instruction file, so the tests
// run against the dialect the project actually writes rather than a fixture that
// agrees with whatever the compiler happens to do today.
func readRepoAgentsMD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The test binary runs in the package directory, so the file is two levels up.
	body, err := os.ReadFile(filepath.Join(wd, "..", "..", "AGENTS.md"))
	if err != nil {
		t.Skipf("the repository instruction file is not readable from here: %v", err)
	}
	return string(body)
}

// texts is a helper for reading the compiled rules of a document without
// repeating the two-call shape at every assertion.
func texts(t *testing.T, body string) []string {
	t.Helper()
	snap, _ := Compile("AGENTS.md", []byte(body))
	out := make([]string, 0, len(snap.Instructions))
	for _, item := range snap.Instructions {
		out = append(out, item.Text)
	}
	return out
}

func rules(t *testing.T, body string) []Instruction {
	t.Helper()
	snap, _ := Compile("AGENTS.md", []byte(body))
	return snap.Instructions
}

func TestAFenceHidesTheRulesInsideIt(t *testing.T) {
	body := "# Style\n\n" +
		"- Go only, no TypeScript\n\n" +
		"```bash\n" +
		"- this is an example, not a rule\n" +
		"- neither is this\n" +
		"```\n\n" +
		"- Async-first coordinator\n"
	got := texts(t, body)
	want := []string{"Go only, no TypeScript", "Async-first coordinator"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fence content reached the rules:\n got %q\nwant %q", got, want)
	}
}

func TestAnUnclosedFenceSwallowsTheTailAndNamesBothLines(t *testing.T) {
	body := "# Style\n\n" +
		"- Go only\n\n" +
		"```bash\n" +
		"- swallowed one\n" +
		"- swallowed two\n"
	snap, diags := Compile("AGENTS.md", []byte(body))
	if got := texts(t, body); len(got) != 1 {
		t.Fatalf("the swallowed tail produced rules: %q", got)
	}
	var found Diagnostic
	for _, item := range diags {
		if item.Kind == DiagUnclosedFence {
			found = item
		}
	}
	if found.Line != 5 {
		t.Errorf("the fence opening line is %d, want 5: %s", found.Line, found)
	}
	if found.EndLine != 7 {
		t.Errorf("the end of the swallowed tail is %d, want 7: %s", found.EndLine, found)
	}
	if !strings.Contains(found.Message, "never closed") {
		t.Errorf("the finding does not say what went wrong: %s", found)
	}
	if snap.Len() != 1 {
		t.Errorf("snapshot has %d rules, want the one before the fence", snap.Len())
	}
}

func TestATableIsReferenceDataAndNotARule(t *testing.T) {
	body := "# Palette\n\n" +
		"| Token | Hex |\n" +
		"|---|---|\n" +
		"| `--ground` | `#FFFFFF` |\n" +
		"| `--fault` | `#FF0000` |\n\n" +
		"Use the tokens.\n"
	snap, diags := Compile("AGENTS.md", []byte(body))
	if snap.Len() != 1 || snap.Instructions[0].Text != "Use the tokens." {
		t.Fatalf("table rows leaked into the rules: %+v", texts(t, body))
	}
	if len(diags) != 0 {
		t.Fatalf("a well-formed table reported %v", diags)
	}
}

func TestAnUnterminatedTableSwallowsTheTailAndNamesBothLines(t *testing.T) {
	body := "# Palette\n\n" +
		"| Token | Hex |\n" +
		"| `--ground` | `#FFFFFF` |\n" +
		"| `--fault` | `#FF0000` |\n"
	got := texts(t, body)
	if len(got) != 0 {
		t.Fatalf("rows with no delimiter were delivered as rules: %q", got)
	}
	_, diags := Compile("AGENTS.md", []byte(body))
	var found Diagnostic
	for _, item := range diags {
		if item.Kind == DiagUnterminatedTable {
			found = item
		}
	}
	if found.Line != 3 || found.EndLine != 5 {
		t.Fatalf("the finding names %d-%d, want 3-5: %s", found.Line, found.EndLine, found)
	}
}

func TestAnHtmlCommentIsSkippedAcrossLines(t *testing.T) {
	body := "- kept one\n" +
		"<!-- a note to self\n" +
		"- not a rule at all\n" +
		"still a comment -->\n" +
		"- kept two\n"
	got := texts(t, body)
	want := []string{"kept one", "kept two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comment content reached the rules:\n got %q\nwant %q", got, want)
	}
}

func TestAnUnclosedCommentSwallowsTheTailAndNamesBothLines(t *testing.T) {
	body := "- kept\n<!-- opened here\n- swallowed\n"
	_, diags := Compile("AGENTS.md", []byte(body))
	var found Diagnostic
	for _, item := range diags {
		if item.Kind == DiagUnclosedComment {
			found = item
		}
	}
	if found.Line != 2 || found.EndLine != 3 {
		t.Fatalf("the finding names %d-%d, want 2-3: %s", found.Line, found.EndLine, found)
	}
}

func TestABlockquoteIsStrippedAndReclassified(t *testing.T) {
	body := "> - never delete the pid file\n\n" +
		"> Two checks, not one, and both are load-bearing.\n\n" +
		"- plain rule\n"
	got := rules(t, body)
	if len(got) != 3 {
		t.Fatalf("want three rules, got %d: %q", len(got), texts(t, body))
	}
	if got[0].Priority != Mandatory {
		t.Errorf("a quoted directive read as %q, want it classified as a bullet", got[0].Priority)
	}
	if strings.Contains(got[1].Text, ">") {
		t.Errorf("the quote marker survived into the rule: %q", got[1].Text)
	}
	if got[1].Priority != Explanatory {
		t.Errorf("a quoted paragraph read as %q, want explanatory", got[1].Priority)
	}
}

func TestHorizontalRulesAndHeadingsProduceNothing(t *testing.T) {
	body := "# Title\n\n---\n\n### Section\n\n- the only rule\n\n***\n"
	got := texts(t, body)
	want := []string{"the only rule"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structure leaked into the rules:\n got %q\nwant %q", got, want)
	}
}

func TestAContinuationLineJoinsItsBullet(t *testing.T) {
	body := "- Canonical source lives at `/srv/app`. It was moved out of\n" +
		"  `/srv/other` so the two are never confused.\n" +
		"- A separate rule.\n"
	got := texts(t, body)
	want := []string{
		"Canonical source lives at `/srv/app`. It was moved out of `/srv/other` so the two are never confused.",
		"A separate rule.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("continuation handling:\n got %q\nwant %q", got, want)
	}
}

func TestCarriageReturnsDoNotChangeWhatCompiles(t *testing.T) {
	unix := "# Style\n\n- Go only, no TypeScript\n"
	windows := strings.ReplaceAll(unix, "\n", "\r\n")
	a, _ := Compile("AGENTS.md", []byte(unix))
	b, _ := Compile("AGENTS.md", []byte(windows))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("a CRLF file compiled differently:\n unix=%+v\nwin=%+v", a.Instructions, b.Instructions)
	}
}

func TestAnEmptyFileCompilesToNothingWithoutError(t *testing.T) {
	for _, body := range []string{"", "\n", "   \n\n\t\n", "\r\n\r\n"} {
		snap, diags := Compile("AGENTS.md", []byte(body))
		if snap.Len() != 0 {
			t.Errorf("%q compiled to %d rules", body, snap.Len())
		}
		if len(diags) != 0 {
			t.Errorf("%q reported %v", body, diags)
		}
		if snap.Digest == "" {
			t.Errorf("%q produced no digest", body)
		}
	}
}

func TestCompilingTheSameBytesTwiceProducesTheSameValue(t *testing.T) {
	body := readRepoAgentsMD(t)
	first, firstDiags := Compile("AGENTS.md", []byte(body))
	for i := 0; i < 5; i++ {
		again, againDiags := Compile("AGENTS.md", []byte(body))
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("compile %d differs from the first", i)
		}
		if !reflect.DeepEqual(firstDiags, againDiags) {
			t.Fatalf("diagnostics on pass %d differ from the first", i)
		}
	}
}

func TestARuleCarriesTheLineAndTheSectionItCameFrom(t *testing.T) {
	body := "# Build\n\n- go build\n\n## Release\n\n- scripts/release.sh is the only release path\n"
	got := rules(t, body)
	if len(got) != 2 {
		t.Fatalf("want two rules, got %d", len(got))
	}
	if got[0].Line != 3 || got[0].EndLine != 3 {
		t.Errorf("first rule is at %d-%d, want 3-3", got[0].Line, got[0].EndLine)
	}
	if got[1].Section != "Build > Release" {
		t.Errorf("section is %q, want %q", got[1].Section, "Build > Release")
	}
}

func TestAParagraphAndABulletDefaultToDifferentPriorities(t *testing.T) {
	body := "- A preference about formatting\n" +
		"- Never commit a binary\n" +
		"\n" +
		"The daemon is supervised by a watcher.\n\n" +
		"Always run the gate before a release.\n"
	got := rules(t, body)
	want := []Priority{Advisory, Mandatory, Explanatory, Advisory}
	if len(got) != len(want) {
		t.Fatalf("want %d rules, got %d: %q", len(want), len(got), texts(t, body))
	}
	for i, item := range got {
		if item.Priority != want[i] {
			t.Errorf("rule %d (%q) is %q, want %q", i, item.Text, item.Priority, want[i])
		}
	}
}

func TestATagOverridesTheDefaultAndIsStrippedFromTheText(t *testing.T) {
	body := "- A preference about formatting [mandatory]\n" +
		"- Watch the pid slot [applies=harness/*] [tools=write,patch]\n" +
		"- Only after a failure [triggers=error]\n"
	got := rules(t, body)
	if len(got) != 3 {
		t.Fatalf("want three rules, got %d", len(got))
	}
	if got[0].Priority != Mandatory {
		t.Errorf("the tag did not set the priority: %q", got[0].Priority)
	}
	if strings.Contains(got[0].Text, "[") {
		t.Errorf("the tag survived into the delivered text: %q", got[0].Text)
	}
	if got[1].Applies != "harness/*" {
		t.Errorf("applies is %q, want harness/*", got[1].Applies)
	}
	if !reflect.DeepEqual(got[1].Triggers, []string{"tool:write", "tool:patch"}) {
		t.Errorf("tools landed as %q", got[1].Triggers)
	}
	if !reflect.DeepEqual(got[2].Triggers, []string{"error"}) {
		t.Errorf("triggers landed as %q", got[2].Triggers)
	}
}

func TestABracketedSpanThatIsNotATagSurvivesIntoTheRule(t *testing.T) {
	body := "- Read the [release checklist] before tagging\n"
	got := rules(t, body)
	if len(got) != 1 || !strings.Contains(got[0].Text, "[release checklist]") {
		t.Fatalf("an ordinary bracketed span was eaten: %q", texts(t, body))
	}
}

func TestALongRuleIsTruncatedAndMarkedOversized(t *testing.T) {
	long := strings.Repeat("the gate must pass ", 60)
	got := rules(t, "- "+long+"\n")
	if len(got) != 1 {
		t.Fatalf("want one rule, got %d", len(got))
	}
	if !got[0].Oversized {
		t.Fatal("a rule past the cap was not marked oversized")
	}
	if len(got[0].Text) > MaxInstructionBytes {
		t.Fatalf("the rule is %d bytes, over the %d cap", len(got[0].Text), MaxInstructionBytes)
	}
	if !strings.HasPrefix(long, got[0].Text) {
		t.Fatal("the truncation is not a prefix of the rule, so the text was altered rather than cut")
	}
}

func TestAScopeThatMatchesEverythingIsLinted(t *testing.T) {
	_, diags := Compile("AGENTS.md", []byte("- Run the gate [applies=main]\n"))
	if !diags.Has(DiagBroadScope) {
		t.Fatalf("a scope with no separator and no dot was not linted: %v", diags)
	}
	_, scoped := Compile("AGENTS.md", []byte("- Run the gate [applies=harness/*]\n"))
	if scoped.Has(DiagBroadScope) {
		t.Fatalf("a scoped rule was linted as broad: %v", scoped)
	}
}

func TestAVolumeAboveTheAlarmIsReported(t *testing.T) {
	var b strings.Builder
	for i := 0; i <= VolumeWarn; i++ {
		b.WriteString("- rule number ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	_, diags := Compile("AGENTS.md", []byte(b.String()))
	if !diags.Has(DiagVolume) {
		t.Fatalf("%d rules did not raise the volume alarm", VolumeWarn+1)
	}
}

func TestDiagnosticsCarryTheirSourceAndRenderOneLineEach(t *testing.T) {
	_, diags := Compile("AGENTS.md", []byte("```\n- swallowed\n"))
	if len(diags) == 0 {
		t.Fatal("no diagnostics")
	}
	if diags.Error() == "" {
		t.Fatal("Diagnostics.Error is empty for a non-empty set")
	}
	for _, item := range diags {
		if item.Source != "AGENTS.md" {
			t.Errorf("diagnostic names source %q", item.Source)
		}
		if strings.Contains(item.String(), "\n") {
			t.Errorf("a diagnostic renders as more than one line: %q", item.String())
		}
	}
}

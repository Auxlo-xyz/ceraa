package instruction

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stubMatcher is the host matcher for these tests.
//
// It is deliberately not a glob implementation. The whole point of the Matcher
// seam is that this package holds no glob logic, and a test that brought its own
// real globber would be testing a matcher that no production caller has.
func stubMatcher(applies, target string) bool {
	if applies == "*" || applies == target {
		return true
	}
	// A relative pattern is compared against each trailing run of the target's
	// segments, which is the part of memory.Brief.matches that makes "src/*"
	// reach an absolute path at all.
	segments := strings.Split(target, "/")
	for i := 1; i < len(segments); i++ {
		suffix := strings.Join(segments[i:], "/")
		if ok, err := filepath.Match(applies, suffix); err == nil && ok {
			return true
		}
	}
	return false
}

func compile(t *testing.T, body string) Snapshot {
	t.Helper()
	snap, _ := Compile("AGENTS.md", []byte(body))
	return snap
}

func TestOnlyTheRulesThatConcernTheOperationAreSelected(t *testing.T) {
	snap := compile(t, "- Inbox rules [applies=harness/inbox/*]\n"+
		"- Release rules [applies=scripts/*]\n"+
		"- A standing preference about naming\n")
	got := Select(snap, Trigger{Tool: "write", Targets: []string{"/srv/harness/inbox/inbox.go"}}, NewSeen(), Options{Matcher: stubMatcher})
	// The unscoped rule fires too: it applies everywhere, and it is what the Seen
	// set keeps from arriving on every subsequent call.
	if len(got.Rules) != 2 {
		t.Fatalf("want the scoped rule and the general one, got %d: %+v", len(got.Rules), got.Rules)
	}
	if !strings.Contains(got.Rules[0].Text, "Inbox rules") {
		t.Fatalf("the wrong rule came first: %+v", got.Rules[0])
	}
	for _, item := range got.Rules {
		if strings.Contains(item.Text, "Release rules") {
			t.Fatal("a rule scoped to another directory was selected")
		}
	}
	if got.Matched != 2 {
		t.Errorf("Matched is %d, want 2", got.Matched)
	}
}

func TestAScopedRuleIsWithheldRatherThanGuessedWhenNoMatcherIsSupplied(t *testing.T) {
	snap := compile(t, "- Inbox rules [applies=harness/inbox/*]\n- A standing preference about naming\n")
	got := Select(snap, Trigger{Tool: "write", Targets: []string{"/srv/harness/inbox/inbox.go"}}, NewSeen(), Options{})
	if len(got.Rules) != 1 || !strings.Contains(got.Rules[0].Text, "standing preference") {
		t.Fatalf("a scoped rule was delivered with no way to check its scope: %+v", got.Rules)
	}
}

func TestMandatoryRulesAreDeliveredBeforeAdvisoryOnes(t *testing.T) {
	snap := compile(t, "- A preference about naming\n"+
		"- Never truncate the session store\n"+
		"\nA paragraph of context about the store.\n"+
		"\n- Always run the local gate\n")
	got := Select(snap, Trigger{Targets: []string{"/srv/anything"}}, NewSeen(), Options{Matcher: stubMatcher})
	want := []Priority{Mandatory, Mandatory, Advisory, Explanatory}
	if len(got.Rules) != len(want) {
		t.Fatalf("want %d rules, got %d", len(want), len(got.Rules))
	}
	for i, item := range got.Rules {
		if item.Priority != want[i] {
			t.Errorf("rule %d is %q, want %q", i, item.Priority, want[i])
		}
	}
}

func TestAMandatoryRuleSurvivesAByteBudgetThatDropsAdvisoryOnes(t *testing.T) {
	var b strings.Builder
	b.WriteString("- Never truncate the session store\n")
	for i := 0; i < 20; i++ {
		b.WriteString("- Some context that is long enough to matter ")
		b.WriteString(strings.Repeat("x", 60))
		b.WriteString("\n")
	}
	snap := compile(t, b.String())
	got := Select(snap, Trigger{Targets: []string{"/srv/anything"}}, NewSeen(), Options{Matcher: stubMatcher, Limit: 100})
	if len(got.Rules) == 0 || got.Rules[0].Priority != Mandatory {
		t.Fatalf("the mandatory rule was dropped first: %+v", got.Rules)
	}
	if got.Withheld == 0 {
		t.Fatal("nothing was withheld, so the budget was not applied at all")
	}
}

func TestABroadRuleCannotFloodANote(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("- A standing preference number ")
		b.WriteString(strings.Repeat("y", i%11+1))
		b.WriteString("\n")
	}
	snap := compile(t, b.String())
	got := Select(snap, Trigger{Tool: "read", Targets: []string{"/srv/anything"}}, NewSeen(), Options{Matcher: stubMatcher})
	if len(got.Rules) > SelectLimit {
		t.Fatalf("selected %d rules, over the limit of %d", len(got.Rules), SelectLimit)
	}
	note := Annotate(Trigger{Tool: "read", Targets: []string{"/srv/anything"}}, got)
	if len(note) > NoteBudget {
		t.Fatalf("the note is %d bytes, over the %d budget", len(note), NoteBudget)
	}
	if !strings.Contains(note, "more not shown") {
		t.Fatalf("a truncated list did not announce its own bound:\n%s", note)
	}
	// Every line in the note is a whole rule: a rule cut to fit would read to
	// the model as if it were complete.
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, "- ") && !strings.HasSuffix(line, ")") && !strings.Contains(line, "more not shown") {
			t.Fatalf("a line was cut rather than dropped whole: %q", line)
		}
	}
}

func TestAnErrorTriggerFiresOnlyOnAFailedOperation(t *testing.T) {
	snap := compile(t, "- Re-read the session file after a failure [triggers=error]\n- Read the file before every write\n")
	ok := Select(snap, Trigger{Targets: []string{"/srv/x"}}, NewSeen(), Options{Matcher: stubMatcher})
	bad := Select(snap, Trigger{Targets: []string{"/srv/x"}, Failed: true}, NewSeen(), Options{Matcher: stubMatcher})
	if len(ok.Rules) != 1 || strings.Contains(ok.Rules[0].Text, "after a failure") {
		t.Fatalf("the error rule fired on a successful operation: %+v", ok.Rules)
	}
	if len(bad.Rules) != 2 {
		t.Fatalf("want both rules on a failure, got %d", len(bad.Rules))
	}
}

func TestAToolTriggerFiresOnlyForThatTool(t *testing.T) {
	snap := compile(t, "- Patch the file rather than rewriting it [tools=patch]\n- Read before every write\n")
	write := Select(snap, Trigger{Tool: "write", Targets: []string{"/srv/x"}}, NewSeen(), Options{Matcher: stubMatcher})
	patch := Select(snap, Trigger{Tool: "patch", Targets: []string{"/srv/x"}}, NewSeen(), Options{Matcher: stubMatcher})
	if len(write.Rules) != 1 || strings.Contains(write.Rules[0].Text, "Patch the file") {
		t.Fatalf("a tool rule fired for the wrong tool: %+v", write.Rules)
	}
	if len(patch.Rules) != 2 {
		t.Fatalf("want both rules for patch, got %d", len(patch.Rules))
	}
}

func TestAnUnrecognisedTriggerWithholdsTheRule(t *testing.T) {
	snap := compile(t, "- Only during a release [triggers=vibes]\n")
	got := Select(snap, Trigger{Targets: []string{"/srv/x"}}, NewSeen(), Options{Matcher: stubMatcher})
	if len(got.Rules) != 0 {
		t.Fatalf("a rule whose trigger cannot be evaluated was delivered: %+v", got.Rules)
	}
}

func TestASeenSetSuppressesASecondDeliveryAndResetsOnDemand(t *testing.T) {
	snap := compile(t, "- Prefer tab-free diffs\n- A standing preference about naming\n")
	seen := NewSeen()
	event := Trigger{Targets: []string{"/srv/anything"}}

	first := Select(snap, event, seen, Options{Matcher: stubMatcher})
	if len(first.Rules) != 2 {
		t.Fatalf("want two rules on the first call, got %d", len(first.Rules))
	}
	seen.Mark(ids(first.Rules)...)

	second := Select(snap, event, seen, Options{Matcher: stubMatcher})
	if len(second.Rules) != 0 {
		t.Fatalf("a rule was delivered twice in one run: %+v", second.Rules)
	}
	if second.AlreadyDelivered != 2 {
		t.Errorf("AlreadyDelivered is %d, want 2", second.AlreadyDelivered)
	}

	seen.Reset()
	afterReset := Select(snap, event, seen, Options{Matcher: stubMatcher})
	if len(afterReset.Rules) != 2 {
		t.Fatalf("Reset did not make the rules eligible again: %+v", afterReset.Rules)
	}
}

func TestCarryUnionsADeliveredSetWithoutLosingWhatItAlreadyHad(t *testing.T) {
	first := NewSeen()
	first.Mark("ione")
	second := NewSeen()
	second.Mark("itwo")
	first.Carry(second)
	if !first.Delivered("ione") || !first.Delivered("itwo") {
		t.Fatalf("Carry dropped a remembered delivery: %+v", first.delivered)
	}
	if first.Len() != 2 {
		t.Fatalf("Carry produced %d ids, want 2", first.Len())
	}
}

func TestANilSeenSetIsHarmless(t *testing.T) {
	var seen *Seen
	seen.Mark("i1")
	if seen.Delivered("i1") {
		t.Error("a nil set reported a delivery")
	}
	seen.Carry(NewSeen())
	seen.Reset()
	if seen.Len() != 0 {
		t.Error("a nil set reported a length")
	}
}

func TestTheNoteSaysWhatItIsAndWhatItIsNot(t *testing.T) {
	snap := compile(t, "- Never truncate the session store\n")
	got := Select(snap, Trigger{Tool: "write", Targets: []string{"/srv/sessions/store.go"}}, NewSeen(), Options{Matcher: stubMatcher})
	note := Annotate(Trigger{Tool: "write", Targets: []string{"/srv/sessions/store.go"}}, got)
	for _, want := range []string{
		"<project_instructions",
		"MUST: Never truncate the session store",
		"(AGENTS.md:1)",
		"not",
		"enforcement",
		"</project_instructions>",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("the note is missing %q:\n%s", want, note)
		}
	}
	if !strings.HasPrefix(note, "\n\n") {
		t.Errorf("the note does not start on a fresh line, so it would be glued to the tool output: %q", note)
	}
}

func TestAnnotatingNothingProducesNoFrame(t *testing.T) {
	if note := Annotate(Trigger{}, Selection{}); note != "" {
		t.Fatalf("an empty selection rendered a frame: %q", note)
	}
}

func ids(items []Instruction) []ID {
	out := make([]ID, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

func TestSelectionCountsWhatItWithheld(t *testing.T) {
	snap := compile(t, "- One standing preference about naming\n- Two standing preferences about naming\n")
	got := Select(snap, Trigger{Targets: []string{"/srv/x"}}, NewSeen(), Options{Matcher: stubMatcher, Limit: 1})
	if len(got.Rules) != 1 || got.Withheld != 1 || got.Matched != 2 {
		t.Fatalf("counts are wrong: %+v", got)
	}
}

func TestTheStoreMergesSourcesInDeliveryOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- Project rules\n")
	writeFile(t, dir, "memory/AGENTS.md", "- Workspace rules\n")
	store := NewStore(dir, "AGENTS.md", "memory/AGENTS.md")
	snap := store.Refresh()
	if len(snap.Instructions) != 2 {
		t.Fatalf("want two rules, got %d", len(snap.Instructions))
	}
	if snap.Instructions[0].Source != "AGENTS.md" || snap.Instructions[1].Source != "memory/AGENTS.md" {
		t.Fatalf("sources are out of delivery order: %+v", snap.Instructions)
	}
}

func TestDuplicateSourcesAreCompiledOnce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "AGENTS.md", "- One rule\n")
	store := NewStore(dir, "AGENTS.md", "AGENTS.md", " AGENTS.md ")
	snap := store.Refresh()
	if len(snap.Instructions) != 1 {
		t.Fatalf("a repeated source was compiled %d times", len(snap.Instructions))
	}
	if got := store.Sources(); !reflect.DeepEqual(got, []string{"AGENTS.md"}) {
		t.Fatalf("sources are %q", got)
	}
}

func TestAMandatoryRuleIsDeliveredOnEveryMatchingOperation(t *testing.T) {
	snap := compile(t, "- Never truncate the session store\n- A standing preference about naming\n")
	seen := NewSeen()
	event := Trigger{Tool: "read", Targets: []string{"/srv/anything"}}

	first := Select(snap, event, seen, Options{Matcher: stubMatcher})
	seen.Mark(ids(first.Rules)...)

	second := Select(snap, event, seen, Options{Matcher: stubMatcher})
	if len(second.Rules) != 1 || second.Rules[0].Priority != Mandatory {
		t.Fatalf("a mandatory rule was suppressed by the delivered set: %+v", second.Rules)
	}
	if !seen.Delivered(first.Rules[0].ID) {
		t.Fatal("the mandatory rule was not marked, so the caller's record is wrong")
	}
}

func TestANoteThatCannotHoldEveryMandatoryRuleSaysSo(t *testing.T) {
	var b strings.Builder
	for i := 0; i < SelectLimit*3; i++ {
		b.WriteString("- Never do the thing number ")
		b.WriteString(strings.Repeat("z", i%13+1))
		b.WriteString("\n")
	}
	snap := compile(t, b.String())
	event := Trigger{Tool: "read", Targets: []string{"/srv/anything"}}
	got := Select(snap, event, NewSeen(), Options{Matcher: stubMatcher, Limit: SelectLimit * 3})
	note := Annotate(event, got)
	if len(note) > NoteBudget {
		t.Fatalf("the note is %d bytes, over the %d budget", len(note), NoteBudget)
	}
	if !strings.Contains(note, "more not shown") {
		t.Fatalf("a note that dropped rules did not say so:\n%s", note)
	}
}

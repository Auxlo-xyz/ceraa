package instruction

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// idOf compiles a document and returns the id of the rule carrying a phrase, so
// each stability test states its own document and names its own rule rather than
// indexing into a result that shifts when segmentation changes.
func idOf(t *testing.T, body, phrase string) ID {
	t.Helper()
	for _, item := range rules(t, body) {
		if strings.Contains(item.Text, phrase) {
			return item.ID
		}
	}
	t.Fatalf("no rule contains %q in:\n%s", phrase, body)
	return ""
}

func TestARuleKeepsItsIDWhenItIsRewrapped(t *testing.T) {
	one := "- Never print a remote URL: the embedded credential lands in the session. [applies=harness/]\n"
	two := "- Never print a remote URL: the embedded\n" +
		"  credential lands in the\n" +
		"  session. [applies=harness/]\n"
	if idOf(t, one, "Never print") != idOf(t, two, "Never print") {
		t.Fatal("rewrapping a rule changed its id")
	}
}

func TestARuleKeepsItsIDWhenTheHeadingAroundItIsRenamed(t *testing.T) {
	before := "# Adding a tool\n\n- A tool must be reachable twice\n"
	after := "# Registering a tool\n\n- A tool must be reachable twice\n"
	if idOf(t, before, "reachable twice") != idOf(t, after, "reachable twice") {
		t.Fatal("renaming a heading changed a rule id")
	}
}

func TestARuleKeepsItsIDWhenItMovesToAnotherHeading(t *testing.T) {
	before := "# Adding a tool\n\n- A tool must be reachable twice\n"
	after := "# Verifying a finding\n\n### Nested\n\n- A tool must be reachable twice\n"
	if idOf(t, before, "reachable twice") != idOf(t, after, "reachable twice") {
		t.Fatal("moving a rule to another section changed its id")
	}
}

func TestARuleKeepsItsIDWhenRulesAreAddedAboveIt(t *testing.T) {
	before := "# Build\n\n- Ship with scripts/build.sh\n"
	after := "# Module\n\n- A new rule at the top\n" +
		"- Another new rule\n\n# Build\n\n- Go 1.27, single binary\n" +
		"- A new rule inside the section\n\n- Ship with scripts/build.sh\n"
	if idOf(t, before, "Ship with") != idOf(t, after, "Ship with") {
		t.Fatal("adding rules above it changed its id")
	}
}

func TestARuleKeepsItsIDWhenTheDocumentIsReordered(t *testing.T) {
	before := "# Style\n\n- One rule about formatting\n- Another rule about naming\n"
	after := "# Style\n\n- Another rule about naming\n- One rule about formatting\n"
	if idOf(t, before, "about formatting") != idOf(t, after, "about formatting") {
		t.Fatal("reordering the document changed a rule id")
	}
}

func TestARuleKeepsItsIDWhenItsScopeIsWidened(t *testing.T) {
	narrow := "- Push after every fix [mandatory] [applies=harness/inbox/*]\n"
	wide := "- Push after every fix [mandatory] [applies=harness/*]\n"
	unscoped := "- Push after every fix [mandatory]\n"
	if idOf(t, narrow, "Push after") != idOf(t, wide, "Push after") {
		t.Fatal("widening a rule's scope changed its id")
	}
	if idOf(t, narrow, "Push after") != idOf(t, unscoped, "Push after") {
		t.Fatal("removing a rule's scope changed its id")
	}
}

func TestARuleKeepsItsIDWhenItComesFromAnotherFile(t *testing.T) {
	first, _ := Compile("AGENTS.md", []byte("- Push after every fix\n"))
	second, _ := Compile("memory/AGENTS.md", []byte("- Push after every fix\n"))
	if first.Instructions[0].ID != second.Instructions[0].ID {
		t.Fatal("the same sentence in two files has two ids")
	}
	if first.Instructions[0].Source == second.Instructions[0].Source {
		t.Fatal("both rules claim the same source, so the source is not being carried")
	}
}

func TestARuleChangesItsIDOnlyWhenItsOwnWordsChange(t *testing.T) {
	before := "- Push after every fix, without asking\n"
	after := "- Push after every fix, without being asked\n"
	if idOf(t, before, "Push after") == idOf(t, after, "Push after") {
		t.Fatal("changing a rule's words did not change its id")
	}
}

func TestTheIDAndTheHashCannotDisagree(t *testing.T) {
	body := readRepoAgentsMD(t)
	snap, _ := Compile("AGENTS.md", []byte(body))
	if snap.Len() == 0 {
		t.Fatal("the repository instruction file compiled to nothing")
	}
	for _, item := range snap.Instructions {
		// Recomputed here rather than trusting either field: an id and a hash
		// computed by two different code paths would both look well formed and
		// disagree about every rule in the project.
		sum := sha256.Sum256([]byte(item.Text))
		want := hex.EncodeToString(sum[:])
		if item.Hash != want {
			t.Fatalf("hash %q does not describe the text of rule %s", item.Hash, item.ID)
		}
		base := "i" + want[:24]
		if item.DuplicateOf == "" {
			if string(item.ID) != base {
				t.Fatalf("id %q is not the digest of its text (%q)", item.ID, base)
			}
			if len(item.ID) != 25 {
				t.Fatalf("id %q is %d characters, want 25", item.ID, len(item.ID))
			}
			continue
		}
		if string(item.DuplicateOf) != base {
			t.Fatalf("a repeat points at %q, which is not its own digest", item.DuplicateOf)
		}
		if !strings.HasPrefix(string(item.ID), base) {
			t.Fatalf("a repeat id %q is not its own digest plus a suffix", item.ID)
		}
		if suffix := strings.TrimPrefix(string(item.ID), base); suffix == "" {
			t.Fatal("a repeat id carries no suffix, so it collides with the original")
		}
	}
}

func TestTheIDIsTheDigestOfTheDeliveredTextNotOfTheDocument(t *testing.T) {
	body := "- Run the gate [applies=harness/*]\n"
	got := rules(t, body)
	sum := sha256.Sum256([]byte("Run the gate"))
	if got[0].Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("the hash was taken over something other than the delivered text: %q", got[0].Hash)
	}
}

func TestTwoIdenticalRulesKeepTheFirstIDAndTheRestAreSuffixed(t *testing.T) {
	body := "# One\n\n- Never delete the pid file\n" +
		"\n# Two\n\n- Never delete the pid file\n" +
		"\n# Three\n\n- Never delete the pid file\n"
	got := rules(t, body)
	if len(got) != 3 {
		t.Fatalf("three copies compiled to %d rules; a repeat is a fact about the document", len(got))
	}
	if got[0].DuplicateOf != "" {
		t.Errorf("the first copy claims to be a repeat of %q", got[0].DuplicateOf)
	}
	for i, item := range got[1:] {
		if item.DuplicateOf != got[0].ID {
			t.Errorf("copy %d points at %q, want the first copy %q", i+1, item.DuplicateOf, got[0].ID)
		}
		want := string(got[0].ID) + strconv.Itoa(i+1)
		if string(item.ID) != want {
			t.Errorf("copy %d has id %q, want %q", i+1, item.ID, want)
		}
	}

	_, diags := Compile("AGENTS.md", []byte(body))
	reports := 0
	for _, item := range diags {
		if item.Kind != DiagDuplicate {
			continue
		}
		reports++
		if item.Source != "AGENTS.md" {
			t.Errorf("the duplicate finding names source %q", item.Source)
		}
		if item.Line != 3 {
			t.Errorf("the duplicate finding starts at line %d, want the first copy at 3", item.Line)
		}
		if !strings.Contains(item.Message, "3 times") {
			t.Errorf("the duplicate finding does not say how many copies: %s", item.Message)
		}
	}
	if reports != 1 {
		t.Fatalf("three copies produced %d findings, want one", reports)
	}
}

func TestAddingASecondCopyDoesNotMoveTheFirstCopyID(t *testing.T) {
	before := "- Never delete the pid file\n- Never truncate the session store\n"
	after := "# Somewhere else\n\n- Never delete the pid file\n\n" + before
	if idOf(t, before, "pid file") != idOf(t, after, "pid file") {
		t.Fatal("adding an identical copy elsewhere moved the original's id")
	}
	if idOf(t, before, "session store") != idOf(t, after, "session store") {
		t.Fatal("adding an identical copy elsewhere moved an unrelated id")
	}
}

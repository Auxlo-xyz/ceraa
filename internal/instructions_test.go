package internal

import (
	"os"
	"path/filepath"
	"testing"

	"ceraa/harness/instruction"
)

// The loader has to produce rules from a real file, not just compile.
//
// A store's constructor reads nothing on purpose and Snapshot answers with the
// last good compile, so a loader that called Snapshot without Refresh first would
// return an empty set forever - which is indistinguishable from a project with no
// AGENTS.md, and would ship as a working feature that never delivers anything.
func TestTheLoaderReadsARealInstructionFile(t *testing.T) {
	root := t.TempDir()
	body := "## Outbound network\n\n- Every network tool goes through harness/netguard. [applies=harness/**]\n"
	if err := os.WriteFile(filepath.Join(root, instruction.DefaultFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	load := InstructionRules(root)
	snapshot := load()

	if len(snapshot.Instructions) == 0 {
		t.Fatal("the loader compiled nothing from a file that exists, so nothing would ever be delivered")
	}
	var found bool
	for _, rule := range snapshot.Instructions {
		if rule.Text == "Every network tool goes through harness/netguard." {
			found = true
			if rule.Applies != "harness/**" {
				t.Errorf("applies = %q, want the tag the file declared", rule.Applies)
			}
			if rule.Source != instruction.DefaultFile {
				t.Errorf("source = %q, want the project-relative file", rule.Source)
			}
		}
	}
	if !found {
		t.Errorf("the rule the file declares is not in the snapshot: %#v", snapshot.Instructions)
	}
}

// A project with no instruction file yields nothing, and that is a state rather
// than an error: every caller wires this unconditionally, so a missing AGENTS.md
// must not become a run failure.
func TestAProjectWithNoInstructionFileYieldsNothingWithoutFailing(t *testing.T) {
	load := InstructionRules(t.TempDir())

	if snapshot := load(); len(snapshot.Instructions) != 0 {
		t.Errorf("a project with no AGENTS.md compiled %d rules", len(snapshot.Instructions))
	}
}

// The loader is called once per run by the runner, so a second call in the same
// run must not be a second compile of a changed file. What it must do is give
// the same answer twice for an unchanged file, which is what makes the per-run
// boundary observable at all.
func TestAnUnchangedFileGivesTheSameSnapshotTwice(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, instruction.DefaultFile),
		[]byte("- Keep one rule. [applies=notes.txt]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	load := InstructionRules(root)

	first, second := load(), load()
	if len(first.Instructions) != len(second.Instructions) {
		t.Fatalf("an unchanged file gave %d then %d rules", len(first.Instructions), len(second.Instructions))
	}
}

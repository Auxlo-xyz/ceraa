package compaction

import (
	"fmt"
	"strings"
	"testing"
)

// A fold was refused twice on 2026-10-12 with `the summary is missing the
// "Handoff" section`, each time after paying for a ~210k-token summarisation.
// These pin the prompt and the repair instruction that answer that.

func TestTheTemplateSaysEveryHeadingIsRequired(t *testing.T) {
	if !strings.Contains(summaryTemplate, "Every heading below is required") {
		t.Error("the template does not state that the headings are required, so a summariser that drops one is not told it failed")
	}
	if !strings.Contains(summaryTemplate, "rejected whole") {
		t.Error("the template does not say what happens to a summary that misses a heading")
	}
}

// The preamble added to the template must not be read as a heading by
// requiredSummaryHeadings, which scrapes "## " lines out of the template itself.
// A sentence there would become a tenth required section that no model can fill.
func TestTheTemplatePreambleAddsNoHeading(t *testing.T) {
	for _, heading := range requiredSummaryHeadings() {
		if words := len(strings.Fields(heading)); words > 4 {
			t.Errorf("required heading %q is %d words, which reads as prose scraped as a heading", heading, words)
		}
	}
}

// The repair instruction has to name the heading. Asking a model to work out
// which of nine it got wrong is a second chance to get it wrong, and the
// validator already knows.
func TestTheRepairInstructionNamesTheHeadingThatWasMissing(t *testing.T) {
	failure := summaryMissing("Handoff")
	if failure == nil {
		t.Fatal("the fixture is a valid summary, so this test proves nothing")
	}
	instruction := compactionInstruction(2, failure)
	if !strings.Contains(instruction, `"Handoff"`) {
		t.Errorf("the repair instruction does not name the missing heading: %q", instruction)
	}
	if !strings.Contains(instruction, "rejected and thrown away") {
		t.Error("the repair instruction does not say the previous reply was refused")
	}
}

// The first attempt must not claim anything was rejected. There was nothing
// rejected, and a model told it had already failed writes defensively.
func TestTheFirstAttemptCarriesNoRepairNote(t *testing.T) {
	first := compactionInstruction(1, nil)
	// Matched on the repair note's own opening, not on "rejected": the template
	// legitimately uses that word to say what happens to a summary missing a
	// heading, and grepping for it here would match the template, not the note.
	if strings.Contains(first, "Your previous reply") {
		t.Errorf("the first attempt is carrying a repair note: %q", first)
	}
}

// A failure the validator could not describe must still produce an instruction
// that repairs something, rather than an empty sentence.
func TestTheRepairInstructionFallsBackWhenThereIsNoReason(t *testing.T) {
	instruction := compactionInstruction(2, nil)
	if !strings.Contains(instruction, "not in the required format") {
		t.Errorf("no reason given and no fallback either: %q", instruction)
	}
}

// summaryMissing builds a summary that satisfies every rule except one heading,
// so the error the repair instruction carries is the validator's own rather than
// one written by hand. The section bodies are long enough to clear
// minSummaryChars, so the heading is the only thing wrong with it.
func summaryMissing(heading string) error {
	var b strings.Builder
	for _, h := range requiredSummaryHeadings() {
		if strings.EqualFold(h, heading) {
			continue
		}
		fmt.Fprintf(&b, "## %s\nSomething durable worth carrying forward in this section.\n\n", h)
	}
	return validateSummary(b.String())
}

// The template must still yield exactly the sections it always did, or a stored
// summary written under the old prompt would fail validation under the new one.
func TestTheHeadingsAreUnchangedByHardening(t *testing.T) {
	got := requiredSummaryHeadings()
	if len(got) != 9 {
		t.Fatalf("the template now yields %d headings, want 9: %v", len(got), got)
	}
	if got[0] != "Objective" || got[8] != "Design Lessons Learnt" {
		t.Errorf("the first and last headings moved: %q ... %q", got[0], got[8])
	}
}

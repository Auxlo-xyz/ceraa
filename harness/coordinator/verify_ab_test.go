package coordinator

import (
	"encoding/json"
	"strings"
	"testing"
)

// Proves the two arms actually differ, and that a declined call is recorded as a
// failure rather than silently passing as "no trial".
func TestVerifyABArmsDiffer(t *testing.T) {
	v := abVariants()
	if len(v) != 2 {
		t.Fatalf("arms = %d, want 2", len(v))
	}
	bothBytes, mergedBytes := abDefinitionBytes(v[0].reg), abDefinitionBytes(v[1].reg)
	t.Logf("both=%d bytes (~%d tok)  merged=%d bytes (~%d tok)  saving=%d bytes (~%d tok/turn)",
		bothBytes, bothBytes/4, mergedBytes, mergedBytes/4, bothBytes-mergedBytes, (bothBytes-mergedBytes)/4)
	if mergedBytes >= bothBytes {
		t.Error("the merged arm is not smaller, so it would not save anything")
	}
	if _, found := v[1].reg.Get("edit_line"); found {
		t.Error("edit_line is still registered in the merged arm")
	}
	if _, found := v[0].reg.Get("edit_line"); !found {
		t.Error("edit_line is missing from the both arm")
	}
	// The single-change wrapper cost that the merge imposes.
	one := `{"path":"a.py","edits":[{"old_text":"DEBUG = False","new_text":"DEBUG = True"}]}`
	t.Logf("one-element batch call: %d bytes; flat edit_line call would be: %d bytes", len(one), len(one)-len(`"edits":[],`)+0)
}

// Proves a prose answer and a declined call both score as failures, because a
// merge that made the model answer in prose instead of calling a tool would
// otherwise look like a pass.
func TestVerifyABScoringIsFailClosed(t *testing.T) {
	arm := &abArm{name: "merged"}
	arm.add("value", abCall{parseOK: true, translateOK: true, appliedOK: true, fileOK: true})
	arm.add("value", abCall{parseOK: true, translateOK: true, appliedOK: true, fileOK: true, prose: true})
	arm.add("value", abCall{parseOK: true, translateOK: false})
	arm.add("value", abCall{toolName: "batch_edit_file", parseOK: false})
	t.Logf("ok=%d/%d = %.0f%%", arm.ok, arm.totals, arm.successRate())
	if arm.ok != 1 {
		t.Errorf("ok = %d, want 1; prose, untranslated and unparsed calls must all fail", arm.ok)
	}
	if arm.successRate() != 25 {
		t.Errorf("rate = %.0f%%, want 25%%", arm.successRate())
	}
}

// Proves usage harvesting reads the Messages shape from both carrying events.
func TestVerifyABUsageHarvest(t *testing.T) {
	c := abCall{}
	abHarvestUsage(map[string]any{"message": map[string]any{"usage": map[string]any{"input_tokens": float64(1234), "output_tokens": float64(12)}}}, &c)
	abHarvestUsage(map[string]any{"usage": map[string]any{"output_tokens": float64(40)}}, &c)
	b, _ := json.Marshal(c)
	if c.inputTok != 1234 || c.outputTok != 40 {
		t.Errorf("usage = %d in / %d out, want 1234 / 40", c.inputTok, c.outputTok)
	}
	t.Logf("harvested %s", b)
	// A zero-token event must not clobber a real count.
	abHarvestUsage(map[string]any{"usage": map[string]any{"output_tokens": float64(0)}}, &c)
	if c.outputTok != 40 {
		t.Errorf("output = %d, want the 40 already seen", c.outputTok)
	}
}

// Proves every task is well formed, so a failure is the model's and not a typo in
// the fixture.
func TestVerifyABTasksAreWellFormed(t *testing.T) {
	for _, task := range abTasks() {
		if task.name == "" || task.content == "" || task.prompt == "" || task.want == "" {
			t.Errorf("incomplete task %+v", task)
		}
		if strings.Contains(task.prompt, "```") {
			t.Errorf("%s: the prompt must not contain a fence, it is wrapped in one", task.name)
		}
		t.Logf("%-8s %-30q -> %q", task.name, strings.SplitN(task.prompt, ", ", 2)[1], task.want)
	}
}

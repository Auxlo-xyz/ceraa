package coordinator

import (
	"strings"
	"testing"
)

func TestLooksLikeRefusalCatchesTheOpeningForms(t *testing.T) {
	refusals := []string{
		"I can't do that.",
		"i cannot access the network from this environment.",
		"I can\u2019t verify that without more context.",
		"I'm unable to read that file.",
		"I am not able to run that command.",
		"I won't be able to complete this.",
		"I genuinely can't do that.",
		"I honestly can't do that.",
		"I can\u2019t help with that request.",
		"I can't help you do that.",
		"I really cannot verify that.",
		"I'm honestly not able to reach it.",
	}
	for _, text := range refusals {
		if !looksLikeRefusal(text) {
			t.Errorf("missed refusal: %q", text)
		}
	}
}

func TestLooksLikeRefusalIgnoresEnthusiasmAndLateDoubts(t *testing.T) {
	allowed := []string{
		"I can't wait to try this.",
		"I cannot believe the tests passed.",
		"I genuinely can't wait to try this.",
		strings.Repeat("step ", 26) + "I can't do the last one.",
	}
	for _, text := range allowed {
		if looksLikeRefusal(text) {
			t.Errorf("flagged a non-refusal: %q", text)
		}
	}
}

func TestRefusalGateWithholdsThenReleasesAPlainAnswer(t *testing.T) {
	var out strings.Builder
	g := newRefusalGate(func(text string) { out.WriteString(text) })
	g.hold()
	for _, delta := range strings.Split("Here is how to do it: open the file and change the value.", " ") {
		if g.observe(delta + " ") {
			t.Fatal("a normal answer was treated as a refusal")
		}
	}
	if out.Len() != 0 {
		t.Fatalf("gate leaked %d bytes while still inside the window: %q", out.Len(), out.String())
	}
	g.release()
	if got := out.String(); !strings.Contains(got, "Here is how to do it") {
		t.Fatalf("released text = %q", got)
	}
}

func TestRefusalGateDiscardsARefusalAndEmitsNothing(t *testing.T) {
	var out strings.Builder
	g := newRefusalGate(func(text string) { out.WriteString(text) })
	g.hold()
	if g.observe("I ") {
		t.Fatal("the first fragment decided early")
	}
	if !g.observe("can't do that without internet access.") {
		t.Fatal("the refusal was not detected")
	}
	// Deltas arriving after the abort must not leak either.
	g.observe(" Let me explain.")
	g.release()
	if out.Len() != 0 {
		t.Fatalf("refusal reached the renderer: %q", out.String())
	}
	if !g.firedRefusal() {
		t.Fatal("gate did not record the refusal")
	}
}

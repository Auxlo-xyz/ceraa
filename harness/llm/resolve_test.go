package llm

import (
	"testing"
)

// The catalogue lists the paid build, Ceraa's subscription defaults name the
// free one, and the two share a reasoning profile. Both spellings resolve.
func TestNormalizeCatalogModelStripsFreeVariants(t *testing.T) {
	for in, want := range map[string]string{
		"space-bunny-free":             "space-bunny",
		"upstage/solar-pro4:free":      "upstage/solar-pro4",
		"SPACE-BUNNY-FREE":             "SPACE-BUNNY",
		"deepseek/deepseek-v4.1-flash": "deepseek/deepseek-v4.1-flash",
		"deepseek/deepseek-chat":       "deepseek/deepseek-chat",
		"  gpt-5  ":                    "gpt-5",
		"free":                         "free",
	} {
		if got := NormalizeCatalogModel(in); got != want {
			t.Errorf("NormalizeCatalogModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThinkingPassesThroughWhenUnresolved(t *testing.T) {
	tk := Thinking{}
	for _, level := range []ReasonLevel{ReasonOff, ReasonLow, ReasonMedium, ReasonHigh, ReasonMax} {
		if got := tk.Resolved(level); got != level {
			t.Errorf("unresolved thinking rewrote %s to %s", level, got)
		}
	}
}

// The live shape of deepseek-v4.1-flash: max, high and low. Anything else has to
// move, which is what stops a level the model does not list from reaching it.
func TestThinkingClampsToTheDeclaredVocabulary(t *testing.T) {
	tk := Thinking{Known: true, Efforts: []string{"max", "high", "low"}, Default: "high"}
	for level, want := range map[ReasonLevel]ReasonLevel{
		ReasonMedium:  "high",
		ReasonLow:     "low",
		ReasonHigh:    "high",
		ReasonMax:     "max",
		ReasonMinimal: "high",
		ReasonXHigh:   "high",
		ReasonOff:     "high",
	} {
		if got := tk.Resolved(level); got != want {
			t.Errorf("Resolved(%s) = %s, want %s", level, got, want)
		}
	}
	if tk.Allows(ReasonMedium) {
		t.Error("medium was reported as allowed")
	}
	if !tk.Allows(ReasonLow) {
		t.Error("low was reported as not allowed")
	}
}

// Claude declares reasoning with no vocabulary. Guessing one would be narrower
// than the truth, so it is left alone.
func TestThinkingLeavesAVocabularylessModelAlone(t *testing.T) {
	tk := Thinking{Known: true}
	if got := tk.Resolved(ReasonMedium); got != ReasonMedium {
		t.Errorf("a model with no declared vocabulary was rewritten: %s", got)
	}
}

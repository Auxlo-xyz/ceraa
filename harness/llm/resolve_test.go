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

// With no declared vocabulary the levels Ceraa has always sent pass through,
// and the top tier collapses to high: see TestThinkingCollapsesTheTopTierWhenUnresolved.
func TestThinkingPassesThroughWhenUnresolved(t *testing.T) {
	tk := Thinking{}
	for _, level := range []ReasonLevel{ReasonOff, ReasonMinimal, ReasonLow, ReasonMedium, ReasonHigh} {
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
	} {
		if got := tk.Resolved(level); got != want {
			t.Errorf("Resolved(%s) = %s, want %s", level, got, want)
		}
	}
	// "off" means send no reasoning field at all, which is how the provider is
	// left on its own default. It is never clamped into a thinking tier, or a
	// user who asked for none would be billed for the top one.
	if got := tk.Resolved(ReasonOff); got != ReasonOff {
		t.Errorf("Resolved(off) = %s, want off", got)
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

// An unresolved vocabulary keeps the old conservative shape: xhigh and max
// collapse to high, because a tier the model never claimed is a 400.
func TestThinkingCollapsesTheTopTierWhenUnresolved(t *testing.T) {
	tk := Thinking{}
	for level, want := range map[ReasonLevel]ReasonLevel{
		ReasonXHigh: ReasonHigh,
		ReasonMax:   ReasonHigh,
		ReasonHigh:  ReasonHigh,
		ReasonLow:   ReasonLow,
	} {
		if got := tk.Resolved(level); got != want {
			t.Errorf("Resolved(%s) = %s, want %s", level, got, want)
		}
	}
}

// With a declared vocabulary the top tier survives, which is the point.
func TestThinkingKeepsTheDeclaredTopTier(t *testing.T) {
	tk := Thinking{Known: true, Efforts: []string{"max", "xhigh", "high", "low"}, Default: "high"}
	for level, want := range map[ReasonLevel]ReasonLevel{
		ReasonMax:   ReasonMax,
		ReasonXHigh: ReasonXHigh,
		ReasonHigh:  ReasonHigh,
	} {
		if got := tk.Resolved(level); got != want {
			t.Errorf("Resolved(%s) = %s, want %s", level, got, want)
		}
	}
}

// The transport mapping is one to one now, including the top tier.
func TestAnthropicEffortIsOneToOne(t *testing.T) {
	for level, want := range map[ReasonLevel]string{
		ReasonMinimal: "low",
		ReasonLow:     "low",
		ReasonMedium:  "medium",
		ReasonHigh:    "high",
		ReasonXHigh:   "xhigh",
		ReasonMax:     "max",
		ReasonOff:     "",
	} {
		if got := anthropicEffort(level); got != want {
			t.Errorf("anthropicEffort(%s) = %q, want %q", level, got, want)
		}
	}
}

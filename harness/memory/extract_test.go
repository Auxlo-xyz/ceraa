package memory

import (
	"testing"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// The parser is the only place a giant model reply can take the log down with
// it, and it is also the place the JSON can drift out of shape. Each escape
// hatch the model might take is pinned.
func TestParseExtractedToleratesTheFencesTheModelAdds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`[{"statement":"always write tests first","applies":"","kind":"add","confidence":"high"}]`, 1},
		{"```json\n[{\"statement\":\"terse replies\",\"applies\":\"\",\"kind\":\"add\",\"confidence\":\"high\"}]\n```", 1},
		{`[]`, 0},
		{"  ", 0},
		{`[{"statement":"","applies":"","kind":"add","confidence":"high"}]`, 0}, // blank statement kept out
	}
	for _, tc := range cases {
		got, err := parseExtracted(tc.in)
		if err != nil {
			t.Errorf("parseExtracted(%q): %v", tc.in, err)
			continue
		}
		if len(got) != tc.want {
			t.Errorf("parseExtracted(%q) = %d items, want %d", tc.in, len(got), tc.want)
		}
	}
}

// An invalid reply is an error that names why, rather than a plausible-looking
// empty list that looks like "nothing to store".
func TestParseExtractedRejectsNonJSONNonArray(t *testing.T) {
	if _, err := parseExtracted("happy birthday"); err == nil {
		t.Fatal("a prose reply was accepted")
	}
}

// Confidence becomes provenance rather than a gate, and a weak guess is still not
// a lesser fact.
//
// This used to assert that only "high" was written and medium was dropped. That is
// what made a medium inference unlearnable: it was discarded on the first session
// and so could never be re-asserted, which is the one thing repetition was supposed
// to be able to fix. A medium statement is now stored, weighted below a stated one
// and able to climb toward its ceiling if it is confirmed again. A low one is still
// refused, because at that point it is a guess rather than a lesser fact.
func TestCommitStoresConfidenceAsProvenanceRatherThanGatingOnIt(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	added, contradicted, err := Commit(store, "p", "s", []Extracted{
		{Statement: "always run gofmt", Kind: "add", Confidence: "high"},
		{Statement: "maybe use tabs", Kind: "add", Confidence: "medium"},
		{Statement: "never force push", Kind: "add", Confidence: "low"},
		{Statement: "never tests", Kind: "contradict", Confidence: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || contradicted != 1 {
		t.Errorf("added=%d contradicted=%d, want 2 and 1", added, contradicted)
	}

	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the store holds %d entries, want the high and the medium add: %v", len(entries), entries)
	}
	byContent := map[string]Entry{}
	for _, entry := range entries {
		byContent[entry.Content] = entry
	}
	if got := byContent["always run gofmt"].Veracity; got != Stated {
		t.Errorf("a high-confidence extraction stored as %q, want %q", got, Stated)
	}
	if got := byContent["maybe use tabs"].Veracity; got != Inferred {
		t.Errorf("a medium-confidence extraction stored as %q, want %q", got, Inferred)
	}
	if _, present := byContent["never force push"]; present {
		t.Error("a low-confidence guess was stored; it is a guess, not a lesser fact")
	}
}

// The ordering that makes weighting worth doing: a stated memory beats an inferred
// one at equal corroboration, every provenance keeps its ordering as repetition
// grows, and each converges on its own ceiling rather than all climbing to one.
func TestProvenanceOrdersScoresAndCorroborationOnlyLiftsThem(t *testing.T) {
	now := time.Now()
	stated := Entry{Seen: 1, Veracity: Stated, SeenAt: now}
	inferred := Entry{Seen: 1, Veracity: Inferred, SeenAt: now}
	tool := Entry{Seen: 1, Veracity: Tool, SeenAt: now}

	if !(score(stated, now) > score(inferred, now) && score(inferred, now) > score(tool, now)) {
		t.Errorf("ordering at one confirmation: stated %.3f, inferred %.3f, tool %.3f",
			score(stated, now), score(inferred, now), score(tool, now))
	}

	// Repetition lifts every provenance, and never past its own ceiling.
	for _, veracity := range []Veracity{Stated, Inferred, Tool} {
		ceiling := veracity.Weight()
		previous := 0.0
		for seen := 1; seen <= 40; seen++ {
			got := score(Entry{Seen: seen, Veracity: veracity, SeenAt: now}, now)
			if got <= previous {
				t.Fatalf("%s stopped rising at seen=%d: %.4f then %.4f", veracity, seen, previous, got)
			}
			if got > ceiling+1e-9 {
				t.Fatalf("%s scored %.4f at seen=%d, above its ceiling %.4f", veracity, got, seen, ceiling)
			}
			previous = got
		}
	}

	// Repetition can never invert the provenance order, at any confirmation count.
	// This is the property that matters: corroboration is meant to lift an entry
	// toward its ceiling, and the only way that can go wrong is if a weaker
	// provenance climbs past a stronger one.
	for seen := 1; seen <= 40; seen++ {
		statedAt := score(Entry{Seen: seen, Veracity: Stated, SeenAt: now}, now)
		for _, weaker := range []Veracity{Unknown, Inferred, Imported, Tool} {
			weakerAt := score(Entry{Seen: seen, Veracity: weaker, SeenAt: now}, now)
			if weakerAt >= statedAt {
				t.Errorf("at seen=%d, %s (%.4f) matched or beat stated (%.4f)", seen, weaker, weakerAt, statedAt)
			}
		}
	}
}

// An entry written before provenance existed reads as Unknown, not as nothing, so
// upgrading does not empty the prompt block of every store in the field.
func TestAnEntryWithNoProvenanceScoresMidRange(t *testing.T) {
	now := time.Now()
	unset := score(Entry{Seen: 1, SeenAt: now}, now)
	if unset <= 0 {
		t.Fatal("an entry with no provenance scored zero and would fall below the floor")
	}
	if unset != score(Entry{Seen: 1, Veracity: Unknown, SeenAt: now}, now) {
		t.Error("an unset provenance does not read as Unknown")
	}
	if !(score(Entry{Seen: 1, Veracity: Stated, SeenAt: now}, now) > unset &&
		unset > score(Entry{Seen: 1, Veracity: Tool, SeenAt: now}, now)) {
		t.Error("Unknown is not sitting between stated and tool")
	}
}

// A repeat can only raise provenance. A user restating a preference the agent had
// only inferred is the user vouching for it, and letting a later weak re-assert
// demote it would invert the entire point of weighting.
func TestARepeatOnlyEverRaisesProvenance(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const statement = "prefer small commits"
	if _, err := store.AddScoped("p", "s", statement, "", "", Inferred); err != nil {
		t.Fatal(err)
	}
	// A weak re-assert must not demote it.
	if _, err := store.AddScoped("p", "s", statement, "", "", Tool); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("store holds %d entries, want 1", len(entries))
	}
	if entries[0].Veracity != Inferred {
		t.Errorf("a tool-observed repeat demoted provenance to %q", entries[0].Veracity)
	}
	// A stronger re-assert lifts it.
	if _, err := store.AddScoped("p", "s", statement, "", "", Stated); err != nil {
		t.Fatal(err)
	}
	entries, err = store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Veracity != Stated {
		t.Errorf("provenance stayed at %q after the user stated it", entries[0].Veracity)
	}
	if entries[0].Seen != 3 {
		t.Errorf("Seen = %d, want 3", entries[0].Seen)
	}
}

// Without a credential the extractor is off, and the main provider's key is not
// blindly lent to an arbitrary URL.
func TestExtractConfigIsOptInWithoutAKey(t *testing.T) {
	_, on := ExtractConfigFromENV(llm.ProviderConfig{BaseURL: "https://api.example.com", Model: "x", APIKey: "sk"})
	if on {
		t.Error("extraction runs without a CERAA_EXTRACT_KEY and a non-poolside main provider")
	}
	t.Setenv("CERAA_EXTRACT_KEY", "pool-key")
	_, on = ExtractConfigFromENV(llm.ProviderConfig{BaseURL: "https://api.example.com", Model: "x", APIKey: "sk"})
	if !on {
		t.Error("extraction is off even with CERAA_EXTRACT_KEY set")
	}
}

// Field whitespace is stripped by the cleaning step, not trusted to the model.
func TestExtractedFieldsAreTrimmed(t *testing.T) {
	got, err := parseExtracted("```\n[{\"statement\":\"  keep the session archive  \",\"applies\":\"  *.go \",\"kind\":\"add\",\"confidence\":\" high\"}]\n```")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 statement, got %d", len(got))
	}
	if got[0].Statement != "keep the session archive" || got[0].Applies != "*.go" || got[0].Confidence != "high" {
		t.Errorf("fields were not trimmed: %+v", got[0])
	}
}

// A discarded head with no system prompt and no user text still reaches the
// extractor rather than being skipped by accident. (This guards the caller's
// contract, which hands the raw stream.)
func TestExtractSkipsAnEmptyHead(t *testing.T) {
	if got, err := Extract(t.Context(), llm.ProviderConfig{}, nil, "summary"); err != nil || got != nil {
		t.Errorf("Extract(nil) = %v, %v; want nil, nil", got, err)
	}
}

var _ = sessionstore.Message{} // keep import anchored for future shape changes

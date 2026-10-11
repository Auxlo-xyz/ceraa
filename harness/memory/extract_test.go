package memory

import (
	"path/filepath"
	"strings"
	"testing"

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
// a lesser fact: high reads as stated, medium as inferred, and low is refused,
// because at that point it is a guess rather than a lesser fact.
func TestVeracityForConfidenceMapsHighAndMediumAndRefusesLow(t *testing.T) {
	if got := VeracityForConfidence("high"); got != Stated {
		t.Errorf("high = %q, want %q", got, Stated)
	}
	if got := VeracityForConfidence("medium"); got != Inferred {
		t.Errorf("medium = %q, want %q", got, Inferred)
	}
	if got := VeracityForConfidence("low"); got != "" {
		t.Errorf("low = %q, want refused", got)
	}
}

// Without a credential the learner is off, and the main provider's key is not
// blindly lent to an arbitrary URL.
func TestExtractConfigIsOptInWithoutAKey(t *testing.T) {
	_, on := ExtractConfigFromENV(llm.ProviderConfig{BaseURL: "https://api.example.com", Model: "x", APIKey: "sk"})
	if on {
		t.Error("extraction runs without a CERAA_EXTRACT_KEY and a main provider on a different host")
	}
	t.Setenv("CERAA_EXTRACT_KEY", "test-key")
	_, on = ExtractConfigFromENV(llm.ProviderConfig{BaseURL: "https://api.example.com", Model: "x", APIKey: "sk"})
	if !on {
		t.Error("extraction is off even with CERAA_EXTRACT_KEY set")
	}
}

// The learner follows the same opt-in: no key, no hook, and compaction still
// runs without the memory-mining step.
func TestMakeLearnerIsOffWithoutAKey(t *testing.T) {
	t.Setenv("CERAA_EXTRACT_KEY", "")
	log := OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))
	learn, on := MakeLearner(llm.ProviderConfig{BaseURL: "https://api.example.com", Model: "x", APIKey: "sk"}, log)
	if on || learn != nil {
		t.Error("MakeLearner returned a hook with no extraction credential configured")
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

// The log gets the durable-person subset of an extraction: profile facts,
// preferences, live goals and standing constraints. Task state and project
// context stay in the turn ledger and the checkpoint summary instead, because
// the log is a durable picture of the user rather than another copy of the work.
func TestAppendPersonalMemoriesCarriesOnlyTheDurablePerson(t *testing.T) {
	log := OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))
	added := AppendPersonalMemories(log, "owner", []Extracted{
		{Statement: "Emperor M.K prefers direct answers", Type: "preference", Kind: "add", Confidence: "high"},
		{Statement: "Emperor M.K wants to ship the new landing page", Type: "goal", Kind: "add", Confidence: "medium"},
		{Statement: "Emperor M.K never works on Sundays", Type: "constraint", Kind: "add", Confidence: "high"},
		{Statement: "Emperor M.K is a backend engineer", Type: "profile", Kind: "add", Confidence: "medium"},
		{Statement: "Fixed the login redirect", Type: "decision", Kind: "add", Confidence: "high"},
		{Statement: "The API paginates at fifty rows", Type: "context", Kind: "add", Confidence: "high"},
		{Statement: "Maybe use tabs", Type: "preference", Kind: "add", Confidence: "low"},
		{Statement: "Prefers direct answers", Type: "observation", Kind: "confirm", Confidence: "high"},
	})
	if added != 4 {
		t.Fatalf("AppendPersonalMemories = %d, want the profile, preference, goal and constraint", added)
	}

	entries := log.Tail(10)
	if len(entries) != 1 {
		t.Fatalf("log holds %d entries, want one compact user-memory entry", len(entries))
	}
	for _, want := range []string{
		"Durable user memory:",
		"Profile: Emperor M.K is a backend engineer",
		"Preference: Emperor M.K prefers direct answers",
		"Goal: Emperor M.K wants to ship the new landing page",
		"Constraint: Emperor M.K never works on Sundays",
	} {
		if !strings.Contains(entries[0], want) {
			t.Errorf("log entry = %q, want it to contain %q", entries[0], want)
		}
	}
	for _, avoid := range []string{"Fixed the login redirect", "fifty rows", "Maybe use tabs"} {
		if strings.Contains(entries[0], avoid) {
			t.Errorf("log entry = %q, want it to exclude task-scoped material %q", entries[0], avoid)
		}
	}

	// The log has no rewrite operation, so the same extraction must not become a
	// second permanent copy.
	if again := AppendPersonalMemories(log, "owner", []Extracted{
		{Statement: "Emperor M.K prefers direct answers", Type: "preference", Kind: "add", Confidence: "high"},
	}); again != 0 {
		t.Errorf("second identical write added %d, want no duplicate", again)
	}
	if got := log.Lines(); got != 1 {
		t.Errorf("log holds %d entries, want the original compact entry only", got)
	}
	if got := AppendPersonalMemories(nil, "owner", []Extracted{
		{Statement: "Emperor M.K prefers direct answers", Type: "preference", Kind: "add", Confidence: "high"},
	}); got != 0 {
		t.Errorf("missing log returned %d, want a no-op rather than a failure", got)
	}
}

// A compaction can produce more durable claims than one note should carry. The
// cap sheds the tail by selection order rather than by cutting a sentence, and a
// sentence that is withheld remains eligible rather than being retired unseen.
func TestAppendPersonalMemoriesIsBounded(t *testing.T) {
	log := OpenMemoryFile(filepath.Join(t.TempDir(), "Memory.md"))
	statements := make([]Extracted, 0, personalMemoryMaxStatements+4)
	for i := 0; i < personalMemoryMaxStatements+4; i++ {
		statements = append(statements, Extracted{
			Statement:  strings.Repeat("durable preference ", 8) + strings.Repeat("x", i),
			Type:       "preference",
			Kind:       "add",
			Confidence: "high",
		})
	}
	if got := AppendPersonalMemories(log, "owner", statements); got != personalMemoryMaxStatements {
		t.Fatalf("AppendPersonalMemories = %d, want the per-compaction cap %d", got, personalMemoryMaxStatements)
	}
	entry := log.Tail(1)[0]
	if got := len([]rune(entry)); got > personalMemoryMaxEntryRunes {
		t.Fatalf("user-memory entry is %d runes, want at most %d", got, personalMemoryMaxEntryRunes)
	}
}

// The extractor's defaults must name a provider that holds still. Poolside
// laguna returned three different documents across runs of the same prompt on
// the same transcript, so its output could not be committed to memory and
// audited later; the extraction path needs a repeatable answer.
func TestExtractorDefaultProviderIsRepeatable(t *testing.T) {
	if strings.Contains(DefaultExtractBaseURL, "poolside") || strings.Contains(DefaultExtractModel, "laguna") {
		t.Fatalf("defaults still point at the provider that sampled a different answer each run: %s %s",
			DefaultExtractBaseURL, DefaultExtractModel)
	}
	if !strings.HasPrefix(DefaultExtractBaseURL, "https://") {
		t.Errorf("extract base URL is not HTTPS: %s", DefaultExtractBaseURL)
	}
}

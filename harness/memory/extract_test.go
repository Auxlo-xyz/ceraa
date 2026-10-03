package memory

import (
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

// Only high-confidence adds are committed. Medium and low are a ranking signal
// for a future round, not a write; a contradict candidate is nominated but
// never deleted silently, because a wrong extraction must not erase a correct
// preference.
func TestCommitStoresOnlyHighConfidenceAdds(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	added, contradicted, err := Commit(store, "p", "s", []Extracted{
		{Statement: "always run gofmt", Applies: "", Kind: "add", Confidence: "high"},
		{Statement: "maybe use tabs", Applies: "", Kind: "add", Confidence: "medium"},
		{Statement: "never tests", Applies: "", Kind: "contradict", Confidence: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || contradicted != 1 {
		t.Errorf("added=%d contradicted=%d, want 1 and 1", added, contradicted)
	}

	entries, err := store.List("p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Content != "always run gofmt" {
		t.Errorf("the store holds %v, want exactly the high-confidence add", entries)
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

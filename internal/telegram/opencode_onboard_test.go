package telegram

import (
	"testing"

	"ceraa/harness/llm"
)

// OpenCode onboarding is a key prompt and nothing else.
//
// The defect it guards: the flow started at the "model" stage, so the first
// thing a user saw was "Send the model ID". Zen serves 84 models and the only
// surface that can show them is the Model menu, so asking for one here asked for
// a value the user had no way to know and no list to choose from. The stage is
// the entire behaviour: "key" means the model is skipped, and the key handler
// fills the default in when it finds none.
func TestOpenCodeOnboardingStartsAtTheKeyStage(t *testing.T) {
	draft := openCodeDraft()
	if draft.Stage != "key" {
		t.Errorf("Stage = %q, want %q; a user would be asked for a model id they cannot see a catalog for",
			draft.Stage, "key")
	}
	if draft.ID != llm.OpenCodeSubscription {
		t.Errorf("ID = %q, want %q", draft.ID, llm.OpenCodeSubscription)
	}
	if draft.URL != llm.OpenCodeBaseURL {
		t.Errorf("URL = %q, want %q; the base URL is filled in so no path can be mistyped",
			draft.URL, llm.OpenCodeBaseURL)
	}
}

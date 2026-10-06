package coordinator

import (
	"strings"
	"testing"

	"ceraa/harness/llm"
)

// The cache key was the same string for every session and every run, so all of
// them shared one provider-side bucket. A measured run showed the same
// 3584-token hit repeated across requests whose conversation grew from 3876 to
// 13736 tokens, then alternation between 8704 and 14848 - other runs' entries,
// not this one. The key has to separate sessions or the hit rate is noise.
func TestTheCacheKeyIsScopedToTheSession(t *testing.T) {
	model := llm.ProviderConfig{BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-6-luna"}
	a := sessionPromptCacheKey(model, "tg-1")
	b := sessionPromptCacheKey(model, "tg-2")
	if a == b {
		t.Error("two sessions share a cache key, so their prefixes collide in the provider's bucket")
	}
	if !strings.HasPrefix(a, promptCacheKey(model)) {
		t.Error("the session key no longer identifies the endpoint and model")
	}
	// Stable within a session, or nothing is ever reused.
	if a != sessionPromptCacheKey(model, "tg-1") {
		t.Error("the key is not stable within one session")
	}
}

// A different endpoint or model must not share a bucket either.
func TestTheCacheKeySeparatesEndpointsAndModels(t *testing.T) {
	a := llm.ProviderConfig{BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-6-luna"}
	b := llm.ProviderConfig{BaseURL: "https://api.atria-asi.ai/v1", Model: "Atria-Dawn-Preview"}
	if sessionPromptCacheKey(a, "s") == sessionPromptCacheKey(b, "s") {
		t.Error("two providers share a cache key")
	}
	if sessionPromptCacheKey(a, "s") == sessionPromptCacheKey(a, "s2") {
		t.Error("the session is not part of the key")
	}
}

// An absent session must still produce a usable key rather than an empty one,
// because the CLI and any caller without a session would otherwise send nothing.
func TestTheCacheKeySurvivesAnAbsentSession(t *testing.T) {
	model := llm.ProviderConfig{BaseURL: "https://x.example/v1", Model: "m"}
	got := sessionPromptCacheKey(model, "")
	if got == "" || !strings.HasPrefix(got, "ceraa-") {
		t.Errorf("key = %q, want a ceraa- prefixed fallback", got)
	}
}

// The key has to fit the length the provider accepts, otherwise it returns 200
// and quietly buys nothing. The digests were originally written in full, which
// produced an 83-character key: 26 over the limit. A key that long is not an
// error, it is a key that does nothing, so the length is pinned here.
func TestTheCacheKeyFitsTheProviderLengthLimit(t *testing.T) {
	model := llm.ProviderConfig{BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-6-luna"}
	for _, sessionID := range []string{"", "tg-1", "a-very-long-session-identifier-0123456789"} {
		key := sessionPromptCacheKey(model, sessionID)
		if n := len([]rune(key)); n > 64 {
			t.Errorf("key for session %q is %d chars, over the 64 the provider accepts: %q", sessionID, n, key)
		}
	}
}

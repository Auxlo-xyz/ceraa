package opencodeauth

import (
	"context"
	"testing"

	"ceraa/harness/llm"
)

// initStoreForTest walks the same secret-store setup the rest of the suite
// does. It is here rather than shared because the free grant is about an EMPTY
// store, and every helper in opencodeauth_test.go begins by storing one.
func initEmptyStoreForTest(t *testing.T) {
	t.Helper()
	initSecret(t)
	if err := secretSet(secretName, ""); err != nil {
		t.Fatal(err)
	}
}

// No key anywhere resolves to the public tier, not to an error: the keyless
// grant is what the CLI itself serves on, it bills nothing, and asking every
// user for a key before the first working request is a toll a free tier does
// not charge. The grant is derived (never stored), so an existing blob keeps
// decoding the same way and a key added later simply stops deriving it.
func TestCredentialForFallsBackToThePublicTier(t *testing.T) {
	initEmptyStoreForTest(t)

	grant, err := CredentialFor(context.Background(), "")
	if err != nil {
		t.Fatalf("the keyless fallback returned an error: %v", err)
	}
	if !grant.FreeTier {
		t.Errorf("FreeTier = false, want true: an absent key is the public tier")
	}
	if grant.APIKey != PublicAPIKey {
		t.Errorf("APIKey = %q, want %q: an empty key would send no Authorization header at all", grant.APIKey, PublicAPIKey)
	}
	if grant.ProjectID != "" {
		t.Errorf("ProjectID = %q, want none: there is no account to belong to a workspace", grant.ProjectID)
	}
}

// The public bearer is refused on the keyed route, so a stored profile URL
// naming /zen/v1 (mostly a keyed profile whose key was removed) would send a
// guaranteed refusal. The free grant is authoritative about its route instead.
func TestApplyCredentialRoutesFreeTierToThePublicBase(t *testing.T) {
	initEmptyStoreForTest(t)

	grant, _ := CredentialFor(context.Background(), "")
	model := ApplyCredential(llm.ProviderConfig{
		BaseURL: llm.OpenCodeBaseURL, APIKey: "carried-over",
	}, grant, "ses-ceraa-free")

	if model.APIKey != PublicAPIKey {
		t.Errorf("APIKey = %q, want the public bearer, not a carried-over key", model.APIKey)
	}
	if model.BaseURL != llm.OpenCodePublicBaseURL {
		t.Errorf("BaseURL = %q, want %q; the keyed root would refuse this bearer",
			model.BaseURL, llm.OpenCodePublicBaseURL)
	}
	if model.Model != llm.DefaultOpenCodePublicModel {
		t.Errorf("Model = %q, want %q: a free session needs a model the whitelist serves",
			model.Model, llm.DefaultOpenCodePublicModel)
	}
	if model.MaxRetries < OpenCodeFreeRetries {
		t.Errorf("MaxRetries = %d, want at least %d: one first-200 admission never opens without it",
			model.MaxRetries, OpenCodeFreeRetries)
	}
	if !model.OpenCodeGateway {
		t.Errorf("OpenCodeGateway = false; the body contract is part of the free tier too")
	}
	if !HasOpenCodeHeaders(model.ExtraHeaders) {
		t.Errorf("the header contract has to travel on the free route: %v", model.ExtraHeaders)
	}
}

// A free grant replaces an empty model id, but leaves a stored one alone when
// it is on the whitelist: a user's model pick is not a bug to correct.
func TestApplyCredentialKeepsAFreeWhitelistModel(t *testing.T) {
	initEmptyStoreForTest(t)

	grant, _ := CredentialFor(context.Background(), "")
	model := ApplyCredential(llm.ProviderConfig{
		BaseURL: llm.OpenCodePublicBaseURL, Model: llm.OpenCodePublicFreeModels[1],
	}, grant, "ses-ceraa-free-2")

	if model.Model != llm.OpenCodePublicFreeModels[1] {
		t.Errorf("Model = %q, want the stored whitelist id", model.Model)
	}
}

// The default model must be one the whitelist actually serves, and the default
// must be the one measured answering live: free and served are different
// claims, and the constant carrying the measured one is what "default" means.
func TestDefaultPublicModelServes(t *testing.T) {
	if !llm.IsOpenCodePublicFreeModel(llm.DefaultOpenCodePublicModel) {
		t.Fatalf("DefaultOpenCodePublicModel %q is outside OpenCodePublicFreeModels", llm.DefaultOpenCodePublicModel)
	}
	if llm.DefaultOpenCodePublicModel != "glm-5.3-flash-free" {
		t.Fatalf("DefaultOpenCodePublicModel = %q; only glm-5.3-flash-free was measured answering live",
			llm.DefaultOpenCodePublicModel)
	}
}

// The resolved session shape reaches the public route through the same
// GatewaySessionID, so a keyless session draw encodes the complement exactly
// the same time. This is the assertion that ties the free tier onto the one
// measured session rule: a suffix that stops reading counter 1 breaks the
// public route too.
func TestPublicGrantRendersTheSessionShape(t *testing.T) {
	initEmptyStoreForTest(t)

	grant, _ := CredentialFor(context.Background(), "")
	model := ApplyCredential(llm.ProviderConfig{BaseURL: llm.OpenCodePublicBaseURL}, grant, "ses-ceraa-free-3")
	headers := model.ExtraHeadersFunc("ses-ceraa-free-3")
	if headers == nil {
		t.Fatal("ExtraHeadersFunc is nil")
	}
	session := headers[HeaderSession]
	if session == "" {
		t.Fatal("x-opencode-session missing on the free grant")
	}
	rendered := GatewaySessionID("ses-ceraa-free-3")
	// The measured rule is over the twelve hex characters, which start at
	// string index 4: hex[9]/hex[10] are string indices 13/14, the second and
	// third of the complement field the GatewaySessionID forces to 0xFFE.
	if rendered[13] != 'f' || rendered[14] != 'f' {
		t.Errorf("session %s: hex[9:11] = %q (%q), both must be 'f' to pass",
			rendered, rendered[13:15], rendered[4:16])
	}
}

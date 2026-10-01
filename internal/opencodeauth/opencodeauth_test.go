package opencodeauth

import (
	"context"
	"path/filepath"
	"testing"

	"ceraa/harness/llm"
	"ceraa/internal/build"
)

func initSecret(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := secretInit(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
}

// The identity is the whole reason this package has an opinion. Pinned exactly,
// and pinned both ways: a prefix check passed a wrong id once in clineauth, and
// the wrong value here would be a request claiming to be a different program.
func TestClientIDIsCeraa(t *testing.T) {
	if ClientID != "ceraa" {
		t.Errorf("ClientID = %q, want %q", ClientID, "ceraa")
	}
	if ClientID == "cli" || ClientID == "opencode" {
		t.Errorf("ClientID = %q claims to be the OpenCode CLI; this program is Ceraa", ClientID)
	}
	want := "ceraa/" + build.Version
	if ClientUserAgent != want {
		t.Errorf("ClientUserAgent = %q, want %q", ClientUserAgent, want)
	}
}

func TestRequestHeadersPairSessionAndAffinity(t *testing.T) {
	headers := RequestHeaders("ses-abc", "")
	// request.ts sends the session under both names: one for the trace, one for
	// routing. Sending only one of them is a half-contract.
	if headers[HeaderSession] != "ses-abc" {
		t.Errorf("%s = %q, want ses-abc", HeaderSession, headers[HeaderSession])
	}
	if headers[HeaderAffinity] != "ses-abc" {
		t.Errorf("%s = %q, want ses-abc", HeaderAffinity, headers[HeaderAffinity])
	}
	if headers[HeaderClient] != ClientID {
		t.Errorf("%s = %q, want %q", HeaderClient, headers[HeaderClient], ClientID)
	}
	if headers[HeaderUA] != ClientUserAgent {
		t.Errorf("%s = %q, want %q", HeaderUA, headers[HeaderUA], ClientUserAgent)
	}
	// No project means no header, not an empty one: opencode's own source omits
	// it conditionally and an empty value is worse than an absent one.
	if value, ok := headers[HeaderProject]; ok {
		t.Errorf("%s = %q with no project configured, want the header absent", HeaderProject, value)
	}
}

func TestRequestHeadersSendsProjectOnlyWhenSet(t *testing.T) {
	headers := RequestHeaders("ses-abc", "  ws-9  ")
	if headers[HeaderProject] != "ws-9" {
		t.Errorf("%s = %q, want ws-9 trimmed", HeaderProject, headers[HeaderProject])
	}
}

// A correlation id that repeats correlates nothing. This is the failure the
// static map would have shipped: one id, computed at config build, reused for
// every request the process ever makes.
func TestPerRequestHeadersIssueAFreshRequestID(t *testing.T) {
	builder := perRequestHeaders("", "ses-fallback")
	first := builder("ses-one")
	if first[HeaderRequest] == "" {
		t.Fatal("no x-opencode-request was set")
	}
	seen := map[string]bool{first[HeaderRequest]: true}
	for i := 0; i < 50; i++ {
		id := builder("ses-one")[HeaderRequest]
		if seen[id] {
			t.Fatalf("x-opencode-request repeated after %d calls: %q", i+1, id)
		}
		seen[id] = true
	}
}

// The session the transport resolved for this request is the real one. The
// fallback only covers a caller that had no session at config time.
func TestPerRequestHeadersPreferTheTransportSession(t *testing.T) {
	withSession := perRequestHeaders("", "fallback")("ses-real")
	if withSession[HeaderSession] != "ses-real" {
		t.Errorf("%s = %q, want ses-real", HeaderSession, withSession[HeaderSession])
	}
	without := perRequestHeaders("", "fallback")("")
	if without[HeaderSession] != "fallback" {
		t.Errorf("%s = %q with no session, want the config-time fallback", HeaderSession, without[HeaderSession])
	}
}

func TestHasOpenCodeHeadersRequiresTheWholeContract(t *testing.T) {
	if HasOpenCodeHeaders(nil) {
		t.Error("nil headers reported as complete")
	}
	if HasOpenCodeHeaders(map[string]string{HeaderClient: ClientID, HeaderUA: ClientUserAgent}) {
		t.Error("a set without a session reported as complete")
	}
	if !HasOpenCodeHeaders(RequestHeaders("ses", "")) {
		t.Error("RequestHeaders output not recognised as complete")
	}
}

func TestApplyCredentialConfiguresTheProvider(t *testing.T) {
	model := llm.ProviderConfig{Model: "claude-sonnet-5"}
	got := ApplyCredential(model, Credential{APIKey: "zk-1"}, "ses-cfg")
	if got.BaseURL != llm.OpenCodeBaseURL {
		t.Errorf("BaseURL = %q, want %q", got.BaseURL, llm.OpenCodeBaseURL)
	}
	if got.APIKey != "zk-1" {
		t.Errorf("APIKey = %q, want zk-1", got.APIKey)
	}
	if !HasOpenCodeHeaders(got.ExtraHeaders) {
		t.Errorf("ExtraHeaders = %v, missing the gateway contract", got.ExtraHeaders)
	}
	if got.ExtraHeadersFunc == nil {
		t.Fatal("ExtraHeadersFunc is nil; x-opencode-request would never be issued per request")
	}
	if got.ExtraHeadersFunc("ses-tx")[HeaderRequest] == "" {
		t.Error("ExtraHeadersFunc produced no request id")
	}
	// A profile that set its own contract wins rather than being overwritten.
	own := llm.ProviderConfig{ExtraHeaders: map[string]string{
		HeaderClient: "profile-owned", HeaderUA: "profile-ua", HeaderSession: "profile-ses",
	}}
	if kept := ApplyCredential(own, Credential{APIKey: "zk-1"}, "ses-cfg"); kept.ExtraHeaders[HeaderClient] != "profile-owned" {
		t.Errorf("a profile's own headers were replaced with %q", kept.ExtraHeaders[HeaderClient])
	}
}

// A custom base URL in the profile is honoured, the same way the other
// subscription providers honour one.
func TestApplyCredentialKeepsAProfileURL(t *testing.T) {
	got := ApplyCredential(llm.ProviderConfig{BaseURL: "https://example.invalid/v1"}, Credential{APIKey: "k"}, "")
	if got.BaseURL != "https://example.invalid/v1" {
		t.Errorf("BaseURL = %q, the profile's own URL was discarded", got.BaseURL)
	}
}

func TestNewRequestIDIsThirtyTwoHexChars(t *testing.T) {
	id := NewRequestID()
	if len(id) != 32 {
		t.Errorf("len = %d, want 32", len(id))
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seen[NewRequestID()] = true
	}
	if len(seen) < 199 {
		t.Errorf("%d distinct ids from 200 draws; ids repeat far too often", len(seen))
	}
}

func TestSaveAndLoadTheCredential(t *testing.T) {
	initSecret(t)
	if SignedIn() {
		t.Fatal("SignedIn true with nothing stored")
	}
	if _, err := Load(); err != ErrNoCredential {
		t.Errorf("Load with nothing stored: err = %v, want ErrNoCredential", err)
	}
	// An empty key would store successfully and read back as a credential with
	// no key, which fails on the first request instead of at the point of
	// writing it.
	if err := Save(Credential{ProjectID: "ws-1"}); err == nil {
		t.Error("a credential with no api key was stored")
	}
	if err := Save(Credential{APIKey: "  zk-2  ", ProjectID: " ws-9 "}); err != nil {
		t.Fatal(err)
	}
	if !SignedIn() {
		t.Fatal("SignedIn false after storing a credential")
	}
	credential, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if credential.APIKey != "zk-2" {
		t.Errorf("APIKey = %q, want the stored value trimmed", credential.APIKey)
	}
	if credential.ProjectID != "ws-9" {
		t.Errorf("ProjectID = %q, want ws-9", credential.ProjectID)
	}
	// Round-trips through the encrypted store rather than through a plain field,
	// so a malformed blob must not read as a credential.
	if err := secretSet(secretName, "not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != ErrNoCredential {
		t.Errorf("Load of a malformed record: err = %v, want ErrNoCredential", err)
	}
}

// Zen's chat-completions path is OpenAI-compatible, so the provider must not
// select a native transport. A native transport would send a body shape this
// endpoint does not serve.
func TestZenUsesNoNativeTransport(t *testing.T) {
	got := ApplyCredential(llm.ProviderConfig{Model: "mimo-v2.6-flash-free"}, Credential{APIKey: "k"}, "")
	if got.CodexNative || got.AnthropicNative || got.XaiNative {
		t.Errorf("a native transport was selected for an OpenAI-compatible endpoint: %+v", got)
	}
}

func TestResolveAcceptsAContextLikeTheOAuthProviders(t *testing.T) {
	initSecret(t)
	// Written this way so the call site can be shaped the same as clineauth's and
	// minimaxauth's, which take a context they actually use to refresh.
	if _, err := Resolve(context.Background()); err != ErrNoCredential {
		t.Errorf("Resolve with nothing stored: err = %v, want ErrNoCredential", err)
	}
}

// A hand-written config puts the key in the profile. Requiring it to be moved
// into the encrypted store first would mean the documented config does not work.
func TestCredentialForPrefersTheProfileKey(t *testing.T) {
	initSecret(t)

	// Neither source has a key: the caller is told so rather than handed a
	// credential with an empty key that fails on the first request.
	if _, err := CredentialFor(context.Background(), "  "); err != ErrNoCredential {
		t.Errorf("err = %v, want ErrNoCredential", err)
	}

	// Store only.
	if err := Save(Credential{APIKey: "zk-store", ProjectID: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	fromStore, err := CredentialFor(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if fromStore.APIKey != "zk-store" {
		t.Errorf("APIKey = %q, want the stored one", fromStore.APIKey)
	}
	if fromStore.ProjectID != "ws-1" {
		t.Errorf("ProjectID = %q, the store should still contribute the project", fromStore.ProjectID)
	}

	// Profile wins over the store, but the project id is still account state the
	// profile has nowhere to hold.
	merged, err := CredentialFor(context.Background(), "zk-profile")
	if err != nil {
		t.Fatal(err)
	}
	if merged.APIKey != "zk-profile" {
		t.Errorf("APIKey = %q, want zk-profile from the profile", merged.APIKey)
	}
	if merged.ProjectID != "ws-1" {
		t.Errorf("ProjectID = %q, want ws-1 carried over from the store", merged.ProjectID)
	}

	// Profile key with no store at all still resolves.
	if err := secretSet(secretName, ""); err != nil {
		t.Fatal(err)
	}
	solo, err := CredentialFor(context.Background(), "zk-solo")
	if err != nil {
		t.Fatalf("a profile key with an empty store did not resolve: %v", err)
	}
	if solo.APIKey != "zk-solo" {
		t.Errorf("APIKey = %q, want zk-solo", solo.APIKey)
	}
}

// The credential the profile holds has to survive into the request, or the
// provider signs in and then sends an unauthenticated call.
func TestApplyCredentialSendsTheResolvedKey(t *testing.T) {
	got := ApplyCredential(llm.ProviderConfig{}, Credential{APIKey: "zk-store"}, "")
	if got.APIKey != "zk-store" {
		t.Errorf("APIKey = %q, want zk-store", got.APIKey)
	}
}

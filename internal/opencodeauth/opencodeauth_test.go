package opencodeauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
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
}

// The gateway keys on the User-Agent. First measured 2026-10-01 as a version
// floor (426 below 1.18.0); re-measured 2026-10-14 as a suffix rule: a bare
// "opencode/<version>" with nothing after it is refused FreeTierError at any
// version, while any suffix content passes. The suffix is where this program
// stays honest, naming itself beside the measured release.
//
// The version is parsed rather than compared as a string because "1.9.0" sorts
// before "1.18.0" as text and is above it as a number, and it is 1.9.0 and every
// other such value that the old floor existed to reject.
func TestGatewayUserAgentMeetsTheVersionFloor(t *testing.T) {
	if !strings.HasPrefix(GatewayUserAgent, "opencode/") {
		t.Fatalf("GatewayUserAgent = %q, want the opencode/ prefix; anything else is refused FreeTierError", GatewayUserAgent)
	}
	parts := strings.Split(strings.TrimPrefix(GatewayUserAgent, "opencode/"), " ")
	parts = strings.Split(parts[0], ".")
	if len(parts) < 2 {
		t.Fatalf("GatewayUserAgent = %q carries no major.minor version", GatewayUserAgent)
	}
	major, minor := 0, 0
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		t.Fatalf("GatewayUserAgent = %q: %v", GatewayUserAgent, err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minor); err != nil {
		t.Fatalf("GatewayUserAgent = %q: %v", GatewayUserAgent, err)
	}
	if major < 1 || (major == 1 && minor < 18) {
		t.Errorf("GatewayUserAgent = %q is below the 1.18.0 floor; the gateway answers 426 to it", GatewayUserAgent)
	}
	fields := strings.Fields(GatewayUserAgent)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "ceraa/") {
		t.Errorf("GatewayUserAgent = %q carries no ceraa/ suffix; a bare version is refused FreeTierError", GatewayUserAgent)
	}
}

// x-opencode-session is the other half of what the gateway keys on, and it
// accepts nothing outside opencode's own id grammar: "ses_", twelve lowercase
// hex characters, fourteen base62 characters. Anything else is FreeTierError.
//
// The grammar alone stopped being enough on 2026-10-14: the gateway decodes
// the twelve-bit sequence counter in the hex field and refuses counters 64
// and above however fresh the timestamp, while 0 through 63 pass and the
// timestamp itself is never checked. The id below therefore fixes the low
// twelve bits to 0xFFE (counter 1, what a fresh generator emits first) while
// the timestamp stays a hash of the conversation, keeping the id deterministic
// per conversation.
//
// A conversation maps to one id and keeps it, because that is the pairing the
// real client uses: every request in a conversation carries that conversation's
// session rather than opening a new one per call.
func TestGatewaySessionIDIsOpencodeShapedAndStable(t *testing.T) {
	shape := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	first := GatewaySessionID("ses-ceraa-conversation")
	if !shape.MatchString(first) {
		t.Errorf("GatewaySessionID = %q, want ses_ + 12 lowercase hex + 14 base62", first)
	}
	var field uint64
	if _, err := fmt.Sscanf(first[4:16], "%x", &field); err != nil {
		t.Fatalf("GatewaySessionID = %q: %v", first, err)
	}
	// The generator complements (timestamp << 12 | counter), so the counter
	// the gateway reads back is the complement of the field's low twelve
	// bits. This id fixes those bits to 0xFFE, which reads back as 1.
	if counter := ^field & 0xfff; counter >= 64 {
		t.Errorf("GatewaySessionID = %q carries counter %d; the gateway refuses 64 and above", first, counter)
	}
	if again := GatewaySessionID("ses-ceraa-conversation"); again != first {
		t.Errorf("the same conversation produced two ids: %q then %q", first, again)
	}
	if other := GatewaySessionID("ses-a-different-conversation"); other == first {
		t.Errorf("two conversations shared the gateway session id %q", first)
	}
	// The empty conversation still gets a well-formed id rather than an empty
	// header, because the gateway refuses a request with no session at all.
	if !shape.MatchString(GatewaySessionID("")) {
		t.Errorf("GatewaySessionID(\\\"\\\") = %q, want a well-formed id", GatewaySessionID(""))
	}
}

func TestRequestHeadersPairSessionAndAffinity(t *testing.T) {
	headers := RequestHeaders("ses-abc", "")
	// Two different values on purpose. The gateway reads x-opencode-session and
	// will not accept Ceraa's own conversation id in it, so the conversation is
	// rendered into opencode's shape there and left as itself on the affinity
	// header, where something downstream can resolve it back to this session.
	if headers[HeaderSession] != GatewaySessionID("ses-abc") {
		t.Errorf("%s = %q, want the conversation rendered into opencode's shape", HeaderSession, headers[HeaderSession])
	}
	// The session id repeats the session: either one alone is refused.
	if headers[HeaderSessionID] != headers[HeaderSession] {
		t.Errorf("%s = %q, want the gateway session %q", HeaderSessionID, headers[HeaderSessionID], headers[HeaderSession])
	}
	if headers[HeaderAffinity] != "ses-abc" {
		t.Errorf("%s = %q, want ses-abc, the real conversation", HeaderAffinity, headers[HeaderAffinity])
	}
	if headers[HeaderClient] != ClientID {
		t.Errorf("%s = %q, want %q", HeaderClient, headers[HeaderClient], ClientID)
	}
	if headers[HeaderUA] != GatewayUserAgent {
		t.Errorf("%s = %q, want %q", HeaderUA, headers[HeaderUA], GatewayUserAgent)
	}
	// The two identities are not the same field and must not become the same
	// value: the gateway accepts any client but only accepts this User-Agent, so
	// a header set that put "ceraa" in both would work on one and fail on the
	// other.
	if headers[HeaderUA] == ClientID || headers[HeaderUA] == "ceraa/"+build.Version {
		t.Errorf("%s = %q claims Ceraa on the header the gateway gates on", HeaderUA, headers[HeaderUA])
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
//
// Both headers are checked: affinity carries the conversation verbatim, which is
// what makes the preference observable at all, and the gateway session carries
// the same conversation rendered into opencode's shape.
func TestPerRequestHeadersPreferTheTransportSession(t *testing.T) {
	withSession := perRequestHeaders("", "fallback")("ses-real")
	if withSession[HeaderAffinity] != "ses-real" {
		t.Errorf("%s = %q, want ses-real", HeaderAffinity, withSession[HeaderAffinity])
	}
	if withSession[HeaderSession] != GatewaySessionID("ses-real") {
		t.Errorf("%s = %q, want the rendered id for ses-real", HeaderSession, withSession[HeaderSession])
	}
	without := perRequestHeaders("", "fallback")("")
	if without[HeaderAffinity] != "fallback" {
		t.Errorf("%s = %q with no session, want the config-time fallback", HeaderAffinity, without[HeaderAffinity])
	}
}

func TestHasOpenCodeHeadersRequiresTheWholeContract(t *testing.T) {
	if HasOpenCodeHeaders(nil) {
		t.Error("nil headers reported as complete")
	}
	if HasOpenCodeHeaders(map[string]string{HeaderClient: ClientID, HeaderUA: GatewayUserAgent}) {
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
		HeaderSessionID: "profile-ses-id",
	}}
	if kept := ApplyCredential(own, Credential{APIKey: "zk-1"}, "ses-cfg"); kept.ExtraHeaders[HeaderClient] != "profile-owned" {
		t.Errorf("a profile's own headers were replaced with %q", kept.ExtraHeaders[HeaderClient])
	}
	// A partial contract is completed rather than kept: three of the four
	// headers pass the old check but the gateway refuses either session
	// header alone, so keeping them would send a failing request.
	partial := llm.ProviderConfig{ExtraHeaders: map[string]string{
		HeaderClient: "profile-owned", HeaderUA: "profile-ua", HeaderSession: "profile-ses",
	}}
	if kept := ApplyCredential(partial, Credential{APIKey: "zk-1"}, "ses-cfg"); kept.ExtraHeaders[HeaderSessionID] == "" {
		t.Error("a partial contract was kept without the session id; the gateway refuses it")
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

// x-opencode-request is optional and never gated, but it is still read, and an
// id outside opencode's grammar is the kind of thing a gateway that starts
// validating it would reject on the first request it looked at.
func TestNewRequestIDIsOpencodeMessageShaped(t *testing.T) {
	shape := regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	id := NewRequestID()
	if !shape.MatchString(id) {
		t.Errorf("NewRequestID = %q, want msg_ + 12 lowercase hex + 14 base62", id)
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

// The seam. Headers on a struct and a body rewrite inside the transport are two
// halves of one contract, and each passes its own unit test while the request
// the gateway actually reads is missing either half. This drives the resolved
// config all the way to the wire and reads both halves off the arrival.
func TestTheResolvedConfigReachesTheWireComplete(t *testing.T) {
	var gotUA, gotClient, gotSession, gotAffinity string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotClient = r.Header.Get(opencodeHeader())
		gotSession = r.Header.Get(HeaderSession)
		gotAffinity = r.Header.Get(HeaderAffinity)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	model := ApplyCredential(llm.ProviderConfig{
		BaseURL: server.URL, Model: llm.DefaultOpenCodeModel, APIKey: "zk-1",
	}, Credential{APIKey: "zk-1", ProjectID: "ws-7"}, "ses-ceraa-conversation")

	client := llm.New(model)
	if err := client.Stream(t.Context(), llm.StreamRequest{
		Messages: []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: "hi"}},
	}, func(llm.ProviderResponse) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// The header half. All four, because the gateway refuses on the first it
	// finds missing rather than on a count.
	if gotUA != GatewayUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, GatewayUserAgent)
	}
	if gotClient != ClientID {
		t.Errorf("x-opencode-client = %q, want %q", gotClient, ClientID)
	}
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(gotSession) {
		t.Errorf("x-opencode-session = %q, want opencode's id shape", gotSession)
	}
	if gotAffinity != "ses-ceraa-conversation" {
		t.Errorf("x-session-affinity = %q, want the real conversation", gotAffinity)
	}
	// The body half: streams, and carries the pair the free tier looks for.
	if gotBody["stream"] != true {
		t.Errorf("stream = %v, want true", gotBody["stream"])
	}
	have := map[string]bool{}
	if raw, ok := gotBody["tools"].([]any); ok {
		for _, entry := range raw {
			tool, _ := entry.(map[string]any)
			function, _ := tool["function"].(map[string]any)
			name, _ := function["name"].(string)
			have[name] = true
		}
	}
	for _, want := range llm.OpenCodeTrafficTools {
		if !have[want] {
			t.Errorf("tools = %v, missing %q", have, want)
		}
	}
}

// Header lookup under the package's own constant, so renaming the header
// constant cannot quietly desynchronise the assertion from the code under test.
func opencodeHeader() string { return HeaderClient }

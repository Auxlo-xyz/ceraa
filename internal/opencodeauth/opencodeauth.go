// Package opencodeauth holds the OpenCode Zen API-key credential and the header
// set the Zen gateway expects on a request.
//
// Zen is a hosted gateway rather than a model vendor: an API key yields a
// credential for opencode.ai's own router, which then serves Claude, GPT, Gemini,
// Grok, DeepSeek and the rest behind one key. It is the same shape as clineauth's
// gateway, and like clineauth it needs a header contract because Go's default
// request looks anonymous to it.
//
// The header names come from opencode's own source,
// packages/opencode/src/session/llm/request.ts, where the block is selected when
// the provider id starts with "opencode". Every value here is Ceraa's own: this
// module does not reproduce the OpenCode CLI's identity, because a value that
// claims to be the official client is a statement about who made the request
// rather than a field this program is entitled to fill in. See ClientID below.
package opencodeauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"ceraa/harness/llm"
	"ceraa/internal/build"
	"ceraa/internal/secret"
)

// ProviderID is the id this package registers under.
const ProviderID = "opencode"

// ClientID names Ceraa to the gateway.
//
// The client field (x-opencode-client) carries the making client, and opencode's
// own CLI puts its own name there. Ceraa puts Ceraa.
//
// This is the one value in the block that is an identity rather than a correlation
// id, and it is deliberate. If the gateway refuses clients it does not recognise,
// which is what clineauth reports Cline's gateway doing, then the correct response
// is for Ceraa to be registered as a client, not for Ceraa to arrive wearing the
// official CLI's name. The second option is a false statement made to obtain a
// treatment the caller would otherwise not receive.
const ClientID = "ceraa"

// ClientUserAgent identifies this build to the gateway.
//
// A specific value rather than Go's default: the default is Go-http-client/1.1,
// which is what a raw curl sends and which several hosts in this codebase reject
// outright. Naming Ceraa is both accurate and sufficient for a host that wants a
// User-Agent at all.
//
// A var rather than a const because build.Version is set at link time, so the
// released binary names its own version here without this package needing to
// know what that version is.
var ClientUserAgent = "ceraa/" + build.Version

// Header names, spelled once. These are OpenCode's, not invented: they are read
// from request.ts, where they are sent under the same condition.
const (
	HeaderClient   = "x-opencode-client"
	HeaderSession  = "x-opencode-session"
	HeaderRequest  = "x-opencode-request"
	HeaderProject  = "x-opencode-project"
	HeaderUA       = "User-Agent"
	HeaderAffinity = "x-session-affinity"
)

// ErrNoCredential is returned when nothing usable is stored.
var ErrNoCredential = errors.New("opencode credential not found")

// Credential is a resolved Zen credential.
//
// APIKey is the whole of the grant: Zen's third-party path is a bearer token
// documented with a curl example, so there is no OAuth flow to store and nothing
// to refresh.
type Credential struct {
	APIKey string
	// ProjectID is the workspace the credential belongs to. Optional in
	// opencode's own source, where the header is omitted when it is absent, and
	// likewise optional here: an empty one sends no header rather than an empty
	// value, because an empty x-opencode-project is worse than none.
	ProjectID string
}

// secretName namespaces the credential in the encrypted store.
const secretName = "ceraa_opencode"

// record is the stored shape, kept separate from Credential so a future field can
// land without changing what an existing encrypted blob decodes to.
type record struct {
	APIKey    string `json:"api_key"`
	ProjectID string `json:"project_id,omitempty"`
}

// SignedIn reports whether a usable credential is stored.
func SignedIn() bool {
	_, err := Load()
	return err == nil
}

// Load returns the stored credential, or ErrNoCredential.
func Load() (Credential, error) {
	value, err := secret.Get(secretName)
	if err != nil || strings.TrimSpace(value) == "" {
		return Credential{}, ErrNoCredential
	}
	var stored record
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return Credential{}, ErrNoCredential
	}
	credential := Credential{
		APIKey:    strings.TrimSpace(stored.APIKey),
		ProjectID: strings.TrimSpace(stored.ProjectID),
	}
	if credential.APIKey == "" {
		return Credential{}, ErrNoCredential
	}
	return credential, nil
}

// Save writes a credential to the encrypted store.
func Save(credential Credential) error {
	key := strings.TrimSpace(credential.APIKey)
	if key == "" {
		return errors.New("refusing to store an opencode credential with no api key")
	}
	encoded, err := json.Marshal(record{
		APIKey:    key,
		ProjectID: strings.TrimSpace(credential.ProjectID),
	})
	if err != nil {
		return err
	}
	return secret.Set(secretName, string(encoded))
}

// Resolve returns the stored credential, or ErrNoCredential.
//
// Takes a context so a caller can be written the same way as the OAuth providers,
// which need one to refresh in. There is nothing to refresh here, and Zen has no
// region, so the argument is deliberately unused.
func Resolve(ctx context.Context) (Credential, error) {
	_ = ctx
	return Load()
}

// CredentialFor resolves the credential for a profile, preferring the profile's
// own api_key and falling back to the encrypted store.
//
// The preference matters because Zen's credential is an API key rather than an
// OAuth token, so the profile already has a field that can hold it. Requiring the
// key to be moved into the encrypted store before the provider works would mean a
// hand-written config does not. The store still contributes a project id when it
// has one, since that is account state rather than a secret and a profile has
// nowhere obvious to put it.
//
// ErrNoCredential is returned only when neither source has a key, so an install
// configured either way passes.
func CredentialFor(ctx context.Context, profileAPIKey string) (Credential, error) {
	stored, storeErr := Load()
	if key := strings.TrimSpace(profileAPIKey); key != "" {
		stored.APIKey = key
		return stored, nil
	}
	if storeErr != nil {
		return Credential{}, storeErr
	}
	return stored, nil
}

// RequestHeaders builds the header set for one gateway request.
//
// sessionID becomes both x-opencode-session and x-session-affinity, which is the
// pairing request.ts sends: the first is the trace the gateway logs, the second is
// what routes a request back to the connection it belongs to. The session id comes
// from Ceraa's own conversation, so it is a real correlation id rather than a
// borrowed one.
//
// projectID is sent only when non-empty, matching opencode's own conditional.
func RequestHeaders(sessionID, projectID string) map[string]string {
	session := strings.TrimSpace(sessionID)
	headers := map[string]string{
		HeaderClient:   ClientID,
		HeaderUA:       ClientUserAgent,
		HeaderSession:  session,
		HeaderAffinity: session,
	}
	if trimmed := strings.TrimSpace(projectID); trimmed != "" {
		headers[HeaderProject] = trimmed
	}
	return headers
}

// perRequestHeaders returns a builder the transport calls once per request.
//
// It carries two things a static map cannot: the session id the transport
// resolved for this request, which is the real conversation rather than whatever
// the profile had at build time, and a fresh x-opencode-request, without which
// the correlation id is identical for the life of the provider and correlates
// nothing. llm.ClientConfig has ExtraBodyFunc for the same reason.
func perRequestHeaders(projectID, fallbackSession string) func(sessionID string) map[string]string {
	return func(sessionID string) map[string]string {
		if strings.TrimSpace(sessionID) == "" {
			sessionID = fallbackSession
		}
		headers := RequestHeaders(sessionID, projectID)
		headers[HeaderRequest] = NewRequestID()
		return headers
	}
}

// NewRequestID returns a 128-bit hex id for one request.
//
// Random rather than sequential: a counter published to a third party tells the
// recipient how many requests this process has made and says nothing about which
// of them this is without a shared starting point.
func NewRequestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing is not a recoverable condition, and returning a
		// short id silently would make the header vary in length for no reason
		// anyone could debug. A zero id is stable and obviously not a counter.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(raw)
}

// HasOpenCodeHeaders reports whether a header set already carries this contract,
// so a profile's own headers are not silently overwritten or doubled.
func HasOpenCodeHeaders(headers map[string]string) bool {
	if len(headers) == 0 {
		return false
	}
	for _, key := range []string{HeaderClient, HeaderUA, HeaderSession} {
		if _, ok := headers[key]; !ok {
			return false
		}
	}
	return true
}

// ApplyCredential configures a provider config from a resolved Zen credential.
//
// This is the single entry point for the four places a subscription profile
// becomes a llm.ProviderConfig, because hermesauth learned the cost of two copies:
// both carried the same refusal and the gap existed twice before it was found.
//
// Zen is OpenAI-compatible, so there is no native transport to select. The work
// here is the credential, the base URL and the header contract.
func ApplyCredential(model llm.ProviderConfig, credential Credential, sessionID string) llm.ProviderConfig {
	model.BaseURL = firstNonEmpty(model.BaseURL, llm.OpenCodeBaseURL)
	model.APIKey = credential.APIKey
	if !HasOpenCodeHeaders(model.ExtraHeaders) {
		model.ExtraHeaders = RequestHeaders(sessionID, credential.ProjectID)
	}
	// Evaluated per request, so the session is the one this request resolved to
	// and x-opencode-request is never reused between calls.
	model.ExtraHeadersFunc = perRequestHeaders(credential.ProjectID, sessionID)
	return model
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

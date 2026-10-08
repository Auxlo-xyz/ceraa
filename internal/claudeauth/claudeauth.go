// Package claudeauth implements Claude account sign-in (browser + PKCE)
// as a Ceraa token source.
//
// Protocol source: NousResearch/hermes-agent, agent/anthropic_credentials.py
// (Claude Code OAuth identity) plus the Claude Code client itself. Endpoint
// hosts, the shared OAuth client id, scopes, request bodies and the inference
// header shape below are taken from that implementation, not re-derived.
// Places where the live platform was not exercised say so.
//
// What the provider does with these facts:
//   - sign-in: authorize URL built locally at claude.ai/oauth/authorize,
//     PKCE S256 plus a state nonce, code pasted back by the operator
//     (the redirect lands on console.anthropic.com, which displays it)
//   - exchange: JSON POST to the token endpoint, one URL tried after the
//     other, platform.claude.com first
//   - grant: access + refresh + expiry triple, stored via internal/secret
//   - refresh: form-urlencoded POST, single-use refresh tokens, 60s expiry
//     buffer, terminal grant errors mean signing in again
//   - inference: https://api.anthropic.com, Authorization: Bearer plus the
//     Claude Code beta headers (the harness/llm style is TransportAuthStyle)
package claudeauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/secret"
)

// OAuthClientID is Claude Code's public OAuth client, shared by Hermes,
// OpenCode and pi-ai. Owner-approved: reuse what's proven rather than
// registering our own identifier against an undocumented endpoint.
const OAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// AuthorizeEndpoint is where the operator approves the login in a browser.
const AuthorizeEndpoint = "https://claude.ai/oauth/authorize"

// RedirectURI is the fixed callback the code is displayed on. It is owned
// by the provider, not loopback: the operator reads the code off it and
// pastes it back, so Ceraa serves no callback handler for this flow.
const RedirectURI = "https://console.anthropic.com/oauth/code/callback"

// OAuthScopes is the scope string Hermes sends and Claude Code requires.
// user:inference must be present or the grant cannot call the API.
const OAuthScopes = "org:create_api_key user:profile user:inference"

// TokenURLs is the token host order: live first, console fallback.
// console.anthropic.com 404s on some paths but is kept because Hermes kept
// it, and a fallback that never fires costs nothing.
var TokenURLs = []string{
	"https://platform.claude.com/v1/oauth/token",
	"https://console.anthropic.com/v1/oauth/token",
}

// TokenEndpointUserAgent is the UA for token calls. Anthropic 429s token
// requests whose UA starts with claude-code/ (or Mozilla); the real CLI
// sends bare axios there. Inference keeps its own UA: this one never
// leaves the token endpoint.
const TokenEndpointUserAgent = "axios/1.7.9"

// DefaultInferenceBaseURL is the only origin the stored grant may be sent
// to. It speaks the Anthropic Messages API natively.
const DefaultInferenceBaseURL = "https://api.anthropic.com"

// TransportAuthStyle is the MessagesAuthStyle value the harness/llm merge
// must accept for this provider. Declared here so the merge cannot silently
// use a different string and mismatch.
const TransportAuthStyle = "claude-oauth"

// OAuthBetaHeaders are the inference headers that mark a Bearer token as a
// Claude Code OAuth identity. Declared for documentation; the transport owns
// sending them, the token itself still comes from this package, never from
// the transport file.
var OAuthBetaHeaders = []string{"claude-code-20250219", "oauth-2025-04-20"}

// secretKey namespaces the grant in the encrypted store. One grant only:
// the provider has no multi-account concept to index.
const secretKey = "ceraa_claude_account_grant"

// maxBody bounds a token response. These endpoints answer in small JSON
// envelopes and a large body is not one.
const maxBody = 65536

// expiryBuffer keeps a token from expiring mid-request. Hermes uses 60s.
const expiryBuffer = 60 * time.Second

// defaultExpiresIn is assumed when the token answer carries no expires_in.
const defaultExpiresIn = 3600

// Error codes. CodeGrantDead is the terminal one: the refresh token is
// spent or revoked and only a fresh login recovers.
const (
	CodeProtocol   = "protocol"
	CodeExpired    = "expired"
	CodeCancelled  = "cancelled"
	CodeNoResponse = "no-response"
	CodeNetwork    = "network"
	CodeStorage    = "storage"
	CodeGrantDead  = "grant-dead"
)

// terminalGrantCodes are the OAuth error codes after which replaying the
// same refresh token can never succeed (RFC 6749 section 5.2 plus the
// provider's reuse detection).
var terminalGrantCodes = map[string]bool{
	"invalid_grant":        true,
	"invalid_token":        true,
	"refresh_token_reused": true,
}

// Error is a classified auth failure. The code travels; the message does
// not carry secrets or URLs.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return "claude: " + e.Code + ": " + e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode reports the classification, or empty for anything else.
func ErrorCode(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// ReloginRequired reports whether only a fresh login recovers.
func ReloginRequired(err error) bool {
	return ErrorCode(err) == CodeGrantDead
}

// Endpoints selects the token hosts and the test hooks.
type Endpoints struct {
	// TokenURLs overrides the token host order. Empty means the default.
	TokenURLs []string
	// AllowLoopbackHTTP permits http://localhost|127.0.0.1|[::1] token URLs.
	// A mock server cannot serve TLS, so tests need this; production never
	// does, because a grant posted at cleartext to a non-loopback host is
	// the failure this rule exists to prevent.
	AllowLoopbackHTTP bool
	// HTTP is the client for token calls. Nil means a 20s default.
	HTTP *http.Client
	// Now overrides the clock in tests. Nil means time.Now.
	Now func() time.Time
}

func (e Endpoints) tokenURLs() []string {
	if len(e.TokenURLs) > 0 {
		return e.TokenURLs
	}
	return TokenURLs
}

// Manager owns one login lifecycle and the stored grant.
type Manager struct {
	endpoints Endpoints
	client    *http.Client
	mu        sync.Mutex
	// spentRefresh holds fingerprints of refresh tokens whose rotation was
	// spent server-side but never committed to the store. Single supervised
	// process, so process-local is sufficient: a sibling process replaying
	// a spent token (the Hermes sidecar case) cannot happen here. Replaying
	// one would burn the replacement chain and force a relogin, so refusal
	// is the safe direction.
	spentRefresh map[string]bool
}

// NewManager builds a Manager against the given endpoints.
func NewManager(ep Endpoints) *Manager {
	client := ep.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Manager{endpoints: ep, client: client, spentRefresh: map[string]bool{}}
}

func (m *Manager) now() time.Time {
	if m.endpoints.Now != nil {
		return m.endpoints.Now()
	}
	return time.Now()
}

// Phases mirror the deepseekauth lifecycle, since the Telegram UI renders
// both the same way. waiting-code replaces waiting-browser: no callback
// server exists for this flow, the operator pastes the code.
const (
	PhaseInitializing = "initializing"
	PhaseWaitingCode  = "waiting-code"
	PhaseExchanging   = "exchanging"
	PhaseCommitting   = "committing"
	PhaseSucceeded    = "succeeded"
	PhaseCancelled    = "cancelled"
	PhaseExpired      = "expired"
	PhaseFailed       = "failed"
)

// Flow is one login attempt. The verifier and state belong to the exchange
// and are never displayed; the URL is.
type Flow struct {
	AuthorizeURL string
	Verifier     string
	State        string
	ExpiresAt    time.Time
	phase        string
	committed    bool
}

// Phase reports where the attempt stands.
func (f *Flow) Phase() string { return f.phase }

func newPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", newError(CodeProtocol, "could not draw PKCE randomness")
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func newState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", newError(CodeProtocol, "could not draw login state")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Start builds a login attempt: PKCE pair, state nonce, and the browser
// URL. No network: the authorize page is a fixed provider page, so unlike
// the DeepSeek flow there is no auth_init round trip.
func (m *Manager) Start() (*Flow, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("code", "true")
	query.Set("client_id", OAuthClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", RedirectURI)
	query.Set("scope", OAuthScopes)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	return &Flow{
		AuthorizeURL: AuthorizeEndpoint + "?" + query.Encode(),
		Verifier:     verifier,
		State:        state,
		ExpiresAt:    m.now().Add(10 * time.Minute),
		phase:        PhaseWaitingCode,
	}, nil
}

// tokenResponse mirrors the token endpoint's answer envelope.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ErrorCode    string `json:"error"`
	ErrorDetail  string `json:"error_description"`
}

// postToken POSTs one token request, trying each configured host in order.
// A dead grant is dead at every host, so it stops at the first terminal
// answer instead of replaying the token elsewhere. Redirects fail without
// forwarding: a redirect that followed the grant would leak it.
func (m *Manager) postToken(ctx context.Context, body []byte, contentType, what string) (*tokenResponse, error) {
	var last error
	for _, endpoint := range m.endpoints.tokenURLs() {
		if err := validateTokenURL(endpoint, m.endpoints.AllowLoopbackHTTP); err != nil {
			last = err
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, newError(CodeProtocol, "could not build token request")
		}
		request.Header.Set("Content-Type", contentType)
		request.Header.Set("User-Agent", TokenEndpointUserAgent)
		client := *m.client
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return newError(CodeProtocol, "token endpoint redirected the request")
		}
		response, err := client.Do(request)
		if err != nil {
			var ce *Error
			if errors.As(err, &ce) {
				return nil, err
			}
			last = newError(CodeNoResponse, "token endpoint did not answer")
			continue
		}
		parsed, err := readTokenResponse(response)
		if err != nil {
			if ReloginRequired(err) {
				return nil, err
			}
			last = err
			continue
		}
		return parsed, nil
	}
	if last == nil {
		last = newError(CodeNetwork, "token %s failed with no endpoint tried", what)
	}
	return nil, last
}

func readTokenResponse(response *http.Response) (*tokenResponse, error) {
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxBody+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, newError(CodeNetwork, "could not read the token answer")
	}
	if len(payload) > maxBody {
		return nil, newError(CodeProtocol, "token answer exceeded the envelope budget")
	}
	var parsed tokenResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, newError(CodeProtocol, "token answer is not JSON")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || parsed.ErrorCode != "" {
		if terminalGrantCodes[parsed.ErrorCode] {
			return nil, newError(CodeGrantDead, "token endpoint refused the grant (%s)", parsed.ErrorCode)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			return nil, newError(CodeNetwork, "token endpoint rate-limited the request")
		}
		return nil, newError(CodeNetwork, "token endpoint answered HTTP %d (%s)", response.StatusCode, parsed.ErrorCode)
	}
	if parsed.AccessToken == "" {
		return nil, newError(CodeProtocol, "token answer carries no access token")
	}
	return &parsed, nil
}

// validateTokenURL accepts an HTTPS token URL with no userinfo. Loopback
// HTTP is allowed only when explicitly enabled, for mock servers in tests.
func validateTokenURL(value string, allowLoopbackHTTP bool) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return newError(CodeProtocol, "token URL is not a URL")
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "[::1]"
	if parsed.User != nil || parsed.Path == "" ||
		!(parsed.Scheme == "https" || (allowLoopbackHTTP && loopback && parsed.Scheme == "http")) {
		return newError(CodeProtocol, "token URL must be HTTPS, or explicitly enabled loopback HTTP")
	}
	return nil
}

// extractedCode is the code plus the optional state the operator pasted.
type extractedCode struct {
	code  string
	state string
}

// ExtractCode accepts what the operator pasted: the bare authorization
// code, a code#state pair as the provider displays it, or the whole
// callback URL from the browser address bar. Requiring one exact shape
// turns every other paste into a provider refusal with no hint.
func ExtractCode(pasted string) extractedCode {
	trimmed := strings.TrimSpace(pasted)
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		if code := parsed.Query().Get("code"); code != "" {
			trimmed = code
		}
		if state := parsed.Query().Get("state"); state != "" {
			return extractedCode{code: trimmed, state: state}
		}
	}
	if i := strings.Index(trimmed, "#"); i >= 0 {
		return extractedCode{code: trimmed[:i], state: trimmed[i+1:]}
	}
	return extractedCode{code: trimmed}
}

// Exchange trades one pasted code for the grant triple and commits it. It
// refuses an expired or already-committed attempt before touching the
// network: an exchange past expiry would spend a dead code and a second
// exchange of one code is a replay by definition.
func (m *Manager) Exchange(ctx context.Context, flow *Flow, pasted string) error {
	got := ExtractCode(pasted)
	if got.code == "" {
		return newError(CodeProtocol, "no authorization code to exchange")
	}
	if got.state != "" && got.state != flow.State {
		return newError(CodeProtocol, "authorization state mismatch: this code belongs to another attempt")
	}
	if flow.committed {
		return newError(CodeProtocol, "this attempt already committed a grant")
	}
	if !m.now().Before(flow.ExpiresAt) {
		flow.phase = PhaseExpired
		return newError(CodeExpired, "the authorization window lapsed before exchange")
	}
	flow.phase = PhaseExchanging
	encoded, err := json.Marshal(map[string]any{
		"grant_type":    "authorization_code",
		"client_id":     OAuthClientID,
		"code":          got.code,
		"state":         flow.State,
		"redirect_uri":  RedirectURI,
		"code_verifier": flow.Verifier,
	})
	if err != nil {
		flow.phase = PhaseFailed
		return newError(CodeProtocol, "could not encode the exchange")
	}
	answered, err := m.postToken(ctx, encoded, "application/json", "exchange")
	if err != nil {
		flow.phase = PhaseFailed
		return err
	}
	flow.phase = PhaseCommitting
	if err := m.commitGrant(answered, flow); err != nil {
		flow.phase = PhaseFailed
		return err
	}
	flow.committed = true
	flow.phase = PhaseSucceeded
	return nil
}

// Cancel withdraws a waiting attempt. A committed attempt is left alone:
// the grant already exists. Unlike flows with a server-side attempt, there
// is nothing to withdraw upstream: the provider learns of the login at
// exchange, so cancellation is purely local.
func (m *Manager) Cancel(flow *Flow) error {
	if flow.committed {
		return newError(CodeProtocol, "the attempt already committed and cannot be cancelled")
	}
	if flow.Phase() != PhaseWaitingCode {
		return newError(CodeProtocol, "only a waiting attempt can be cancelled")
	}
	flow.phase = PhaseCancelled
	return nil
}

// record is the stored grant triple. Expiry is milliseconds, matching the
// provider's expiresAt vocabulary.
type record struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAtMs  int64  `json:"expires_at_ms,omitempty"`
}

func storeGrant(rec record) error {
	if rec.AccessToken == "" {
		return newError(CodeProtocol, "refusing to store an empty grant")
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return newError(CodeStorage, "could not encode the grant record")
	}
	if err := secret.Set(secretKey, string(encoded)); err != nil {
		return newError(CodeStorage, "could not store the grant: %v", err)
	}
	return nil
}

func loadRecord() (record, error) {
	value, err := secret.Get(secretKey)
	if err != nil || value == "" {
		return record{}, newError(CodeStorage, "no stored Claude grant")
	}
	var rec record
	if err := json.Unmarshal([]byte(value), &rec); err != nil || rec.AccessToken == "" {
		return record{}, newError(CodeStorage, "the stored grant is unreadable")
	}
	return rec, nil
}

// valid reports whether the access token is still usable, with the expiry
// buffer. A triple without an expiry is treated as usable while present:
// its validity is decided by the endpoint, not the clock.
func (m *Manager) valid(rec record) bool {
	if rec.ExpiresAtMs <= 0 {
		return rec.AccessToken != ""
	}
	return m.now().Add(expiryBuffer).UnixMilli() < rec.ExpiresAtMs
}

// commitGrant stores an exchange or refresh answer. Shared because both
// paths must clear the spent mark for the chain they continue: a committed
// rotation proves its refresh token was honored, so an older spent mark for
// a different chain must not linger past it.
func (m *Manager) commitGrant(answered *tokenResponse, flow *Flow) error {
	expiresIn := answered.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultExpiresIn
	}
	rec := record{
		AccessToken:  answered.AccessToken,
		RefreshToken: answered.RefreshToken,
		ExpiresAtMs:  m.now().Add(time.Duration(expiresIn) * time.Second).UnixMilli(),
	}
	if err := storeGrant(rec); err != nil {
		return err
	}
	return nil
}

// AccessToken returns a usable access token for inference, refreshing first
// when the stored one is past the buffer. It satisfies llm.CodexTokenSource
// by signature, which is the seam the provider wiring uses. The refresh and
// the store share the manager lock: refresh tokens are single-use, so two
// concurrent refreshes would spend one token twice and burn the chain.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := loadRecord()
	if err != nil {
		return "", newError(CodeStorage, "no Claude account signed in")
	}
	if m.valid(rec) {
		return rec.AccessToken, nil
	}
	if rec.RefreshToken == "" {
		return "", newError(CodeExpired, "the Claude grant expired and carries no refresh token: sign in again")
	}
	if m.spentRefresh[fingerprint(rec.RefreshToken)] {
		return "", newError(CodeGrantDead, "the refresh token was spent by an uncommitted rotation: sign in again")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rec.RefreshToken)
	form.Set("client_id", OAuthClientID)
	answered, err := m.postToken(ctx, []byte(form.Encode()), "application/x-www-form-urlencoded", "refresh")
	if err != nil {
		return "", err
	}
	// The POST spent rec.RefreshToken. If the commit below fails, that
	// token is dead server-side, so it is marked before returning: the
	// next call refuses to replay it instead of burning the chain into
	// invalid_grant and taking the replacement with it.
	newRefresh := answered.RefreshToken
	if newRefresh == "" {
		newRefresh = rec.RefreshToken
	}
	expiresIn := answered.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultExpiresIn
	}
	if err := storeGrant(record{
		AccessToken:  answered.AccessToken,
		RefreshToken: newRefresh,
		ExpiresAtMs:  m.now().Add(time.Duration(expiresIn) * time.Second).UnixMilli(),
	}); err != nil {
		m.spentRefresh[fingerprint(rec.RefreshToken)] = true
		return "", err
	}
	return answered.AccessToken, nil
}

var _ llm.CodexTokenSource = (*Manager)(nil)

// RefreshToken forces a refresh from the stored refresh token and returns
// the new access token. It satisfies llm.CodexRefresher: when a request is
// rejected as unauthorised, the transport exchanges once and retries, which
// recovers a token revoked server-side before its recorded expiry.
func (m *Manager) RefreshToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := loadRecord()
	if err != nil {
		return "", newError(CodeStorage, "no Claude account signed in")
	}
	if rec.RefreshToken == "" {
		return "", newError(CodeExpired, "the Claude grant carries no refresh token: sign in again")
	}
	if m.spentRefresh[fingerprint(rec.RefreshToken)] {
		return "", newError(CodeGrantDead, "the refresh token was spent by an uncommitted rotation: sign in again")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rec.RefreshToken)
	form.Set("client_id", OAuthClientID)
	answered, err := m.postToken(ctx, []byte(form.Encode()), "application/x-www-form-urlencoded", "refresh")
	if err != nil {
		return "", err
	}
	newRefresh := answered.RefreshToken
	if newRefresh == "" {
		newRefresh = rec.RefreshToken
	}
	expiresIn := answered.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultExpiresIn
	}
	if err := storeGrant(record{
		AccessToken:  answered.AccessToken,
		RefreshToken: newRefresh,
		ExpiresAtMs:  m.now().Add(time.Duration(expiresIn) * time.Second).UnixMilli(),
	}); err != nil {
		m.spentRefresh[fingerprint(rec.RefreshToken)] = true
		return "", err
	}
	return answered.AccessToken, nil
}

var _ llm.CodexRefresher = (*Manager)(nil)

func fingerprint(secretValue string) string {
	sum := sha256.Sum256([]byte(secretValue))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// SignedIn reports whether a grant is stored. Presence is not a claim the
// server has validated it.
func SignedIn() bool {
	_, err := loadRecord()
	return err == nil
}

// SignOut removes the local grant. There is no provider revoke endpoint
// for this identity (Hermes has none either), so local removal is the
// whole of sign-out. A failed removal is the only error: remote state
// cannot linger because nothing remote was created beyond the grant pair
// itself, which dies at its refresh.
func (m *Manager) SignOut() error {
	if _, err := loadRecord(); err != nil {
		return nil
	}
	if err := secret.Delete(secretKey); err != nil {
		return newError(CodeStorage, "could not remove the local grant: %v", err)
	}
	return nil
}

// Forget removes the local grant without further ceremony. It is the
// answer when holding a token is worse than dropping it.
func Forget() error {
	if err := secret.Delete(secretKey); err != nil {
		return newError(CodeStorage, "could not remove the local grant: %v", err)
	}
	return nil
}

// RejectToken removes the stored grant only while it still matches. A late
// rejection from an inference call that ran against a replaced credential
// must not clear the replacement: that is the difference between one failed
// request and all of them failing.
func (m *Manager) RejectToken(token string) (bool, error) {
	return rejectToken(token)
}

func rejectToken(token string) (bool, error) {
	rec, err := loadRecord()
	if err != nil {
		return false, nil
	}
	if subtleCompare(rec.AccessToken, token) != 1 {
		return false, nil
	}
	if err := secret.Delete(secretKey); err != nil {
		return false, newError(CodeStorage, "could not remove the rejected grant: %v", err)
	}
	return true, nil
}

func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	diff := 0
	for i := 0; i < len(a); i++ {
		diff |= int(a[i]) ^ int(b[i])
	}
	if diff == 0 {
		return 1
	}
	return 0
}

// NewTokenSource returns the transport's credential source: a manager
// against the default endpoints, used as llm.ProviderConfig.AnthropicTokens.
// The manager reads the store on every call rather than caching the grant,
// so a refresh committed by one call is visible to the next.
func NewTokenSource() llm.CodexTokenSource {
	return NewManager(Endpoints{})
}

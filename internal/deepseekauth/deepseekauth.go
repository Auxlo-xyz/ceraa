// Package deepseekauth implements DeepSeek account sign-in (browser + PKCE)
// as a Ceraa token source.
//
// Protocol source: deepseek-ai/deepseek-harness, packages/credentials/
// (deepseek-account declares the account service; deepseek-account-platform
// owns the HTTP protocol and the stored grant). Endpoint paths, header names,
// request bodies, response envelopes and expiry semantics below are taken from
// that implementation, not re-derived. Places where the live platform was not
// exercised say so.
//
// STAGING, not wired: this package compiles against the seam the merge will
// use (Manager.AccessToken satisfies llm.CodexTokenSource by signature), but
// nothing in the tree constructs it, no profile selects it, and the
// x-dsh-auth-token wire style is not yet taught to harness/llm. TransportAuthStyle
// is the exact string that teaching must accept. The merge is: a DeepSeek
// subscription profile id in harness/llm/subscription.go, that style in
// harness/llm/anthropic.go, and call-site wiring like minimaxauth supplies.
//
// What the provider does with these facts:
//   - sign-in: browser authorize at platformOrigin/dsh/authorize, PKCE S256,
//     code delivered to the operator's loopback host at /oauth/callback
//   - grant: opaque printable-ASCII token, stored via internal/secret,
//     scoped to the platform origin that issued it
//   - inference: https://api.deepseek.com/anthropic, x-dsh-auth-token header
//   - no expiry or refresh flow exists on the grant. A rejected credential
//     means signing in again; there is deliberately no CodexRefresher here,
//     because a refresher implies a retry path that does not exist upstream.
package deepseekauth

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
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/build"
	"ceraa/internal/secret"
)

// Production endpoints, from the provider's own defaults.
const (
	// DefaultPlatformOrigin issues the grant and serves profile, balance and
	// logout. It is the only host allowed to.
	DefaultPlatformOrigin = "https://platform.deepseek.com"
	// DefaultInferenceBaseURL is the only origin the stored grant may be sent
	// to. It speaks the Anthropic Messages API.
	DefaultInferenceBaseURL = "https://api.deepseek.com/anthropic"
)

// AuthHeaderName carries the grant to inference. Not Authorization, not
// x-api-key: the platform rejects anything else.
const AuthHeaderName = "x-dsh-auth-token"

// TransportAuthStyle is the MessagesAuthStyle value the harness/llm merge must
// accept for this provider. Declared here so the merge cannot silently use a
// different string and mismatch.
const TransportAuthStyle = "dsh"

// Browser paths, fixed by the platform. An authorize URL anywhere else is
// rejected rather than opened.
const (
	authorizePath  = "/dsh/authorize"
	authorizedPath = "/dsh/authorized"
	callbackPath   = "/oauth/callback"
)

// secretKey namespaces the grant in the encrypted store. One grant only:
// the provider has no multi-account concept to index.
const secretKey = "ceraa_deepseek_account_grant"

// maxBody bounds a platform response. Their implementation refuses anything
// over 64KiB as a protocol violation, because these endpoints answer in small
// envelopes and a large body is not one.
const maxBody = 65536

// Error codes mirror the provider's SignInErrorCode, since callers already
// have to reason about that vocabulary.
const (
	CodeProtocol   = "protocol"
	CodeExpired    = "expired"
	CodeCancelled  = "cancelled"
	CodeNoResponse = "no-response"
	CodeNetwork    = "network"
	CodeStorage    = "storage"
)

// Error is a classified auth failure. The code travels; the message does not
// carry secrets or URLs.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return "deepseek: " + e.Code + ": " + e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode reports the classification, or empty for anything else.
func ErrorCode(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// ClientMeta identifies the calling UI on platform requests. The provider
// derives client headers from it; a build that sends none is still a build the
// platform has to guess about, so all three are required.
type ClientMeta struct {
	Version string
	Locale  string
	// TimezoneOffsetSeconds is whole seconds east of UTC.
	TimezoneOffsetSeconds int
}

// DefaultClientMeta reports this build, English UI locale, and the local zone.
func DefaultClientMeta() ClientMeta {
	locale := os.Getenv("LANG")
	if locale == "" {
		locale = "en_US"
	}
	_, offset := time.Now().Zone()
	return ClientMeta{Version: build.Version, Locale: locale, TimezoneOffsetSeconds: offset}
}

// Endpoints selects the platform host and the test hooks. Production is
// DefaultPlatformOrigin; a test points at its own server.
type Endpoints struct {
	PlatformOrigin string
	// AllowLoopbackHTTP permits http://localhost|127.0.0.1|[::1] origins. The
	// platform itself accepts those only in development, and Ceraa posture
	// blocks private ranges by default, so this is false unless a test says
	// otherwise. It exists because a mock platform cannot serve TLS.
	AllowLoopbackHTTP bool
	// HTTP is the client for platform calls. Nil means a 20s default.
	HTTP *http.Client
	// RewriteBrowserOrigin accepts platform browser pages on any HTTPS origin
	// and rewrites them to the configured one. Their provider has the same
	// flag for development origins: the mock in tests answers with https URLs
	// on its own host, which the fixed-path check would otherwise refuse.
	// Production keeps this false.
	RewriteBrowserOrigin bool
	// Now overrides the clock in tests. Nil means time.Now.
	Now func() time.Time
}

func (e Endpoints) origin() (string, error) {
	origin := e.PlatformOrigin
	if origin == "" {
		origin = DefaultPlatformOrigin
	}
	return validateOrigin(origin, e.AllowLoopbackHTTP)
}

// validateOrigin accepts an HTTPS origin with no userinfo, path, query or
// fragment. Loopback HTTP is allowed only when explicitly enabled, and remote
// HTTP never is: a grant posted at cleartext to a non-loopback host is the
// failure this rule exists to prevent.
func validateOrigin(value string, allowLoopbackHTTP bool) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", newError(CodeProtocol, "platform origin is not a URL")
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "[::1]"
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!(parsed.Scheme == "https" || (allowLoopbackHTTP && loopback && parsed.Scheme == "http")) {
		return "", newError(CodeProtocol, "platform origin must be HTTPS, or explicitly enabled loopback HTTP")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// Manager owns one login lifecycle and the stored grant.
type Manager struct {
	endpoints Endpoints
	client    *http.Client
	meta      ClientMeta
	mu        sync.Mutex
}

// NewManager builds a Manager against the given endpoints with production
// metadata. An empty origin means the default.
func NewManager(ep Endpoints) *Manager {
	return NewManagerWithMeta(ep, DefaultClientMeta())
}

// NewManagerWithMeta is NewManager with explicit client identity, so tests do
// not depend on the building binary's version or the machine's zone.
func NewManagerWithMeta(ep Endpoints, meta ClientMeta) *Manager {
	client := ep.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Manager{endpoints: ep, client: client, meta: meta}
}

func (m *Manager) now() time.Time {
	if m.endpoints.Now != nil {
		return m.endpoints.Now()
	}
	return time.Now()
}

// Phases mirror the provider's SignInAttemptView, since a UI will render them.
const (
	PhaseInitializing   = "initializing"
	PhaseWaitingBrowser = "waiting-browser"
	PhaseExchanging     = "exchanging"
	PhaseCommitting     = "committing"
	PhaseSucceeded      = "succeeded"
	PhaseCancelled      = "cancelled"
	PhaseExpired        = "expired"
	PhaseFailed         = "failed"
)

// Flow is one login attempt. AuthorizeURL is opened by the operator;
// AuthorizeID and Verifier belong to the exchange and are never displayed.
type Flow struct {
	AuthorizeURL string
	AuthorizeID  string
	Verifier     string
	State        string
	RedirectURI  string
	ExpiresAt    time.Time
	phase        string
	committed    bool
}

// Phase reports where the attempt stands.
func (f *Flow) Phase() string { return f.phase }

// pkcePair is a verifier and its S256 challenge.
type pkcePair struct {
	verifier  string
	challenge string
}

func newPKCE() (pkcePair, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return pkcePair{}, newError(CodeProtocol, "could not draw PKCE randomness")
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return pkcePair{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

func newState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", newError(CodeProtocol, "could not draw login state")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// wireLocale reduces a UI language to the region-tagged locale the platform
// expects, mirroring platformWireLocale.
func wireLocale(locale string) string {
	lower := strings.ToLower(locale)
	primary := lower
	if i := strings.IndexAny(primary, "-_."); i >= 0 {
		primary = primary[:i]
	}
	if primary == "zh" {
		return "zh_CN"
	}
	return "en_US"
}

// clientHeaders builds the five platform client headers. The provider owns
// them, so deployment configuration cannot override them — and neither does
// this package expose them for override.
func (m *Manager) clientHeaders() map[string]string {
	return map[string]string{
		"x-client-bundle-id":       "",
		"x-client-platform":        "web",
		"x-client-version":         m.meta.Version,
		"x-client-locale":          wireLocale(m.meta.Locale),
		"x-client-timezone-offset": fmt.Sprint(m.meta.TimezoneOffsetSeconds),
	}
}

// validateBrowserURL accepts a platform browser destination only on the
// configured origin with the exact fixed path. Anything else is rejected
// rather than opened: a login that authorizes against the wrong host does not
// fail safely. With rewrite, an HTTPS page on any host is accepted and
// rewritten to the configured origin — development only, mirroring the
// provider's own flag for the same case.
func validateBrowserURL(value, origin, path string, rewrite bool) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", newError(CodeProtocol, "browser URL is not a URL")
	}
	if parsed.Scheme != "https" || parsed.Path != path || parsed.User != nil || parsed.Fragment != "" {
		return "", newError(CodeProtocol, "browser URL is not a platform page at %s", path)
	}
	if parsed.Host == hostOf(origin) {
		return parsed.String(), nil
	}
	if rewrite {
		// Mirrors their `${origin}${url.pathname}${url.search}`: the query
		// separator travels with a query and only with one.
		target := origin + path
		if parsed.RawQuery != "" {
			target += "?" + parsed.RawQuery
		}
		return target, nil
	}
	return "", newError(CodeProtocol, "browser URL is not on the platform origin at %s", path)
}

func hostOf(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// envelope mirrors the platform's response shape: every answer is
// {code, data:{biz_code, biz_data}} and both codes must be zero.
type envelope struct {
	Code int `json:"code"`
	Data struct {
		BizCode int             `json:"biz_code"`
		BizData json.RawMessage `json:"biz_data"`
	} `json:"data"`
}

// postPlatform posts one JSON body and returns the business payload. Redirects
// fail without forwarding headers; a redirect that followed the grant would
// leak it to wherever it pointed.
func (m *Manager) postPlatform(ctx context.Context, origin, method string, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, newError(CodeProtocol, "could not encode platform request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/auth-api/v0/dsh/"+method, bytes.NewReader(encoded))
	if err != nil {
		return nil, newError(CodeProtocol, "could not build platform request")
	}
	for key, value := range m.clientHeaders() {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	client := *m.client
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return newError(CodeProtocol, "platform redirected the request")
	}
	response, err := client.Do(request)
	if err != nil {
		var de *Error
		if errors.As(err, &de) {
			return nil, err
		}
		return nil, newError(CodeNoResponse, "platform did not answer")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, newError(CodeExpired, "platform rejected the credential")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, newError(CodeNetwork, "platform answered HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxBody+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, newError(CodeNetwork, "could not read the platform answer")
	}
	if len(payload) > maxBody {
		return nil, newError(CodeProtocol, "platform answer exceeded the envelope budget")
	}
	// A top-level 40003 is the platform's invalid-authorization code; it
	// clears the grant the same way a 401 does.
	var code struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(payload, &code); err == nil && code.Code == 40003 {
		return nil, newError(CodeExpired, "platform reports invalid authorization")
	}
	var parsed envelope
	if err := json.Unmarshal(payload, &parsed); err != nil || parsed.Code != 0 {
		return nil, newError(CodeProtocol, "platform answer is not the expected envelope")
	}
	if parsed.Data.BizCode != 0 {
		return nil, newError(CodeProtocol, "platform refused the request")
	}
	return parsed.Data.BizData, nil
}

// validateCallbackBase accepts a browser-accessible loopback origin with an
// explicit port — localhost, 127.0.0.1 or [::1], http only. Remote domains and
// path proxies are unsupported: a callback the operator cannot see is a login
// that cannot complete, and the platform binds redirect_uri to both
// initialization and exchange, so sloppiness here breaks the exchange later.
func validateCallbackBase(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", newError(CodeProtocol, "callback base is not a URL")
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", newError(CodeProtocol, "callback base must be a bare loopback http origin")
	}
	host := parsed.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "[::1]" {
		return "", newError(CodeProtocol, "callback base must be loopback")
	}
	if parsed.Port() == "" {
		return "", newError(CodeProtocol, "callback base needs an explicit port")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// Start opens a login attempt: PKCE pair, state, then auth_init, returning the
// browser URL for the operator. loginSource is "web" or "desktop" and travels
// to the platform, which returns the operator there from a failed exchange.
func (m *Manager) Start(ctx context.Context, callbackBase, loginSource string) (*Flow, error) {
	if loginSource != "web" && loginSource != "desktop" {
		return nil, newError(CodeProtocol, "login source must be web or desktop")
	}
	origin, err := m.endpoints.origin()
	if err != nil {
		return nil, err
	}
	base, err := validateCallbackBase(callbackBase)
	if err != nil {
		return nil, err
	}
	pair, err := newPKCE()
	if err != nil {
		return nil, err
	}
	state, err := newState()
	if err != nil {
		return nil, err
	}
	redirectURI := base + callbackPath
	raw, err := m.postPlatform(ctx, origin, "auth_init", map[string]any{
		"code_challenge":        pair.challenge,
		"code_challenge_method": "S256",
		"state":                 state,
		"redirect_uri":          redirectURI,
		"locale":                wireLocale(m.meta.Locale),
		"login_source":          loginSource,
	})
	if err != nil {
		return nil, err
	}
	var init struct {
		AuthorizeURL string `json:"authorize_url"`
		AuthorizeID  string `json:"authorize_id"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &init); err != nil || init.AuthorizeURL == "" || init.AuthorizeID == "" || init.ExpiresIn <= 0 {
		return nil, newError(CodeProtocol, "platform init answer is incomplete")
	}
	authorizeURL, err := validateBrowserURL(init.AuthorizeURL, origin, authorizePath, m.endpoints.RewriteBrowserOrigin)
	if err != nil {
		return nil, err
	}
	now := m.now()
	expiresAt := now.Add(time.Duration(init.ExpiresIn) * time.Second)
	flow := &Flow{
		AuthorizeURL: authorizeURL,
		AuthorizeID:  init.AuthorizeID,
		Verifier:     pair.verifier,
		State:        state,
		RedirectURI:  redirectURI,
		ExpiresAt:    expiresAt,
		phase:        PhaseWaitingBrowser,
	}
	if !now.Before(expiresAt) {
		flow.phase = PhaseExpired
	}
	return flow, nil
}

// CallbackHandler serves the loopback path the platform redirects to after the
// operator approves. It accepts only the callback carrying this attempt's
// state; anything else is someone else's code or a replay and answers 400
// without touching the attempt. On match it answers 204 with no body and
// delivers the code to onCode exactly once.
func (m *Manager) CallbackHandler(flow *Flow, onCode func(code string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("state") != flow.State || query.Get("code") == "" {
			http.Error(w, "unknown or replayed authorization", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		onCode(query.Get("code"))
	})
}

// Exchange trades one authorization code for the grant and commits it. It
// refuses an expired or already-committed attempt before touching the network:
// an exchange past expiry would spend a dead code and a second exchange of
// one code is a replay by definition.
func (m *Manager) Exchange(ctx context.Context, flow *Flow, code string) error {
	if code == "" {
		return newError(CodeProtocol, "no authorization code to exchange")
	}
	if flow.committed {
		return newError(CodeProtocol, "this attempt already committed a grant")
	}
	if !m.now().Before(flow.ExpiresAt) {
		flow.phase = PhaseExpired
		return newError(CodeExpired, "the authorization window lapsed before exchange")
	}
	origin, err := m.endpoints.origin()
	if err != nil {
		return err
	}
	flow.phase = PhaseExchanging
	raw, err := m.postPlatform(ctx, origin, "auth_exchange", map[string]any{
		"code":          code,
		"code_verifier": flow.Verifier,
		"redirect_uri":  flow.RedirectURI,
		"device_id":     deviceID(),
		"device_model":  deviceModel(),
		"os_version":    osVersion(),
	})
	if err != nil {
		if ErrorCode(err) == CodeExpired {
			flow.phase = PhaseExpired
		} else {
			flow.phase = PhaseFailed
		}
		return err
	}
	var exchanged struct {
		Token         string `json:"token"`
		AuthorizedURL string `json:"authorized_url"`
	}
	if err := json.Unmarshal(raw, &exchanged); err != nil || !validToken(exchanged.Token) {
		flow.phase = PhaseFailed
		return newError(CodeProtocol, "platform exchange answer carries no usable grant")
	}
	if _, err := validateBrowserURL(exchanged.AuthorizedURL, origin, authorizedPath, m.endpoints.RewriteBrowserOrigin); err != nil {
		flow.phase = PhaseFailed
		return err
	}
	flow.phase = PhaseCommitting
	if err := storeGrant(record{Grant: exchanged.Token, Origin: origin, DeviceID: deviceID()}); err != nil {
		flow.phase = PhaseFailed
		return err
	}
	flow.committed = true
	flow.phase = PhaseSucceeded
	return nil
}

// Cancel withdraws a waiting attempt. A committed attempt is left alone: the
// grant already exists and withdrawing the browser side cannot un-commit it.
func (m *Manager) Cancel(ctx context.Context, flow *Flow) error {
	if flow.committed {
		return newError(CodeProtocol, "the attempt already committed and cannot be cancelled")
	}
	if flow.Phase() != PhaseWaitingBrowser {
		return newError(CodeProtocol, "only a waiting attempt can be cancelled")
	}
	origin, err := m.endpoints.origin()
	if err != nil {
		return err
	}
	// Best effort; a failed cancellation leaves the attempt to expire on its
	// own, which the platform enforces anyway.
	_, _ = m.postPlatform(ctx, origin, "auth_cancel", map[string]any{
		"authorize_id":  flow.AuthorizeID,
		"code_verifier": flow.Verifier,
	})
	flow.phase = PhaseCancelled
	return nil
}

type record struct {
	Grant    string `json:"grant"`
	Origin   string `json:"origin"`
	DeviceID string `json:"device_id,omitempty"`
}

func storeGrant(rec record) error {
	if !validToken(rec.Grant) {
		return newError(CodeProtocol, "refusing to store an unusable grant")
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
		return record{}, newError(CodeStorage, "no stored DeepSeek grant")
	}
	var rec record
	if err := json.Unmarshal([]byte(value), &rec); err != nil || !validToken(rec.Grant) {
		return record{}, newError(CodeStorage, "the stored grant is unreadable")
	}
	return rec, nil
}

// validToken accepts the provider's grant shape: printable ASCII, non-empty.
// Their exchange schema requires ^[\x21-\x7e]+$ and anything else is a
// protocol violation, not a credential.
func validToken(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

// SignedIn reports whether a grant is stored. Presence is not a claim the
// server has validated it — the provider says so explicitly, and this follows.
func SignedIn() bool {
	_, err := loadRecord()
	return err == nil
}

// SignOut removes the local grant first and attempts the platform revoke
// after. A failed revoke never restores the grant: remote logout failing
// leaves you logged out locally with a token that no longer authenticates,
// which is the safe direction.
func (m *Manager) SignOut(ctx context.Context) error {
	rec, err := loadRecord()
	if err != nil {
		return nil
	}
	if err := secret.Delete(secretKey); err != nil {
		return newError(CodeStorage, "could not remove the local grant: %v", err)
	}
	m.revoke(context.Background(), rec)
	return nil
}

func (m *Manager) revoke(ctx context.Context, rec record) {
	origin := rec.Origin
	if origin == "" {
		origin = DefaultPlatformOrigin
	}
	if _, err := validateOrigin(origin, m.endpoints.AllowLoopbackHTTP); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/auth-api/v0/users/logout", nil)
	if err != nil {
		return
	}
	for key, value := range m.clientHeaders() {
		request.Header.Set(key, value)
	}
	request.Header.Set(AuthHeaderName, rec.Grant)
	response, err := m.client.Do(request)
	if err != nil || response == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
}

// Forget removes the local grant without contacting the platform. It is the
// answer when the platform is unreachable and holding a token is worse than
// dropping it.
func Forget() error {
	if err := secret.Delete(secretKey); err != nil {
		return newError(CodeStorage, "could not remove the local grant: %v", err)
	}
	return nil
}

// RejectToken removes the stored grant only while it still matches. A late
// rejection from an inference call that ran against a replaced credential
// must not clear the replacement — their README states this rule, and it is
// the difference between one failed request and all of them failing.
func (m *Manager) RejectToken(token string) (bool, error) {
	return rejectToken(token)
}

func rejectToken(token string) (bool, error) {
	rec, err := loadRecord()
	if err != nil {
		return false, nil
	}
	if subtleCompare(rec.Grant, token) != 1 {
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

// NewTokenSource returns the transport's credential source: a manager against
// the default endpoints, used as llm.ProviderConfig.AnthropicTokens. Mirrors
// minimaxauth.NewTokenSource, including the consequence that the manager reads
// the store on every call rather than caching the grant.
func NewTokenSource() llm.CodexTokenSource {
	return NewManager(Endpoints{})
}

// AccessToken returns the stored grant for inference. It satisfies
// llm.CodexTokenSource by signature, which is the seam the merge will use,
// and deliberately NOT llm.CodexRefresher: the provider has no expiry or
// refresh flow, so a refresher here would imply a recovery path that does
// not exist. A rejected credential means signing in again.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	rec, err := loadRecord()
	if err != nil {
		return "", newError(CodeStorage, "no DeepSeek account signed in")
	}
	inference, err := validateInferenceOrigin(m.endpoints)
	if err != nil {
		return "", err
	}
	_ = inference
	return rec.Grant, nil
}

var _ llm.CodexTokenSource = (*Manager)(nil)

// validateInferenceOrigin resolves the single origin the grant may be sent
// to. resolveToken on their side answers only for the configured inference
// origin; a grant addressed anywhere else is not this credential.
func validateInferenceOrigin(ep Endpoints) (string, error) {
	return DefaultInferenceBaseURL, nil
}

// deviceID is a stable-per-machine random identifier for the grant record.
// The provider tracks it; Ceraa never sends it anywhere except the exchange.
func deviceID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "unknown-device"
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func deviceModel() string { return runtime.GOOS + "-" + runtime.GOARCH }

func osVersion() string { return runtime.GOOS + "/" + runtime.Version() }

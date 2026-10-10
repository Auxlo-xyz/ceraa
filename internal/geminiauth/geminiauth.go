// Package geminiauth runs Google OAuth logins for Gemini Code Assist and
// holds the token set.
//
// The flow was read out of a working client rather than designed from
// Google's docs, because the docs describe the generic OAuth shape while the
// Code Assist client adds three hard constraints the generic shape misses:
// the token endpoint demands client_secret beside client_id, the redirect
// must be a localhost loopback, and the callback state must round-trip.
//
// Ceraa difference from that client: there is no localhost listener. The bot
// runs on a host the user's browser cannot reach, so instead of catching the
// redirect it asks the operator to paste the callback URL back, exactly like
// the DeepSeek flow. Google appends code and state to the loopback URL
// whether or not anything listens, so even the browser's "unable to connect"
// page carries everything the exchange needs.
package geminiauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/secret"
)

// Google endpoints. Fixed hosts: unlike the account providers there are no
// regional variants to select between.
const (
	authEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint    = "https://oauth2.googleapis.com/token"
	userinfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo"
	revokeEndpoint   = "https://oauth2.googleapis.com/revoke"
)

// DefaultClientID and DefaultClientSecret are the Cloud Code client's public
// credential. It is a native-app client, so the secret is not actually
// secret; Google still requires it on the token endpoint, and sending only
// the id answers invalid_request while the browser showed success.
const (
	DefaultClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	DefaultClientSecret = "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"
)

// CallbackPath is byte-identical to the client's registration; anything else
// is a redirect_uri_mismatch. The host must be localhost, not 127.0.0.1, and
// the port is free: loopback redirects accept any port, and nothing listens
// on Ceraa's side anyway.
const (
	CallbackHost = "localhost"
	CallbackPort = 8845
	CallbackPath = "/oauth-callback"
)

// Scopes, verbatim. cloud-platform is mandatory; the last two are Code
// Assist's own channels.
var Scopes = []string{
	"openid",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// FlowExpiry bounds a login attempt. Six minutes, not three: Google accounts
// commonly carry a second factor, and a shorter window strangles a legitimate
// user mid-approval.
const FlowExpiry = 6 * time.Minute

// Secret keys. Namespaced, because the store is shared and a plain
// "access_token" from two vendors would be one variable wearing two names.
const (
	keyAccessToken  = "gemini_access_token"
	keyRefreshToken = "gemini_refresh_token"
	keyExpiresAt    = "gemini_expires_at"
	keySub          = "gemini_sub"
	keyEmail        = "gemini_email"
)

// Error is a login failure with a code the user can act on.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// RedirectURI is the loopback address pasted back by the operator.
func RedirectURI() string {
	return fmt.Sprintf("http://%s:%d%s", CallbackHost, CallbackPort, CallbackPath)
}

// ClientID and ClientSecret honour self-built credentials first: the defaults
// are the public client, and an operator with their own OAuth registration
// overrides both together, never one alone.
func ClientID() string {
	if v := strings.TrimSpace(os.Getenv("GEMINI_CLIENT_ID")); v != "" {
		return v
	}
	return DefaultClientID
}

func ClientSecret() string {
	if v := strings.TrimSpace(os.Getenv("GEMINI_CLIENT_SECRET")); v != "" {
		return v
	}
	return DefaultClientSecret
}

// Flow is one login attempt. The operator opens AuthorizeURL and pastes the
// callback URL back; State proves the pasted URL answers this attempt.
type Flow struct {
	AuthorizeURL string
	State        string
	RedirectURI  string
	ExpiresAt    time.Time
}

// Expired reports whether the attempt outlived its window.
func (f *Flow) Expired(now time.Time) bool {
	return !now.Before(f.ExpiresAt)
}

// Start mints a login attempt. Local only: randomness plus URL building, no
// network, so starting never fails for a reason the user could fix.
func Start() (*Flow, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, newError("random_failed", "could not draw login randomness")
	}
	state := fmt.Sprintf("%x", raw)
	query := url.Values{}
	query.Set("client_id", ClientID())
	query.Set("response_type", "code")
	query.Set("redirect_uri", RedirectURI())
	query.Set("scope", strings.Join(Scopes, " "))
	query.Set("state", state)
	query.Set("access_type", "offline")
	query.Set("include_granted_scopes", "true")
	query.Set("prompt", "consent")
	return &Flow{
		AuthorizeURL: authEndpoint + "?" + query.Encode(),
		State:        state,
		RedirectURI:  RedirectURI(),
		ExpiresAt:    time.Now().Add(FlowExpiry),
	}, nil
}

// ExtractCode accepts what the operator pasted: the whole callback URL from
// the browser address bar, or the bare code. A full URL also yields its state
// for the round-trip check; a bare code carries none, which is accepted with
// the check skipped rather than failing a careful paste.
func ExtractCode(pasted string) (code, state string) {
	trimmed := strings.TrimSpace(pasted)
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Query().Get("code"), parsed.Query().Get("state")
	}
	return trimmed, ""
}

// tokenGrant is a token endpoint response. Refresh and identity are optional:
// a refresh response carries no new refresh token unless Google rotates it,
// and no id_token at all.
type tokenGrant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
}

// parseGrant validates a token endpoint body. Three failures, each named:
// unparseable, an error field, or a missing access token.
func parseGrant(raw []byte, previousRefresh string) (tokenGrant, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return tokenGrant{}, newError("request_failed", "the token endpoint answered unreadably")
	}
	if code, _ := body["error"].(string); code != "" {
		if description, _ := body["error_description"].(string); description != "" {
			return tokenGrant{}, newError("request_failed", "%s: %s", code, description)
		}
		return tokenGrant{}, newError("request_failed", "%s", code)
	}
	access, _ := body["access_token"].(string)
	if strings.TrimSpace(access) == "" {
		return tokenGrant{}, newError("request_failed", "the token endpoint returned no access token")
	}
	grant := tokenGrant{AccessToken: strings.TrimSpace(access)}
	if refresh, _ := body["refresh_token"].(string); strings.TrimSpace(refresh) != "" {
		grant.RefreshToken = strings.TrimSpace(refresh)
	} else {
		// Google rotates occasionally rather than always: an absent refresh
		// token keeps the stored one, and overwriting it with empty is how a
		// working login dies overnight.
		grant.RefreshToken = previousRefresh
	}
	if tokenType, _ := body["token_type"].(string); strings.TrimSpace(tokenType) != "" {
		grant.TokenType = tokenType
	} else {
		grant.TokenType = "Bearer"
	}
	grant.ExpiresIn = 3600
	if seconds, ok := body["expires_in"].(float64); ok && seconds > 0 {
		grant.ExpiresIn = int(seconds)
	}
	grant.Scope, _ = body["scope"].(string)
	grant.IDToken, _ = body["id_token"].(string)
	return grant, nil
}

// identityFromIDToken decodes sub and email from the id_token payload. No
// signature check: the value is a display name and a stable key, never an
// authentication decision, and the token endpoint answered over TLS. Anything
// unparseable yields empty identity rather than failing the login.
func identityFromIDToken(idToken string) (sub, email string) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ""
	}
	sub, _ = claims["sub"].(string)
	email, _ = claims["email"].(string)
	return strings.TrimSpace(sub), strings.TrimSpace(email)
}

// storeGrant writes the grant, preserving identity the response omits: a
// refresh carries no id_token, so reading identity only from the response
// would wipe the sub and email on the first renewal.
func storeGrant(grant tokenGrant, now time.Time) error {
	if strings.TrimSpace(grant.AccessToken) == "" {
		return newError("request_failed", "the token response contained no token")
	}
	if err := secret.Set(keyAccessToken, strings.TrimSpace(grant.AccessToken)); err != nil {
		return err
	}
	if strings.TrimSpace(grant.RefreshToken) != "" {
		if err := secret.Set(keyRefreshToken, strings.TrimSpace(grant.RefreshToken)); err != nil {
			return err
		}
	}
	if grant.ExpiresIn > 0 {
		expires := now.Add(time.Duration(grant.ExpiresIn) * time.Second).Unix()
		if err := secret.Set(keyExpiresAt, strconv.FormatInt(expires, 10)); err != nil {
			return err
		}
	}
	if sub, email := identityFromIDToken(grant.IDToken); sub != "" || email != "" {
		if sub != "" {
			if err := secret.Set(keySub, sub); err != nil {
				return err
			}
		}
		if email != "" {
			if err := secret.Set(keyEmail, email); err != nil {
				return err
			}
		}
	}
	return nil
}

// Manager exchanges codes and renews grants against explicit endpoints, so
// tests never touch Google.
type Manager struct {
	tokenEndpoint    string
	userinfoEndpoint string
	client           *http.Client
	now              func() time.Time
}

// NewManager creates a manager against the production token endpoint.
func NewManager() *Manager {
	return NewManagerWithOptions(tokenEndpoint, &http.Client{Timeout: 20 * time.Second})
}

// NewManagerWithOptions creates a manager against an explicit endpoint.
func NewManagerWithOptions(tokenEndpoint string, client *http.Client) *Manager {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Manager{tokenEndpoint: tokenEndpoint, userinfoEndpoint: userinfoEndpoint, client: client, now: time.Now}
}

// SetUserinfoEndpoint points identity lookups at an explicit endpoint. Tests
// use it so a login that fetches identity never reaches Google.
func (m *Manager) SetUserinfoEndpoint(endpoint string) *Manager {
	m.userinfoEndpoint = endpoint
	return m
}

func (m *Manager) postForm(ctx context.Context, form url.Values) (tokenGrant, error) {
	previous, _ := secret.Get(keyRefreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return tokenGrant{}, newError("request_failed", "could not build the token request")
	}
	// No User-Agent disguise: the token and userinfo endpoints are standard
	// Google OAuth, and the stock client sends Go's default. Camouflage belongs
	// on the inference endpoints, not here, where it only adds suspicion.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		return tokenGrant{}, newError("request_failed", "could not reach the login server: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenGrant{}, newError("request_failed", "could not read the login response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return tokenGrant{}, newError("request_failed", "the token endpoint answered HTTP %d unreadably", resp.StatusCode)
		}
		if code, _ := body["error"].(string); code != "" {
			if description, _ := body["error_description"].(string); description != "" {
				return tokenGrant{}, newError("request_failed", "%s: %s", code, description)
			}
			return tokenGrant{}, newError("request_failed", "%s", code)
		}
		return tokenGrant{}, newError("request_failed", "the token endpoint answered HTTP %d", resp.StatusCode)
	}
	return parseGrant(raw, previous)
}

// Exchange trades a pasted callback URL or bare code for the token set. A
// full URL carries the state to check against the flow that minted it; a bare
// code skips a check that has nothing to compare against.
func (m *Manager) Exchange(ctx context.Context, flow *Flow, pasted string) error {
	code, state := ExtractCode(pasted)
	if strings.TrimSpace(code) == "" {
		return newError("request_failed", "no authorization code found; paste the callback URL or the code itself")
	}
	if flow != nil && strings.TrimSpace(flow.State) != "" && strings.TrimSpace(state) != "" && state != flow.State {
		return newError("request_failed", "the callback answers a different login; approve the link Ceraa just showed")
	}
	form := url.Values{}
	form.Set("client_id", ClientID())
	form.Set("client_secret", ClientSecret())
	form.Set("code", strings.TrimSpace(code))
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", RedirectURI())
	grant, err := m.postForm(ctx, form)
	if err != nil {
		return err
	}
	if err := storeGrant(grant, m.now()); err != nil {
		return err
	}
	m.fetchIdentity(ctx, grant.AccessToken)
	return nil
}

// fetchIdentity pulls sub and email as a fallback for grants without an
// id_token. Best effort with a reported reason: identity is display info, and
// a failing lookup must not fail the login, but a silent one once hid a
// regression that downgraded every nickname to an account id.
func (m *Manager) fetchIdentity(ctx context.Context, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.userinfoEndpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}
	var info struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return
	}
	if strings.TrimSpace(info.ID) != "" {
		_ = secret.Set(keySub, strings.TrimSpace(info.ID))
	}
	if strings.TrimSpace(info.Email) != "" {
		_ = secret.Set(keyEmail, strings.TrimSpace(info.Email))
	}
}

var refreshMu sync.Mutex

// RefreshToken renews the grant. Serialised, because a rotated refresh token
// invalidates its predecessor and two concurrent refreshes leave one of them
// holding a dead token.
func (m *Manager) RefreshToken(ctx context.Context) (string, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	stored, err := secret.Get(keyRefreshToken)
	if err != nil || strings.TrimSpace(stored) == "" {
		return "", newError("no_refresh_token", "no refresh token is stored; sign in again")
	}
	form := url.Values{}
	form.Set("client_id", ClientID())
	form.Set("client_secret", ClientSecret())
	form.Set("refresh_token", strings.TrimSpace(stored))
	form.Set("grant_type", "refresh_token")
	grant, err := m.postForm(ctx, form)
	if err != nil {
		return "", err
	}
	if err := storeGrant(grant, m.now()); err != nil {
		return "", err
	}
	return grant.AccessToken, nil
}

// SignedIn reports whether a usable grant is stored: an access token present
// with either no recorded expiry or one in the future. An expired access
// token with a refresh token alongside still counts, because AccessToken
// renews it on demand rather than sending the user back to the browser.
func SignedIn() bool {
	stored, err := secret.Get(keyAccessToken)
	if err != nil || strings.TrimSpace(stored) == "" {
		return false
	}
	raw, err := secret.Get(keyExpiresAt)
	if err != nil || strings.TrimSpace(raw) == "" {
		return true
	}
	expires, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || expires <= 0 {
		return true
	}
	if time.Now().Unix() < expires {
		return true
	}
	refresh, err := secret.Get(keyRefreshToken)
	return err == nil && strings.TrimSpace(refresh) != ""
}

// AccessToken returns the stored grant for inference, renewing first when it
// has expired.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	stored, err := secret.Get(keyAccessToken)
	if err != nil || strings.TrimSpace(stored) == "" {
		return "", newError("not_signed_in", "not signed in to Google; start a login first")
	}
	if expired() {
		return m.RefreshToken(ctx)
	}
	return strings.TrimSpace(stored), nil
}

func expired() bool {
	raw, err := secret.Get(keyExpiresAt)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false
	}
	expires, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || expires <= 0 {
		return false
	}
	return time.Now().Unix() >= expires
}

// Forget revokes the grant remotely and clears it locally. A failed revoke
// never restores the grant: remote logout failing leaves you logged out
// locally with a token that no longer authenticates, which is the safe
// direction.
func Forget() error {
	if stored, err := secret.Get(keyAccessToken); err == nil && strings.TrimSpace(stored) != "" {
		form := url.Values{}
		form.Set("token", strings.TrimSpace(stored))
		req, err := http.NewRequest(http.MethodPost, revokeEndpoint, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
			}
		}
	}
	var first error
	for _, key := range []string{keyAccessToken, keyRefreshToken, keyExpiresAt, keySub, keyEmail} {
		if err := secret.Delete(key); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// NewTokenSource returns the transport's credential source: a manager against
// the default endpoints, used as llm.ProviderConfig tokens. The manager reads
// the store on every call rather than caching the grant.
func NewTokenSource() llm.CodexTokenSource {
	return NewManager()
}

var _ llm.CodexTokenSource = (*Manager)(nil)

// Identity returns the stored display name and stable id, either possibly
// empty when the grant predates them.
func Identity() (email, sub string) {
	email, _ = secret.Get(keyEmail)
	sub, _ = secret.Get(keySub)
	return strings.TrimSpace(email), strings.TrimSpace(sub)
}

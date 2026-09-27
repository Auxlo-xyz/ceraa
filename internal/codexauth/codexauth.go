// Package codexauth implements ChatGPT Codex sign-in for Ceraa.
//
// OpenAI's Codex device authorization is not RFC 8628. A bespoke pair of
// endpoints mints a short user code, the user types it at a verification page,
// and a poll hands back an authorization code together with the PKCE verifier
// minted alongside it. That pair is then exchanged at the ordinary OAuth token
// endpoint, so the resulting credentials are the same as a browser redirect
// would produce.
//
// The flow is done over HTTP rather than by driving the `codex` CLI. Shelling out
// means parsing a human-readable prompt off a process that blocks until a person
// acts, and it makes the login impossible on any host that does not happen to
// have the binary installed.
package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/internal/secret"
)

// Endpoints for the device flow and the token exchange.
const (
	userCodeURL    = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL = "https://auth.openai.com/api/accounts/deviceauth/token"
	oauthTokenURL  = "https://auth.openai.com/oauth/token"
	// verificationURL is where the user types the code. It is not the same host
	// path as the API endpoints.
	verificationURL = "https://auth.openai.com/codex/device"
	// redirectURI the device-issued authorization code is bound to. It must match
	// exactly or the exchange is rejected.
	redirectURI = "https://auth.openai.com/deviceauth/callback"

	// defaultClientID is the published Codex client. It is a public client, so
	// there is no secret to keep.
	defaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// pendingCode is what the poll returns while the user has not yet approved.
	pendingCode = "deviceauth_authorization_pending"

	// defaultPollSeconds is used when the server omits or garbles the interval.
	defaultPollSeconds = 5
	// maxLifetime caps a flow regardless of what the server claims, because a
	// device code that outlives this has stopped being useful.
	maxLifetime = 15 * time.Minute
	// refreshMargin refreshes before expiry rather than after, so a token is never
	// used in the moment it lapses.
	refreshMargin = 5 * time.Minute
)

// Flow status values.
const (
	StatusWaiting   = "waiting"
	StatusApproved  = "approved"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Credential keys in the encrypted store.
const (
	keyAccessToken  = "codex_access_token"
	keyRefreshToken = "codex_refresh_token"
	keyExpiresAt    = "codex_expires_at"
)

// Endpoints holds the URLs, overridable so a test can point the whole flow at a
// local server.
type Endpoints struct {
	UserCode    string
	DeviceToken string
	OAuthToken  string
}

// DefaultEndpoints are the production endpoints.
func DefaultEndpoints() Endpoints {
	return Endpoints{UserCode: userCodeURL, DeviceToken: deviceTokenURL, OAuthToken: oauthTokenURL}
}

// Error is a device-flow failure with a code the user can act on.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsPending reports whether a failure just means the user has not approved yet.
func IsPending(err error) bool {
	var flowErr *Error
	if errors.As(err, &flowErr) {
		return flowErr.Code == pendingCode
	}
	return false
}

// ErrorCode returns the machine-readable code of a flow failure.
func ErrorCode(err error) string {
	var flowErr *Error
	if errors.As(err, &flowErr) {
		return flowErr.Code
	}
	return ""
}

// deviceRequest is what the server mints for a user to approve.
type deviceRequest struct {
	DeviceAuthID    string
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	Interval        time.Duration
}

// tokenResponse is the OAuth token set.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
}

// State is a consistent copy of a flow, taken under the lock.
type State struct {
	FlowID   string
	UserCode string
	AuthURL  string
	Status   string
	Message  string
	// Hints carries the one thing the user most needs to know, such as the
	// account setting that has to be enabled before this can ever succeed.
	Hint string
}

// Flow is one device login in progress.
type Flow struct {
	mu sync.Mutex

	flowID       string
	userCode     string
	authURL      string
	status       string
	message      string
	hint         string
	expiresAt    time.Time
	interval     time.Duration
	deviceAuthID string

	cancel   context.CancelFunc
	done     chan struct{}
	finished bool
}

// Snapshot returns the current state. Every accessor takes the lock, so a caller
// must not hold it across one of them; the lock is not reentrant.
func (f *Flow) Snapshot() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return State{
		FlowID:   f.flowID,
		UserCode: f.userCode,
		AuthURL:  f.authURL,
		Status:   f.status,
		Message:  f.message,
		Hint:     f.hint,
	}
}

// Manager runs device logins.
type Manager struct {
	mu    sync.Mutex
	flows map[string]*Flow

	client     *http.Client
	endpoints  Endpoints
	onComplete func(State)
	clientID   string
	// now and sleep are seams so expiry and the poll cadence can be tested without
	// waiting in real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	// firstPoll is short so the chat reflects approval quickly, then the interval
	// relaxes. A long poll cadence would make a login look stuck.
	firstPoll time.Duration
}

// NewManager creates a manager for the real endpoints.
func NewManager() *Manager {
	return NewManagerWithOptions(DefaultEndpoints(), defaultClientID, &http.Client{Timeout: 30 * time.Second})
}

// NewManagerWithOptions creates a manager with explicit endpoints and a client.
func NewManagerWithOptions(endpoints Endpoints, clientID string, client *http.Client) *Manager {
	if clientID == "" {
		clientID = defaultClientID
	}
	return &Manager{
		flows:     make(map[string]*Flow),
		client:    client,
		endpoints: endpoints,
		clientID:  clientID,
		now:       time.Now,
		sleep:     sleepContext,
		firstPoll: 1 * time.Second,
	}
}

// Start mints a user code and begins polling for approval in the background.
//
// It returns as soon as the code is minted, which is the whole point: the user
// needs the code before they can do anything, and the approval takes minutes.
func (m *Manager) Start() (*Flow, error) {
	request, err := m.requestDeviceCode(context.Background())
	if err != nil {
		return nil, err
	}
	flow := &Flow{
		flowID:       fmt.Sprintf("codex-%d", m.now().UnixNano()),
		userCode:     request.UserCode,
		authURL:      request.VerificationURL,
		status:       StatusWaiting,
		message:      "Waiting for you to approve the code",
		expiresAt:    request.ExpiresAt,
		interval:     request.Interval,
		deviceAuthID: request.DeviceAuthID,
		done:         make(chan struct{}),
	}
	// The account must have device authorization enabled, and when it is not the
	// poll simply never succeeds. Saying so up front is the difference between a
	// user fixing it and a user giving up.
	flow.hint = "ChatGPT security settings must have \"Enable device code authorization for Codex\" turned on."

	m.mu.Lock()
	m.flows[flow.flowID] = flow
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	flow.cancel = cancel
	go m.watch(ctx, flow)

	return flow, nil
}

// requestDeviceCode mints the code the user types.
func (m *Manager) requestDeviceCode(ctx context.Context) (deviceRequest, error) {
	var body struct {
		ClientID     string `json:"client_id"`
		ExpiresAt    string `json:"expires_at"`
		Interval     any    `json:"interval"`
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}
	body.ClientID = m.clientID
	if err := m.postJSON(ctx, m.endpoints.UserCode, body, &body); err != nil {
		return deviceRequest{}, err
	}
	if body.DeviceAuthID == "" || body.UserCode == "" {
		return deviceRequest{}, newError("request_failed",
			"the device authorization response was missing a device id or user code")
	}
	expires := m.now().Add(maxLifetime)
	if parsed, err := time.Parse(time.RFC3339, body.ExpiresAt); err == nil && parsed.After(m.now()) {
		if parsed.Before(expires) {
			expires = parsed
		}
	}
	return deviceRequest{
		DeviceAuthID:    body.DeviceAuthID,
		UserCode:        body.UserCode,
		VerificationURL: verificationURL,
		ExpiresAt:       expires,
		Interval:        pollInterval(body.Interval),
	}, nil
}

// watch polls for approval, then exchanges the code for tokens.
func (m *Manager) watch(ctx context.Context, flow *Flow) {
	interval := m.firstPoll
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if m.now().After(flow.snapshotExpiresAt()) {
			m.finish(flow, StatusFailed, "The code expired before it was approved", "")
			return
		}
		code, verifier, err := m.pollForCode(ctx, flow)
		switch {
		case err == nil:
			if exchangeErr := m.exchange(ctx, code, verifier); exchangeErr != nil {
				m.finish(flow, StatusFailed, exchangeErr.Error(), "")
				return
			}
			m.finish(flow, StatusSucceeded, "Login successful", "")
			return
		case IsPending(err):
			// Expected while the user has not acted yet.
		case ctx.Err() != nil:
			return
		default:
			// A terminal provider failure, most often the account setting being
			// off. Say which, rather than reporting a generic failure.
			hint := ""
			if ErrorCode(err) == "deviceauth_disabled" || strings.Contains(err.Error(), "device") {
				hint = "Check that \"Enable device code authorization for Codex\" is on in ChatGPT security settings."
			}
			m.finish(flow, StatusFailed, err.Error(), hint)
			return
		}
		if err := m.sleep(ctx, interval); err != nil {
			return
		}
		if interval < flow.snapshotInterval() {
			interval = flow.snapshotInterval()
		}
	}
}

// pollForCode asks the device endpoint whether the code has been approved.
func (m *Manager) pollForCode(ctx context.Context, flow *Flow) (code, verifier string, err error) {
	payload := map[string]string{
		"device_auth_id": flow.deviceAuthID,
		"user_code":      flow.userCode,
	}
	var response struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	raw, status, err := m.postRaw(ctx, m.endpoints.DeviceToken, encoded)
	if err != nil {
		// A transport failure is not a decision by the user, so keep waiting.
		return "", "", err
	}
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &response); err != nil {
			return "", "", newError("request_failed", "could not read the authorization response")
		}
		if response.AuthorizationCode == "" || response.CodeVerifier == "" {
			return "", "", newError("request_failed", "approval succeeded but no authorization code was returned")
		}
		return response.AuthorizationCode, response.CodeVerifier, nil
	}
	// Approval outstanding answers 403, occasionally 404, with the pending code.
	// Only a different code is terminal.
	providerCode, message := readProviderError(raw, status)
	if providerCode == pendingCode || (providerCode == "" && message == "") {
		return "", "", newError(pendingCode, "waiting for approval")
	}
	if status == http.StatusTooManyRequests || status >= 500 {
		return "", "", newError(pendingCode, "waiting for approval")
	}
	if message == "" {
		message = fmt.Sprintf("device authorization failed (HTTP %d)", status)
	}
	return "", "", newError(providerCode, "%s", message)
}

// exchange trades the device-issued authorization code and its PKCE verifier for
// tokens, at the ordinary OAuth endpoint.
func (m *Manager) exchange(ctx context.Context, code, verifier string) error {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	form.Set("client_id", m.clientID)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.OAuthToken, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := m.client.Do(request)
	if err != nil {
		return newError("request_failed", "could not reach the token endpoint: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 300 {
		code, message := readProviderError(raw, response.StatusCode)
		if message == "" {
			message = fmt.Sprintf("token exchange failed (HTTP %d)", response.StatusCode)
		}
		return newError(code, "%s", message)
	}
	var tokens tokenResponse
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return newError("request_failed", "could not read the token response")
	}
	return storeTokens(tokens, m.now())
}

// storeTokens writes the token set to the encrypted store. The refresh token is
// written even when empty-checked below, because OpenAI rotates it and the new one
// must replace the old.
func storeTokens(tokens tokenResponse, now time.Time) error {
	if tokens.AccessToken == "" {
		return newError("request_failed", "the token response contained no access token")
	}
	if err := secret.Set(keyAccessToken, tokens.AccessToken); err != nil {
		return err
	}
	if tokens.RefreshToken != "" {
		if err := secret.Set(keyRefreshToken, tokens.RefreshToken); err != nil {
			return err
		}
	}
	if tokens.ExpiresIn > 0 {
		expires := now.Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix()
		if err := secret.Set(keyExpiresAt, strconv.FormatInt(expires, 10)); err != nil {
			return err
		}
	}
	return nil
}

// refreshMu serialises refreshes.
//
// OpenAI rotates the refresh token on every use, so two concurrent refreshes
// invalidate each other and one of them is left holding a dead token. Whoever
// arrives second waits for the first rather than starting its own exchange.
var refreshMu sync.Mutex

// AccessToken returns a valid token for the manager's endpoints, refreshing it if
// it is close to expiry. It returns an empty string when nothing is stored.
//
// The refresh lives on the manager rather than only as a package function so it
// uses the configured token endpoint; a hardcoded URL cannot be pointed
// anywhere, which makes the whole path untestable.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	stored, err := secret.Get(keyAccessToken)
	if err != nil || stored == "" {
		return "", nil
	}
	fresh, err := tokenIsFresh()
	if err != nil || fresh {
		// No usable expiry recorded, so the token is used as-is rather than
		// refreshed on every call.
		return stored, nil
	}

	refreshMu.Lock()
	defer refreshMu.Unlock()

	// Re-check under the lock: whoever refreshed while we waited has already done
	// the work, and OpenAI rotates the refresh token on every use, so a second
	// exchange would invalidate the first and leave us holding a dead token.
	stored, err = secret.Get(keyAccessToken)
	if err != nil || stored == "" {
		return "", nil
	}
	if fresh, err := tokenIsFresh(); err == nil && fresh {
		return stored, nil
	}
	refreshed, refreshErr := m.RefreshToken(ctx)
	if refreshErr != nil {
		// A refresh failure is not a reason to lock the user out of a token that
		// may still work, so the existing one is returned.
		return stored, nil
	}
	return refreshed, nil
}

// tokenIsFresh reports whether the stored expiry is comfortably in the future.
func tokenIsFresh() (bool, error) {
	raw, err := secret.Get(keyExpiresAt)
	if err != nil || raw == "" {
		return false, err
	}
	expires, convErr := strconv.ParseInt(raw, 10, 64)
	if convErr != nil {
		return false, convErr
	}
	// Refreshed before expiry rather than after, so a token is never used in the
	// moment it lapses.
	return time.Now().Before(time.Unix(expires, 0).Add(-refreshMargin)), nil
}

// RefreshToken exchanges the stored refresh token for a new token set.
func (m *Manager) RefreshToken(ctx context.Context) (string, error) {
	refresh, err := secret.Get(keyRefreshToken)
	if err != nil || refresh == "" {
		return "", newError("not_authenticated", "no Codex refresh token is stored")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", m.clientID)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.OAuthToken, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return "", newError("request_failed", "could not reach the token endpoint: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 300 {
		code, message := readProviderError(raw, response.StatusCode)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", response.StatusCode)
		}
		return "", newError(code, "refresh failed: %s", message)
	}
	var tokens tokenResponse
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return "", newError("request_failed", "could not read the refresh response")
	}
	if err := storeTokens(tokens, time.Now()); err != nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

// AccessToken is the production convenience wrapper, using the real endpoints.
func AccessToken(ctx context.Context) (string, error) {
	return NewManager().AccessToken(ctx)
}

// Authenticated reports whether a usable token is stored.
func Authenticated() bool {
	value, err := secret.Get(keyAccessToken)
	return err == nil && value != ""
}

// Forget removes the stored credentials. Deleting a key that is not there is a
// no-op, so this is safe to call when nothing is signed in.
func Forget() error {
	for _, key := range []string{keyAccessToken, keyRefreshToken, keyExpiresAt} {
		if err := secret.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) finish(flow *Flow, status, message, hint string) {
	flow.mu.Lock()
	if flow.finished {
		flow.mu.Unlock()
		return
	}
	flow.finished = true
	flow.status = status
	flow.message = message
	flow.hint = hint
	if flow.cancel != nil {
		flow.cancel()
	}
	close(flow.done)
	flow.mu.Unlock()

	m.mu.Lock()
	delete(m.flows, flow.flowID)
	onComplete := m.onComplete
	m.mu.Unlock()

	// Report the terminal state before returning.
	//
	// This has to be a callback rather than something a caller polls for, because
	// the flow is removed from the registry the instant it finishes. A poller
	// therefore never observes a completed login: it either misses it entirely or
	// finds the flow gone and concludes nothing happened. That is not a race to be
	// tuned, it is a certainty, and it is why a "check the status" handler can
	// never report success.
	//
	// Invoked outside both locks so a callback that touches the manager cannot
	// deadlock, and synchronously so the outcome is delivered before the watching
	// goroutine exits.
	if onComplete != nil {
		onComplete(flow.Snapshot())
	}
}

func (f *Flow) snapshotInterval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.interval <= 0 {
		return defaultPollSeconds * time.Second
	}
	return f.interval
}

func (f *Flow) snapshotExpiresAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expiresAt
}

// Cancel stops a flow. It is idempotent, because the cancel button remains on
// screen after a flow ends and a second tap is a normal thing to do.
func (m *Manager) Cancel(flowID string) error {
	flow, err := m.lookup(flowID)
	if err != nil {
		return nil
	}
	m.finish(flow, StatusCanceled, "Login cancelled", "")
	return nil
}

// SetOnComplete registers a callback invoked once when a flow reaches a terminal
// state: succeeded, failed or cancelled.
//
// A caller that needs to know how a login ended must use this. Polling Check
// cannot work, because a finished flow is no longer in the registry, so the
// poller sees either "in progress" or "no such flow" and never the outcome.
func (m *Manager) SetOnComplete(fn func(State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onComplete = fn
}

// Check returns a live flow.
//
// A flow that has already finished is not live and returns an error. Callers
// wanting the terminal state should have kept the State delivered to their
// SetOnComplete callback.
func (m *Manager) Check(flowID string) (*Flow, error) { return m.lookup(flowID) }

func (m *Manager) lookup(flowID string) (*Flow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	flow, ok := m.flows[flowID]
	if !ok {
		return nil, fmt.Errorf("flow %q is not active", flowID)
	}
	return flow, nil
}

// ListFlows returns flows still in progress.
func (m *Manager) ListFlows() []*Flow {
	m.mu.Lock()
	defer m.mu.Unlock()
	flows := make([]*Flow, 0, len(m.flows))
	for _, flow := range m.flows {
		flows = append(flows, flow)
	}
	return flows
}

// ResultMessage renders a flow for a chat.
func (f *Flow) ResultMessage() string { return renderStatus(f.Snapshot()) }

func renderStatus(state State) string {
	var b strings.Builder
	switch state.Status {
	case StatusSucceeded:
		b.WriteString("✅ Codex login successful. Codex can now be used as an agent.")
	case StatusFailed:
		fmt.Fprintf(&b, "❌ Codex login failed: %s", state.Message)
	case StatusCanceled:
		b.WriteString("✗ Login cancelled.")
	default:
		b.WriteString("🔐 Codex Device Login\n\n")
		fmt.Fprintf(&b, "1. Open %s\n", state.AuthURL)
		fmt.Fprintf(&b, "2. Enter code: <code>%s</code>\n", state.UserCode)
		b.WriteString("3. Approve it, then tap 'Check Login Status'")
	}
	if state.Hint != "" {
		b.WriteString("\n\n")
		b.WriteString(state.Hint)
	}
	return b.String()
}

// InlineKeyboardJSON is the Telegram keyboard for a flow.
//
// Every state gets a way forward. The keyboard used to be cleared on failure and
// on cancel, which left the user looking at a dead message with nothing to press
// and no route back to the provider menu except typing a command.
func (f *Flow) InlineKeyboardJSON() ([]byte, error) {
	return KeyboardJSONForState(f.Snapshot())
}

// KeyboardJSONForState renders the keyboard for a state.
//
// Exported because a caller holding a State rather than a Flow still needs to
// offer the right buttons, and because the state-to-keyboard mapping is the part
// worth testing directly: it is where a dead end hides.
func KeyboardJSONForState(state State) ([]byte, error) {
	switch state.Status {
	case StatusSucceeded:
		// Nothing left to approve. Offer the switch rather than a check that
		// would report success again.
		return json.Marshal(map[string]any{
			"inline_keyboard": [][]map[string]any{
				{{"text": "✅ Use Codex now", "callback_data": "codex:activate"}},
			},
		})
	case StatusFailed:
		return json.Marshal(map[string]any{
			"inline_keyboard": [][]map[string]any{
				{{"text": "🔁 Try again", "callback_data": "codex:login"}},
			},
		})
	case StatusCanceled:
		return json.Marshal(map[string]any{
			"inline_keyboard": [][]map[string]any{
				{{"text": "🔁 Start sign-in again", "callback_data": "codex:login"}},
			},
		})
	}
	return json.Marshal(map[string]any{
		"inline_keyboard": [][]map[string]any{
			{{"text": "🔗 Open Authorization Page", "url": state.AuthURL}},
			{
				{"text": "✓ Check Login Status", "callback_data": "codex:check:" + state.FlowID},
				{"text": "✗ Cancel", "callback_data": "codex:cancel:" + state.FlowID},
			},
		},
	})
}

// postJSON sends and receives JSON in one call.
func (m *Manager) postJSON(ctx context.Context, url string, in, out any) error {
	encoded, err := json.Marshal(in)
	if err != nil {
		return err
	}
	raw, status, err := m.postRaw(ctx, url, encoded)
	if err != nil {
		return err
	}
	if status >= 300 {
		code, message := readProviderError(raw, status)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", status)
		}
		return newError(code, "%s", message)
	}
	return json.Unmarshal(raw, out)
}

// postRaw sends an already-encoded body. It must not encode again: doing so
// turned a JSON body into a base64 string, which the API rejects.
func (m *Manager) postRaw(ctx context.Context, url string, encoded []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return raw, response.StatusCode, nil
}

// readProviderError pulls { "error": { "code", "message" } } off a failure body.
// An unparseable body reads as transient, so a flow keeps waiting rather than
// failing on something it cannot understand.
func readProviderError(raw []byte, status int) (code, message string) {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", ""
	}
	code = firstNonEmpty(envelope.Error.Code, envelope.Code)
	message = firstNonEmpty(envelope.Error.Message, envelope.Message)
	if code == "" && message == "" && status == 0 {
		return "", ""
	}
	return code, message
}

// pollInterval reads the server's cadence, which arrives as either a number or a
// string of seconds.
func pollInterval(raw any) time.Duration {
	var seconds float64
	switch value := raw.(type) {
	case float64:
		seconds = value
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			seconds = 0
		} else {
			seconds = parsed
		}
	}
	if seconds <= 0 {
		seconds = defaultPollSeconds
	}
	return time.Duration(seconds * float64(time.Second))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Package oauthdevice runs an OAuth 2.0 device-authorization login and keeps the
// resulting token fresh.
//
// It exists because two different agents both authenticate this way. The Nous
// Portal and xAI each publish a device-code flow with the same shape and nothing
// else in common: different issuer, different client id, different scope. The
// poll loop, the slow_down handling, the terminal states and the refresh are
// identical, so they live here once and the provider packages supply only their
// own constants.
//
// The terminal-state callback is not optional sugar. A finished flow is no
// longer observable by polling, so a caller that needs to know how a login ended
// has to be told. A status poller sees either "in progress" or "no such flow" and
// never the outcome.
package oauthdevice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GrantTypeDeviceCode is the RFC 8628 device authorization grant.
const GrantTypeDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"

// Terminal login states.
const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Defaults for the poll cadence, used when the server does not say.
const (
	DefaultInterval    = 5 * time.Second
	MinInterval        = 1 * time.Second
	SlowDownIncrement  = 5 * time.Second
	DefaultFlowTimeout = 30 * time.Minute
)

// MaxResponseBytes bounds what a token endpoint can make this process allocate.
const MaxResponseBytes = 1 << 20

// Endpoints are the three URLs a device flow needs. They are supplied rather
// than discovered here because discovery is a trust decision: a discovery
// document can name any host, and a login that trusts it will happily post a
// client secret and a user code to whatever it is told. Each provider package
// passes the endpoints it has verified.
type Endpoints struct {
	DeviceCode string
	Token      string
}

// Config is everything provider-specific about a device login.
type Config struct {
	// ProviderID is the id stored alongside the credential, e.g. "xai".
	ProviderID string
	ClientID   string
	Scope      string
	Endpoints  Endpoints
	// HTTPClient is optional; a 20s-timeout client is used when nil.
	HTTPClient *http.Client
	// Now is optional, for tests.
	Now func() time.Time
}

// Token is a successful device-flow result.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    time.Time
	// Scope is echoed by the server and is worth keeping: the refresh path needs
	// it to tell whether offline_access was actually granted, which is the usual
	// reason a refresh token never arrives.
	Scope string
	// Raw is the decoded response, because providers hang their own fields off it
	// and a caller that cannot see them cannot use them.
	Raw map[string]any
}

// Expired reports whether the token is past its expiry, allowing for skew.
//
// A zero expiry means "no expiry known", not "expired": a provider that omits
// expires_in still issues a usable token, and treating it as expired would make
// every request try to refresh a credential it cannot refresh.
func (t Token) Expired(skew time.Duration) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return !t.ExpiresAt.After(time.Now().Add(skew))
}

// PollError is a device-flow error that carries the OAuth error code, so the
// caller can distinguish "the user has not approved yet" from a real failure.
//
// Without the code, a pending login and a rejected one look identical and the
// user is told the login failed while it is still waiting for them.
type PollError struct {
	Code    string
	Message string
	Status  int
}

func (e *PollError) Error() string { return e.Message }

// IsPending reports whether the error means "keep waiting".
func (e *PollError) IsPending() bool {
	return e.Code == "authorization_pending" || e.Code == "slow_down"
}

// Flow is one in-flight login.
type Flow struct {
	mu              sync.RWMutex
	ID              string
	ProviderID      string
	UserCode        string
	VerificationURL string
	Status          string
	Message         string
	ExpiresAt       time.Time
	deviceCode      string
	interval        time.Duration
	cancel          context.CancelFunc
	config          Config
	HTTPClient      *http.Client
}

func (f *Flow) setStatus(status, message string) {
	f.mu.Lock()
	f.Status = status
	f.Message = message
	f.mu.Unlock()
}

// Snapshot is a copy of the mutable state, safe to read from another goroutine.
type Snapshot struct {
	ID              string
	ProviderID      string
	UserCode        string
	VerificationURL string
	Status          string
	Message         string
	ExpiresAt       time.Time
}

// Snapshot returns the flow's current state.
func (f *Flow) Snapshot() Snapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Snapshot{
		ID:              f.ID,
		ProviderID:      f.ProviderID,
		UserCode:        f.UserCode,
		VerificationURL: f.VerificationURL,
		Status:          f.Status,
		Message:         f.Message,
		ExpiresAt:       f.ExpiresAt,
	}
}

// OnComplete receives each terminal state exactly once. It is called outside
// every lock, so it may call back into the Manager.
type OnComplete func(Snapshot)

// Manager runs device logins for one provider.
type Manager struct {
	mu         sync.Mutex
	config     Config
	flows      map[string]*Flow
	onComplete OnComplete
	onToken    OnToken
	nextID     int
}

// NewManager returns a Manager for one provider's device login.
func NewManager(config Config) *Manager {
	return &Manager{config: config, flows: map[string]*Flow{}}
}

// SetOnComplete registers the terminal-state callback.
func (m *Manager) SetOnComplete(fn OnComplete) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onComplete = fn
}

// Config returns the provider configuration this manager was built with.
func (m *Manager) Config() Config { return m.config }

func (m *Manager) client() *http.Client {
	if m.config.HTTPClient != nil {
		return m.config.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (m *Manager) now() time.Time {
	if m.config.Now != nil {
		return m.config.Now()
	}
	return time.Now()
}

// Start begins a device login and polls in the background until it reaches a
// terminal state or expires.
func (m *Manager) Start(ctx context.Context) (*Flow, error) {
	device, err := m.requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}
	flowCtx, cancel := context.WithCancel(context.Background())
	// Detached from ctx on purpose: a device login outlives the tap that started
	// it, because the user still has to approve the code in a browser. Cancelling
	// on the request context would abandon the login the moment the HTTP handler
	// returned.
	_ = ctx

	interval := DefaultInterval
	if device.Interval > 0 {
		interval = time.Duration(device.Interval) * time.Second
	}
	if interval < MinInterval {
		interval = MinInterval
	}
	timeout := DefaultFlowTimeout
	if device.ExpiresIn > 0 {
		timeout = time.Duration(device.ExpiresIn) * time.Second
	}

	m.mu.Lock()
	m.nextID++
	id := fmt.Sprintf("%s-%d-%d", m.config.ProviderID, m.now().Unix(), m.nextID)
	m.mu.Unlock()

	flow := &Flow{
		ID:              id,
		ProviderID:      m.config.ProviderID,
		UserCode:        device.UserCode,
		VerificationURL: device.VerificationURIComplete,
		Status:          StatusPending,
		deviceCode:      device.DeviceCode,
		interval:        interval,
		ExpiresAt:       m.now().Add(timeout),
		cancel:          cancel,
		config:          m.config,
		HTTPClient:      m.client(),
	}
	if flow.VerificationURL == "" {
		flow.VerificationURL = device.VerificationURI
	}

	m.mu.Lock()
	m.flows[id] = flow
	m.mu.Unlock()

	go m.poll(flowCtx, flow)
	return flow, nil
}

// Check returns a live flow. A flow that has already finished is not live, which
// is why completion is reported by callback and not discovered by polling.
func (m *Manager) Check(flowID string) (*Flow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	flow, ok := m.flows[flowID]
	if !ok {
		return nil, fmt.Errorf("no such login flow %q", flowID)
	}
	return flow, nil
}

// ListFlows returns every flow this manager knows about.
func (m *Manager) ListFlows() []*Flow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Flow, 0, len(m.flows))
	for _, flow := range m.flows {
		out = append(out, flow)
	}
	return out
}

// Cancel stops a login the user abandoned.
func (m *Manager) Cancel(flowID string) error {
	flow, err := m.Check(flowID)
	if err != nil {
		return err
	}
	flow.cancel()
	flow.setStatus(StatusCanceled, "Login cancelled")
	m.finish(flow, StatusCanceled, "Login cancelled")
	return nil
}

func (m *Manager) finish(flow *Flow, status, message string) {
	m.mu.Lock()
	onComplete := m.onComplete
	m.mu.Unlock()
	if onComplete == nil {
		return
	}
	onComplete(flow.Snapshot())
}

func (m *Manager) poll(ctx context.Context, flow *Flow) {
	for {
		if m.now().After(flow.ExpiresAt) {
			flow.setStatus(StatusFailed, "Authorization expired")
			m.finish(flow, StatusFailed, "Authorization expired")
			return
		}
		token, err := m.pollToken(ctx, flow.deviceCode)
		if err == nil {
			m.succeed(flow, token)
			return
		}
		var pending *PollError
		if !errors.As(err, &pending) {
			flow.setStatus(StatusFailed, err.Error())
			m.finish(flow, StatusFailed, err.Error())
			return
		}
		switch pending.Code {
		case "authorization_pending":
			// Keep waiting.
		case "slow_down":
			flow.mu.Lock()
			flow.interval += SlowDownIncrement
			flow.mu.Unlock()
		default:
			flow.setStatus(StatusFailed, pending.Message)
			m.finish(flow, StatusFailed, pending.Message)
			return
		}
		flow.mu.RLock()
		wait := flow.interval
		flow.mu.RUnlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// OnToken receives a successful token so the caller can persist it. Without this
// the login would report success and then have nowhere to put the credential.
type OnToken func(flowID string, token Token) error

// SetOnToken registers the persist hook.
func (m *Manager) SetOnToken(fn OnToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onToken = fn
}

func (m *Manager) succeed(flow *Flow, token Token) {
	if token.AccessToken == "" {
		msg := "Token response did not include access_token"
		flow.setStatus(StatusFailed, msg)
		m.finish(flow, StatusFailed, msg)
		return
	}
	m.mu.Lock()
	onToken := m.onToken
	m.mu.Unlock()
	if onToken != nil {
		if err := onToken(flow.ID, token); err != nil {
			msg := "Could not save login: " + err.Error()
			flow.setStatus(StatusFailed, msg)
			m.finish(flow, StatusFailed, msg)
			return
		}
	}
	flow.setStatus(StatusSucceeded, "Login successful")
	m.finish(flow, StatusSucceeded, "Login successful")
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

func (m *Manager) requestDeviceCode(ctx context.Context) (deviceCodeResponse, error) {
	form := url.Values{
		"client_id": {m.config.ClientID},
		"scope":     {m.config.Scope},
	}
	payload, err := m.postForm(ctx, m.config.Endpoints.DeviceCode, form)
	if err != nil {
		return deviceCodeResponse{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return deviceCodeResponse{}, err
	}
	var device deviceCodeResponse
	if err := json.Unmarshal(encoded, &device); err != nil {
		return deviceCodeResponse{}, err
	}
	if device.DeviceCode == "" || device.UserCode == "" {
		return deviceCodeResponse{}, errors.New("device authorization response is incomplete")
	}
	return device, nil
}

func (m *Manager) pollToken(ctx context.Context, deviceCode string) (Token, error) {
	form := url.Values{
		"grant_type":  {GrantTypeDeviceCode},
		"client_id":   {m.config.ClientID},
		"device_code": {deviceCode},
	}
	payload, err := m.postForm(ctx, m.config.Endpoints.Token, form)
	if err != nil {
		return Token{}, err
	}
	return tokenFromPayload(payload, m.now()), nil
}

// Refresh exchanges a refresh token for a new access token.
func (m *Manager) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return Token{}, errors.New("no refresh token stored")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {m.config.ClientID},
		"refresh_token": {refreshToken},
	}
	payload, err := m.postForm(ctx, m.config.Endpoints.Token, form)
	if err != nil {
		return Token{}, err
	}
	return tokenFromPayload(payload, m.now()), nil
}

func tokenFromPayload(payload map[string]any, now time.Time) Token {
	token := Token{Raw: payload}
	token.AccessToken, _ = payload["access_token"].(string)
	token.RefreshToken, _ = payload["refresh_token"].(string)
	token.TokenType, _ = payload["token_type"].(string)
	token.Scope, _ = payload["scope"].(string)
	if seconds := expirySeconds(payload); seconds > 0 {
		token.ExpiresAt = now.Add(time.Duration(seconds) * time.Second)
	}
	return token
}

func expirySeconds(payload map[string]any) float64 {
	switch value := payload["expires_in"].(type) {
	case float64:
		return value
	case string:
		var seconds float64
		if _, err := fmt.Sscan(value, &seconds); err == nil {
			return seconds
		}
	}
	return 0
}

func (m *Manager) postForm(ctx context.Context, endpoint string, form url.Values) (map[string]any, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := m.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return nil, errors.New("authorization endpoint returned invalid JSON")
		}
		return nil, fmt.Errorf("authorization endpoint returned HTTP %d", response.StatusCode)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return payload, nil
	}
	code, _ := payload["error"].(string)
	description, _ := payload["error_description"].(string)
	if code == "" {
		code = "http_error"
	}
	if description == "" {
		description = fmt.Sprintf("authorization failed with HTTP %d", response.StatusCode)
	}
	return nil, &PollError{Code: code, Message: description, Status: response.StatusCode}
}

// validateEndpoint refuses anything that is not HTTPS with a host.
//
// A token endpoint receives a client id and, on refresh, a refresh token. Being
// handed one over plain HTTP, or with no host at all, is a configuration error
// worth failing on rather than sending a credential anyway.
func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("refusing non-HTTPS authorization endpoint")
	}
	return nil
}

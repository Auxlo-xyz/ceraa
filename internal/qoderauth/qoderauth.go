// Package qoderauth runs Qoder subscription logins and holds the token set.
//
// The protocol was read out of the vendor CLI rather than designed from a
// reference, because Qoder publishes no auth documentation and its public
// OpenAPI host 404s. The relevant JavaScript sits unobfuscated in the packed
// executable, which is also how the poll cadence, the pending signal and the
// CLI client id were established rather than assumed.
package qoderauth

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
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/internal/secret"
)

// defaultClientID is the credential Qoder's own CLI presents on the approval page.
//
// The bundle carries two ids chosen by flag, and reading either one in isolation
// gives the wrong answer. The other id was used first and the page answered
// "Parameter invalid". This is the id from an approval URL of a login that
// actually succeeded, which is the only evidence that counts, so it is the only
// one named here.
const defaultClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

// Endpoints for the production service. The poll and auth hosts came from the
// vendor's endpoint discovery in a live log; the paths came from the bundle.
type Endpoints struct {
	// DeviceSelect is the page the user opens, e.g. https://qoder.com.
	DeviceSelect string
	// OpenAPI is the token API host, e.g. https://openapi.qoder.sh.
	OpenAPI string
}

// DefaultEndpoints are the production endpoints.
func DefaultEndpoints() Endpoints {
	return Endpoints{
		DeviceSelect: "https://qoder.com",
		OpenAPI:      "https://openapi.qoder.sh",
	}
}

func (e Endpoints) pollURL() string {
	return strings.TrimSuffix(e.OpenAPI, "/") + "/api/v1/deviceToken/poll"
}
func (e Endpoints) refreshURL() string {
	return strings.TrimSuffix(e.OpenAPI, "/") + "/api/v1/deviceToken/refresh"
}
func (e Endpoints) userInfoURL() string {
	return strings.TrimSuffix(e.OpenAPI, "/") + "/api/v1/userinfo"
}
func (e Endpoints) authBase() string { return strings.TrimSuffix(e.DeviceSelect, "/") }

// Secret keys. Namespaced rather than bare, the way the other providers are,
// because the store is shared and a plain "refresh_token" from two vendors
// would be one variable wearing two names.
const (
	keyAccessToken  = "qoder_access_token"
	keyRefreshToken = "qoder_refresh_token"
	keyExpiresAt    = "qoder_expires_at"
	keyMachineID    = "qoder_machine_id"
	keyUID          = "qoder_uid"
	keyNickname     = "qoder_nickname"
)

// Error is a device-flow failure with a code the user can act on.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

const pendingCode = "authorization_pending"

// IsPending reports whether a failure just means the user has not approved yet.
func IsPending(err error) bool {
	var flowErr *Error
	if errors.As(err, &flowErr) {
		return flowErr.Code == pendingCode
	}
	return false
}

// pkcePair is the challenge the approval page sees and the verifier that proves
// possession when the token is collected. Generated locally; the server never
// sees the verifier until the poll.
type pkcePair struct {
	verifier  string
	challenge string
}

// newPKCEPair follows the vendor's construction exactly: 43 to 128 characters
// from the unreserved set, then base64url of the SHA-256 digest with padding
// stripped. The length is at the caller's mercy upstream (a verifier under 43
// is rejected), so it is generated correctly here rather than validated there.
func newPKCEPair() (pkcePair, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	length := 43 + randInt(86)
	var b strings.Builder
	b.Grow(length)
	for i := 0; i < length; i++ {
		b.WriteByte(alphabet[randInt(len(alphabet))])
	}
	verifier := b.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return pkcePair{verifier: verifier, challenge: challenge}, nil
}

func randInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// deviceRequest is what the server mints for a user to approve. Built locally
// here rather than fetched from a device-code endpoint, because Qoder has none:
// the approval page takes the PKCE challenge directly. That is the single
// largest structural difference from the standard flow, and it is why the auth
// URL and the poll share a nonce instead of a device code.
type deviceRequest struct {
	Nonce      string
	Challenge  string
	MachineID  string
	AuthURL    string
	Interval   time.Duration
	ExpiresAt  time.Time
	MachineTok string
	verifier   string
}

// tokenResponse is the poll's success body. The token arrives under one of
// three names depending on the endpoint version, so all three are read; the
// identity arrives as user_id/user_name and is needed later, because inference
// requests are signed with the uid and the signer refuses an empty one.
type tokenResponse struct {
	Token        string `json:"token"`
	DeviceToken  string `json:"device_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	ExpireTimeMs int64  `json:"expire_time"`
	TokenType    string `json:"token_type"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
}

// accessToken returns whichever token field the server populated.
func (t tokenResponse) accessToken() string {
	for _, s := range []string{t.Token, t.DeviceToken, t.AccessToken} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// State is a consistent copy of a flow, taken under the lock.
type State struct {
	FlowID  string
	AuthURL string
	Status  string
	Message string
	Hint    string
	Nonce   string
}

// Flow is one device login in progress.
type Flow struct {
	mu sync.Mutex

	flowID     string
	authURL    string
	status     string
	message    string
	hint       string
	nonce      string
	verifier   string
	machineID  string
	machineTok string
	expiresAt  time.Time

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
		FlowID:  f.flowID,
		AuthURL: f.authURL,
		Status:  f.status,
		Message: f.message,
		Hint:    f.hint,
		Nonce:   f.nonce,
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
	// now and sleep are seams so expiry and the poll cadence can be tested
	// without waiting in real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	// The vendor polls every second for five minutes. A longer cadence would make
	// an approval look stuck; a shorter one is a retry in disguise.
	pollInterval time.Duration
	deadline     time.Duration
}

// NewManager creates a manager for the real endpoints.
func NewManager() *Manager {
	return NewManagerWithOptions(DefaultEndpoints(), defaultClientID, &http.Client{Timeout: 30 * time.Second})
}

// NewManagerWithOptions creates a manager against explicit endpoints, for tests
// and for a region that is not the default.
func NewManagerWithOptions(endpoints Endpoints, clientID string, client *http.Client) *Manager {
	if clientID == "" {
		clientID = defaultClientID
	}
	return &Manager{
		flows:        map[string]*Flow{},
		client:       client,
		endpoints:    endpoints,
		clientID:     clientID,
		now:          time.Now,
		sleep:        sleepContext,
		pollInterval: time.Second,
		deadline:     5 * time.Minute,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// machineID returns a stable device identifier, generating and persisting one
// the first time.
//
// This is Ceraa's own machine id, not the vendor CLI's file. Reusing theirs
// would bind two different authorizations to one identifier, and theirs lives in
// a store this process cannot read anyway. A UUID has the same shape and the
// same permanence once written.
func machineID() (string, error) {
	if stored, err := secret.Get(keyMachineID); err == nil && strings.TrimSpace(stored) != "" {
		return strings.TrimSpace(stored), nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	if err := secret.Set(keyMachineID, id); err != nil {
		return "", err
	}
	return id, nil
}

// Start builds the approval URL and the poller for it. Polling begins on Poll,
// not here, so the caller decides when the clock starts.
func (m *Manager) Start() (*Flow, error) {
	pair, err := newPKCEPair()
	if err != nil {
		return nil, newError("pkce_failed", "could not generate the device challenge")
	}
	nonce, err := newUUID()
	if err != nil {
		return nil, newError("pkce_failed", "could not generate the device nonce")
	}
	mid, err := machineID()
	if err != nil {
		return nil, newError("machine_id_failed", "could not read or generate the device identifier")
	}
	query := url.Values{}
	query.Set("challenge", pair.challenge)
	query.Set("challenge_method", "S256")
	query.Set("nonce", nonce)
	query.Set("machine_id", mid)
	query.Set("client_id", m.clientID)
	authURL := m.endpoints.authBase() + "/device/selectAccounts?" + query.Encode()

	_, cancel := context.WithCancel(context.Background())
	flow := &Flow{
		flowID:    fmt.Sprintf("qoder-%d", m.now().UnixNano()),
		authURL:   authURL,
		status:    "pending",
		message:   "approve the login in the browser",
		nonce:     nonce,
		verifier:  pair.verifier,
		machineID: mid,
		expiresAt: m.now().Add(m.deadline),
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	m.mu.Lock()
	m.flows[flow.flowID] = flow
	m.mu.Unlock()
	return flow, nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Poll blocks until the user approves, the flow expires, or the context ends.
//
// The pending signal is an HTTP 404, not a JSON status field. That matters and
// it is not RFC behaviour: a reader that treats any non-2xx as a failure would
// report that the user declined on the very first poll.
func (m *Manager) Poll(ctx context.Context, flow *Flow) error {
	deadline := m.now().Add(m.deadline)
	for {
		raw, status, err := m.pollOnce(ctx, flow)
		if err != nil {
			return err
		}
		switch {
		case status == http.StatusNotFound:
			if !m.now().Before(deadline) {
				flow.setStatus("expired", "the login was not approved in time")
				return newError("expired", "the login was not approved in time; start a new one")
			}
			flow.setStatus("pending", "waiting for approval in the browser")
			if err := m.sleep(ctx, m.pollInterval); err != nil {
				return err
			}
			continue
		case status < 200 || status >= 300:
			flow.setStatus("failed", fmt.Sprintf("the login request failed with status %d", status))
			return newError("request_failed", "the login request failed with status %d", status)
		}
		var tokens tokenResponse
		if err := json.Unmarshal(raw, &tokens); err != nil {
			flow.setStatus("failed", "the token response was not readable")
			return newError("request_failed", "could not read the token response")
		}
		// The vendor's own check: the body is accepted only when a token field is
		// a string. Anything else is still pending or a malformed grant, and both
		// are reported as failures rather than silently accepted.
		if tokens.accessToken() == "" {
			flow.setStatus("failed", "the token response contained no token")
			return newError("request_failed", "the token response contained no token")
		}
		if err := storeTokens(tokens, m.now()); err != nil {
			flow.setStatus("failed", err.Error())
			return err
		}
		// Identity follows the token, best effort: the poll response carries no
		// name, and inference requests are signed with the uid, so a login
		// without identity can authenticate yet never produce a valid request.
		// A userinfo failure does not fail the login; the missing uid surfaces
		// loudly at signing time instead of bricking a good token here.
		m.fetchIdentity(ctx, tokens.accessToken())
		flow.setStatus("completed", "signed in")
		flow.finish()
		if m.onComplete != nil {
			m.onComplete(flow.Snapshot())
		}
		return nil
	}
}

func (m *Manager) pollOnce(ctx context.Context, flow *Flow) ([]byte, int, error) {
	flow.mu.Lock()
	nonce, verifier := flow.nonce, flow.verifier
	flow.mu.Unlock()

	query := url.Values{}
	query.Set("nonce", nonce)
	query.Set("verifier", verifier)
	query.Set("challenge_method", "S256")
	target := m.endpoints.pollURL() + "?" + query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, newError("request_failed", "could not build the poll request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, 0, newError("request_failed", "could not reach the login server: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, newError("request_failed", "could not read the login response")
	}
	return body, resp.StatusCode, nil
}

func (f *Flow) setStatus(status, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.message = status, message
}

// Message and the rest of State are read through Snapshot by callers.
func (f *Flow) Message() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.message
}

func (f *Flow) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.finished {
		f.finished = true
		close(f.done)
	}
}

// storeTokens writes the token set to the encrypted store. The refresh token is
// written whenever it is present, because providers rotate on use and the new one
// must replace the old rather than leaving a stale one beside it. Identity and
// machine id are sticky: a refresh response that omits them must not wipe what
// the login established, because the request signer needs all three.
func storeTokens(tokens tokenResponse, now time.Time) error {
	if tokens.accessToken() == "" {
		return newError("request_failed", "the token response contained no token")
	}
	if err := secret.Set(keyAccessToken, tokens.accessToken()); err != nil {
		return err
	}
	if tokens.RefreshToken != "" {
		if err := secret.Set(keyRefreshToken, tokens.RefreshToken); err != nil {
			return err
		}
	}
	// expire_time arrives as an absolute millisecond timestamp when present;
	// expires_in is relative seconds. A millisecond value below 1e11 would
	// predate 1973, so anything above it is a timestamp, not a duration.
	if tokens.ExpireTimeMs > 1e11 {
		if err := secret.Set(keyExpiresAt, strconv.FormatInt(tokens.ExpireTimeMs/1000, 10)); err != nil {
			return err
		}
	} else if tokens.ExpiresIn > 0 {
		expires := now.Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix()
		if err := secret.Set(keyExpiresAt, strconv.FormatInt(expires, 10)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(tokens.UserID) != "" {
		if err := secret.Set(keyUID, strings.TrimSpace(tokens.UserID)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(tokens.UserName) != "" {
		if err := secret.Set(keyNickname, strings.TrimSpace(tokens.UserName)); err != nil {
			return err
		}
	}
	return nil
}

// fetchIdentity pulls user_id and user_name after a token is stored. The poll
// response carries no name, so without this the credential authenticates but the
// inference signer has no uid to sign with.
func (m *Manager) fetchIdentity(ctx context.Context, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoints.userInfoURL(), nil)
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
	var info tokenResponse
	if err := json.Unmarshal(raw, &info); err != nil {
		return
	}
	if strings.TrimSpace(info.UserID) != "" {
		_ = secret.Set(keyUID, strings.TrimSpace(info.UserID))
	}
	if strings.TrimSpace(info.UserName) != "" {
		_ = secret.Set(keyNickname, strings.TrimSpace(info.UserName))
	}
}

var refreshMu sync.Mutex

// RefreshToken exchanges the stored refresh token for a new set.
//
// The body is JSON carrying the refresh token and this machine's id, with the
// vendor's User-Agent: the endpoint binds the grant to the machine that polled,
// so a standard form-encoded grant is rejected and a refresh from a different
// machine id orphanages the stored one.
//
// Serialised, because a rotated refresh token invalidates its predecessor and
// two concurrent refreshes leave one of them holding a dead token.
func (m *Manager) RefreshToken(ctx context.Context) (string, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	stored, err := secret.Get(keyRefreshToken)
	if err != nil || strings.TrimSpace(stored) == "" {
		return "", newError("no_refresh_token", "no refresh token is stored; sign in again")
	}
	mid, err := machineID()
	if err != nil {
		return "", newError("machine_id_failed", "could not read or generate the device identifier")
	}
	body, err := json.Marshal(map[string]string{
		"refresh_token": strings.TrimSpace(stored),
		"machine_id":    mid,
	})
	if err != nil {
		return "", newError("request_failed", "could not build the refresh request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoints.refreshURL(),
		bytes.NewReader(body))
	if err != nil {
		return "", newError("request_failed", "could not build the refresh request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "qoder/1.0.0")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", newError("request_failed", "could not reach the login server: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", newError("request_failed", "could not read the refresh response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", newError("refresh_failed", "refresh rejected with status %d: %s", resp.StatusCode, firstLine(raw))
	}
	var tokens tokenResponse
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return "", newError("request_failed", "could not read the refresh response")
	}
	if err := storeTokens(tokens, m.now()); err != nil {
		return "", err
	}
	return tokens.accessToken(), nil
}

func firstLine(raw []byte) string {
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	text := strings.TrimSpace(string(line))
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

// UID returns the stored Qoder user id. Inference requests are signed with it
// and the signer refuses an empty one, so a credential without it authenticates
// but can never produce a valid request: that state means sign in again.
func UID() string {
	uid, err := secret.Get(keyUID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(uid)
}

// Nickname returns the stored Qoder display name, or "" when userinfo has not
// been fetched yet.
func Nickname() string {
	name, err := secret.Get(keyNickname)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// MachineID returns the persisted device identifier the grant is bound to.
func MachineID() (string, error) {
	return machineID()
}

// Authenticated reports whether a usable access token is stored.
func Authenticated() bool {
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
	return time.Now().Unix() < expires
}

// AccessToken returns the stored token, refreshing first when it has expired.
func AccessToken(ctx context.Context) (string, error) {
	stored, err := secret.Get(keyAccessToken)
	if err != nil || strings.TrimSpace(stored) == "" {
		return "", newError("not_signed_in", "not signed in to Qoder; start a login first")
	}
	if expired() {
		return defaultManager().RefreshToken(ctx)
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

var defaultOnce sync.Once
var defaultMgr *Manager

func defaultManager() *Manager {
	defaultOnce.Do(func() { defaultMgr = NewManager() })
	return defaultMgr
}

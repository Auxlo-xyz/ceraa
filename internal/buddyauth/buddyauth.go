// Package buddyauth runs Tencent CodeBuddy (domestic) and WorkBuddy (intl)
// subscription logins and holds the token set.
//
// Protocol source: the channel-pack vendor reference at
// /workspace/dsh-our-free-model/vendor/channel-pack/src/ (buddy.ts,
// buddy-oauth.ts, buddy-auth.ts, buddy-adapter.ts, product.ts), which was
// itself reversed from the CodeBuddy CN IDE genie extension (external-link-v2
// polling login) and the WorkBuddy AI 5.5.2 cli/product.json. Endpoint paths,
// query params, header names, pending codes and the token/account field names
// below are taken from that implementation, not re-derived. Places the live
// service was not exercised against are marked UNVERIFIED.
//
// The flow is polling-based, not PKCE with a loopback server: the client
// POSTs for a state, opens the server-issued login URL in a browser, then
// polls for the token and the account. No pasted code or URL is needed at any
// step, so this package has no ExtractCode-style helper: there is nothing to
// paste.
//
// STAGING, not wired: nothing in the tree constructs this package, no profile
// selects it, and no Telegram menu drives it. The seam a future merge uses is
// Manager.AccessToken (satisfies llm.CodexTokenSource) plus
// Manager.RefreshToken (satisfies llm.CodexRefresher).
//
// The credential is OAuth (Bearer access_token + refresh_token with a refresh
// endpoint), not an API key. AccessToken/RefreshToken below say so by
// existing: an API-key credential would have no refresh path.
package buddyauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
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

	"ceraa/harness/llm"
	"ceraa/internal/secret"
)

// Control-plane paths, reversed from the genie extension's product.json and
// index.js (buddy.ts AUTH_*_PATH). Both products share the paths; only the
// host, platform param and identity headers differ.
const (
	authStatePath    = "/v2/plugin/auth/state"
	authTokenPath    = "/v2/plugin/auth/token"
	loginAccountPath = "/v2/plugin/login/account"
	authRefreshPath  = "/v2/plugin/auth/token/refresh"
	// chatCompletionsPath is the inference path the adapter will use
	// (buddy-adapter.ts posts to endpoint + "/v2/chat/completions"). Exposed
	// here only so the future adapter builds the URL from the same constant;
	// this package never calls it.
	chatCompletionsPath = "/v2/chat/completions"
)

// Pending codes, reversed from the IDE catch branches (buddy.ts
// CODE_TOKEN_NOT_READY / CODE_ACCOUNT_NOT_READY). A poll that answers one of
// these means "not approved yet", not failure.
const (
	codeTokenNotReady   = 11217
	codeAccountNotReady = 12151
)

// Header names, reversed from the IDE Jd/jM/qM definitions (buddy.ts
// HTTP_HEADER_*).
const (
	headerDomain           = "X-Domain"
	headerEnterpriseID     = "X-Enterprise-Id"
	headerTenantID         = "X-Tenant-Id"
	headerNoAuth           = "X-No-Authorization"
	headerNoUserID         = "X-No-User-Id"
	headerNoEnterpriseID   = "X-No-Enterprise-Id"
	headerNoDepartmentInfo = "X-No-Department-Info"
	headerRefreshToken     = "X-Refresh-Token"
	headerRefreshSource    = "X-Auth-Refresh-Source"
	headerProductCode      = "X-Product-Code"
)

// Refresh source tag, aligned with the IDE's ide-main (buddy.ts
// AUTH_REFRESH_SOURCE).
const authRefreshSource = "ide-main"

// Deployment-type product tag sent on model/config calls in the reference
// (buddy.ts BUDDY_DEPLOYMENT_TYPE). UNVERIFIED for the auth calls in this
// package: the reference sends X-Product only on fetchModels, never on
// state/token/account/refresh, so this package does not send it either.
const deploymentType = "SaaS"

// Timing defaults, aligned with the IDE (buddy.ts LOGIN_TIMEOUT_MS,
// POLL_INTERVAL_MS, STATE_REQUEST_TIMEOUT_MS, REQUEST_TIMEOUT_MS). The state
// timeout is 10s rather than the IDE's original 5s because the reference
// measured WorkBuddy state calls at 5860-7525ms; tightening it reintroduces
// the failure the reference fixed.
const (
	defaultPollInterval   = time.Second
	defaultDeadline       = 5 * time.Minute
	stateRequestTimeout   = 10 * time.Second
	controlRequestTimeout = 60 * time.Second
	maxBody               = 1 << 20
)

// Product carries every way buddy and workbuddy differ (product.ts
// BuddyProduct). Paths, envelope shape and field names are identical; only
// these values change.
type Product struct {
	// ID is the provider route name: "buddy" or "workbuddy".
	ID string
	// Platform is the auth/state platform query param: "ide" or "workbuddy-ai".
	Platform string
	// Endpoint is the API base, e.g. https://copilot.tencent.com.
	Endpoint string
	// APIDomain is the X-Domain header value, the endpoint host without scheme.
	APIDomain string
	// ProductCode is the X-Product-Code value: "codebuddy" or "workbuddy".
	ProductCode string
	// UserAgent is the client identity the billing backend attributes usage to.
	UserAgent string
	// AppendSessionParams marks WorkBuddy, whose login URL needs version and
	// loginSessionId appended (buddy-oauth.ts decorateLoginUrl).
	AppendSessionParams bool
	// PluginVersion is appended as version= when AppendSessionParams is set.
	PluginVersion string
}

// BuddyProduct is the domestic CodeBuddy configuration (product.ts CODEBUDDY).
func BuddyProduct() Product {
	return Product{
		ID:          "buddy",
		Platform:    "ide",
		Endpoint:    "https://copilot.tencent.com",
		APIDomain:   "copilot.tencent.com",
		ProductCode: "codebuddy",
		UserAgent:   "CodeBuddyIDE/1.106.1",
	}
}

// WorkBuddyProduct is the international WorkBuddy configuration (product.ts
// WORKBUDDY, reversed from the 5.5.2 cli/product.json).
func WorkBuddyProduct() Product {
	return Product{
		ID:                  "workbuddy",
		Platform:            "workbuddy-ai",
		Endpoint:            "https://www.workbuddy.ai",
		APIDomain:           "www.workbuddy.ai",
		ProductCode:         "workbuddy",
		UserAgent:           "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2",
		AppendSessionParams: true,
		PluginVersion:       "5.5.2",
	}
}

func (p Product) stateURL() string {
	return strings.TrimSuffix(p.Endpoint, "/") + authStatePath + "?platform=" + url.QueryEscape(p.Platform)
}

func (p Product) tokenURL(state string) string {
	return strings.TrimSuffix(p.Endpoint, "/") + authTokenPath + "?state=" + url.QueryEscape(state)
}

func (p Product) accountURL(state string) string {
	return strings.TrimSuffix(p.Endpoint, "/") + loginAccountPath + "?state=" + url.QueryEscape(state)
}

func (p Product) refreshURL() string {
	return strings.TrimSuffix(p.Endpoint, "/") + authRefreshPath
}

// ChatCompletionsURL is the inference URL the future adapter will post to.
// Present so the URL is built from the product once, here, rather than being
// re-derived at the call site.
func (p Product) ChatCompletionsURL() string {
	return strings.TrimSuffix(p.Endpoint, "/") + "/v2" + chatCompletionsPath
}

// keyPrefix namespaces this product's secrets in the shared store, the way
// the other providers do, because a plain "access_token" from two vendors
// would be one variable wearing two names. Buddy and workbuddy additionally
// must not see each other: the reference keeps them in separate credential
// refs (BUDDY_ACCESS_TOKEN vs WORKBUDDY_ACCESS_TOKEN), and separate prefixes
// are this store's equivalent.
func (p Product) keyPrefix() string {
	if p.ID == "workbuddy" {
		return "workbuddy_"
	}
	return "buddy_"
}

func (p Product) key(name string) string { return p.keyPrefix() + name }

// Secret field names (suffixes). The full key is prefix + suffix, e.g.
// buddy_access_token or workbuddy_access_token.
const (
	suffixAccessToken      = "access_token"
	suffixRefreshToken     = "refresh_token"
	suffixExpiresAt        = "expires_at"
	suffixRefreshExpiresAt = "refresh_expires_at"
	suffixTokenType        = "token_type"
	suffixScope            = "scope"
	suffixDomain           = "domain"
	suffixUserID           = "user_id"
	suffixNickname         = "nickname"
	suffixEnterpriseID     = "enterprise_id"
	suffixAccountType      = "account_type"
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

const pendingCode = "authorization_pending"

// IsPending reports whether a failure just means the user has not approved yet.
func IsPending(err error) bool {
	var flowErr *Error
	if errors.As(err, &flowErr) {
		return flowErr.Code == pendingCode
	}
	return false
}

// tokenData is the parsed auth/token (or refresh) payload. Field names are
// camelCase on the wire (buddy.ts BuddyToken); expiry arrives either absolute
// (expiresAt) or relative (expiresIn seconds), and live evidence says the
// token endpoint sends only the relative form.
type tokenData struct {
	AccessToken        string
	RefreshToken       string
	ExpiresAtMs        string
	RefreshExpiresAtMs string
	TokenType          string
	Scope              string
	Domain             string
}

// accountData is the parsed login/account payload (buddy.ts BuddyAccount).
type accountData struct {
	UID          string
	Nickname     string
	EnterpriseID string
	AccountType  string
}

// State is a consistent copy of a flow, taken under the lock.
type State struct {
	FlowID     string
	AuthURL    string
	Status     string
	Message    string
	Hint       string
	LoginState string
}

// Flow is one browser login in progress.
type Flow struct {
	mu sync.Mutex

	flowID     string
	authURL    string
	status     string
	message    string
	hint       string
	loginState string

	finished bool
}

// Snapshot returns the current state. Every accessor takes the lock, so a
// caller must not hold it across one of them; the lock is not reentrant.
func (f *Flow) Snapshot() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return State{
		FlowID:     f.flowID,
		AuthURL:    f.authURL,
		Status:     f.status,
		Message:    f.message,
		Hint:       f.hint,
		LoginState: f.loginState,
	}
}

func (f *Flow) setStatus(status, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.message = status, message
}

func (f *Flow) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = true
}

// Manager runs browser logins for one product.
type Manager struct {
	mu      sync.Mutex
	product Product
	client  *http.Client

	onComplete func(State)
	// now and sleep are seams so expiry and the poll cadence can be tested
	// without waiting in real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	// The vendor polls every second for five minutes. A longer cadence would
	// make an approval look stuck; a shorter one is a retry in disguise.
	pollInterval time.Duration
	deadline     time.Duration
}

// NewManager creates a manager for domestic CodeBuddy.
func NewManager() *Manager {
	return NewManagerWithOptions(BuddyProduct(), &http.Client{Timeout: controlRequestTimeout})
}

// NewWorkBuddyManager creates a manager for international WorkBuddy.
func NewWorkBuddyManager() *Manager {
	return NewManagerWithOptions(WorkBuddyProduct(), &http.Client{Timeout: controlRequestTimeout})
}

// NewManagerWithOptions creates a manager for an explicit product against
// explicit endpoints, for tests and for a region that is not the default.
func NewManagerWithOptions(product Product, client *http.Client) *Manager {
	if client == nil {
		client = &http.Client{Timeout: controlRequestTimeout}
	}
	if product.ID == "" {
		product = BuddyProduct()
	}
	return &Manager{
		product:      product,
		client:       client,
		now:          time.Now,
		sleep:        sleepContext,
		pollInterval: defaultPollInterval,
		deadline:     defaultDeadline,
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

// Product returns the product this manager is bound to.
func (m *Manager) Product() Product { return m.product }

// Start fetches a fresh state from the server and returns the flow carrying
// the browser login URL. Polling begins on Poll, not here, so the caller
// decides when the clock starts.
func (m *Manager) Start(ctx context.Context) (*Flow, error) {
	state, authURL, err := m.fetchAuthState(ctx)
	if err != nil {
		return nil, err
	}
	if m.product.AppendSessionParams {
		authURL = decorateLoginURL(authURL, m.product)
	}
	flow := &Flow{
		flowID:     fmt.Sprintf("%s-%d", m.product.ID, m.now().UnixNano()),
		authURL:    authURL,
		status:     "pending",
		message:    "approve the login in the browser",
		hint:       authURL,
		loginState: state,
	}
	return flow, nil
}

// decorateLoginURL appends version and a fresh loginSessionId to the
// server-issued auth URL for products that require it. Only parameters are
// added: platform, state and path all come from the server and must not be
// rebuilt. The session id is a tracing value only; the reference generates a
// new UUID per login, so this does too. UNVERIFIED against a live WorkBuddy
// server: the params are taken from the reference, which notes the server was
// observed not to validate them.
func decorateLoginURL(authURL string, product Product) string {
	parsed, err := url.Parse(authURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return authURL
	}
	query := parsed.Query()
	if product.PluginVersion != "" {
		query.Set("version", product.PluginVersion)
	}
	sessionID, err := newUUID()
	if err != nil {
		return authURL
	}
	query.Set("loginSessionId", sessionID)
	parsed.RawQuery = query.Encode()
	return parsed.String()
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
// It runs the token poll then the account poll in order: the token is needed
// to authenticate the account call, and the account is needed for identity,
// because inference requests are attributed with the uid and a login without
// identity authenticates yet never produces a fully attributed session.
//
// The pending signals are backend codes (11217/12151), not HTTP statuses: a
// reader that treats any non-2xx as a failure reports "declined" on the very
// first poll. Transport errors continue the poll rather than failing it,
// matching the reference: a blip mid-login is not a refusal.
func (m *Manager) Poll(ctx context.Context, flow *Flow) error {
	flow.mu.Lock()
	state := flow.loginState
	flow.mu.Unlock()
	if strings.TrimSpace(state) == "" {
		return newError("request_failed", "the login flow carries no state; start a new one")
	}

	token, err := m.loopGetToken(ctx, state, flow)
	if err != nil {
		return err
	}
	account, err := m.loopGetAccount(ctx, state, token, flow)
	if err != nil {
		return err
	}
	if err := m.storeCredential(token, account); err != nil {
		flow.setStatus("failed", err.Error())
		return err
	}
	flow.setStatus("completed", "signed in")
	flow.finish()
	if m.onComplete != nil {
		m.onComplete(flow.Snapshot())
	}
	return nil
}

// envelope is the backend response shape: {code, message|msg, data}.
// UNVERIFIED in one respect: the adapter reads a displayMsg.{zh,en} object on
// chat errors, but the auth endpoints were only ever observed with
// code/message/data, so only those are parsed here.
type envelope struct {
	Code    float64         `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (m *Manager) doRequest(ctx context.Context, method, target string, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0, nil, newError("request_failed", "could not build the login request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, newError("request_failed", "could not read the login response")
	}
	return resp.StatusCode, raw, nil
}

func parseEnvelope(raw []byte) (envelope, bool) {
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var wide map[string]any
	if err := dec.Decode(&wide); err != nil {
		return env, false
	}
	if code, ok := wide["code"]; ok {
		switch v := code.(type) {
		case json.Number:
			f, _ := v.Float64()
			env.Code = f
		case float64:
			env.Code = v
		}
	}
	if msg, ok := wide["message"].(string); ok {
		env.Message = msg
	}
	if msg, ok := wide["msg"].(string); ok {
		env.Msg = msg
	}
	if data, ok := wide["data"]; ok && data != nil {
		encoded, err := json.Marshal(data)
		if err == nil {
			env.Data = encoded
		}
	}
	return env, true
}

func envelopeMessage(env envelope) string {
	if env.Message != "" {
		return env.Message
	}
	return env.Msg
}

// fetchAuthState POSTs /v2/plugin/auth/state?platform=... with the no-identity
// headers. The request carries no body: the reference helper sends method +
// headers only. The response must carry both state and authUrl.
func (m *Manager) fetchAuthState(ctx context.Context) (string, string, error) {
	headers := map[string]string{
		headerDomain:           m.product.APIDomain,
		headerNoAuth:           "true",
		headerNoUserID:         "true",
		headerNoEnterpriseID:   "true",
		headerNoDepartmentInfo: "true",
		"User-Agent":           m.product.UserAgent,
	}
	status, raw, err := m.doRequest(ctx, http.MethodPost, m.product.stateURL(), headers, stateRequestTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", newError("request_failed", "the login request was cancelled")
		}
		return "", "", newError("request_failed", "could not reach the login server: %v", err)
	}
	if status != http.StatusOK {
		env, _ := parseEnvelope(raw)
		return "", "", newError("request_failed", "auth/state HTTP %d: %s", status, envelopeMessage(env))
	}
	env, ok := parseEnvelope(raw)
	if !ok || len(env.Data) == 0 {
		return "", "", newError("request_failed", "the auth/state response was not readable")
	}
	var record map[string]any
	if err := json.Unmarshal(env.Data, &record); err != nil {
		return "", "", newError("request_failed", "the auth/state response was not readable")
	}
	state, _ := record["state"].(string)
	authURL, _ := record["authUrl"].(string)
	if strings.TrimSpace(state) == "" {
		return "", "", newError("request_failed", "the auth/state response carried no state")
	}
	if strings.TrimSpace(authURL) == "" {
		return "", "", newError("request_failed", "the auth/state response carried no login URL")
	}
	return strings.TrimSpace(state), strings.TrimSpace(authURL), nil
}

func (m *Manager) loopGetToken(ctx context.Context, state string, flow *Flow) (tokenData, error) {
	target := m.product.tokenURL(state)
	headers := map[string]string{
		headerNoAuth: "true",
		"User-Agent": m.product.UserAgent,
	}
	deadline := m.now().Add(m.deadline)
	for {
		if ctx.Err() != nil {
			return tokenData{}, newError("request_failed", "the login was cancelled")
		}
		if !m.now().Before(deadline) {
			flow.setStatus("expired", "the login was not approved in time")
			return tokenData{}, newError("expired", "the login was not approved in time; start a new one")
		}
		if err := m.sleep(ctx, m.pollInterval); err != nil {
			return tokenData{}, newError("request_failed", "the login was cancelled")
		}
		status, raw, err := m.doRequest(ctx, http.MethodGet, target, headers, controlRequestTimeout)
		if err != nil {
			// Transport blip: keep polling, matching the reference.
			continue
		}
		env, _ := parseEnvelope(raw)
		if status == http.StatusOK && len(env.Data) != 0 {
			token, err := parseTokenData(env.Data, m.now())
			if err != nil {
				flow.setStatus("failed", err.Error())
				return tokenData{}, err
			}
			return token, nil
		}
		if int(env.Code) == codeTokenNotReady {
			flow.setStatus("pending", "waiting for approval in the browser")
			continue
		}
		flow.setStatus("failed", fmt.Sprintf("the login request failed with status %d", status))
		return tokenData{}, newError("request_failed", "auth/token HTTP %d code=%d: %s", status, int(env.Code), envelopeMessage(env))
	}
}

func (m *Manager) loopGetAccount(ctx context.Context, state string, token tokenData, flow *Flow) (accountData, error) {
	target := m.product.accountURL(state)
	// X-Domain falls back to the product domain when the token carries none.
	// The parsed token yields "" (not missing) when the field is absent, so a
	// bare use would send an empty domain; the || fallback is load-bearing.
	domain := token.Domain
	if domain == "" {
		domain = m.product.APIDomain
	}
	headers := map[string]string{
		headerDomain:         domain,
		"Authorization":      "Bearer " + token.AccessToken,
		headerNoUserID:       "true",
		headerNoEnterpriseID: "true",
		"User-Agent":         m.product.UserAgent,
	}
	deadline := m.now().Add(m.deadline)
	for {
		if ctx.Err() != nil {
			return accountData{}, newError("request_failed", "the login was cancelled")
		}
		if !m.now().Before(deadline) {
			flow.setStatus("expired", "the account was not approved in time")
			return accountData{}, newError("expired", "the account was not approved in time; start a new one")
		}
		if err := m.sleep(ctx, m.pollInterval); err != nil {
			return accountData{}, newError("request_failed", "the login was cancelled")
		}
		status, raw, err := m.doRequest(ctx, http.MethodGet, target, headers, controlRequestTimeout)
		if err != nil {
			continue
		}
		env, _ := parseEnvelope(raw)
		if status == http.StatusOK && len(env.Data) != 0 {
			return parseAccountData(env.Data), nil
		}
		if int(env.Code) == codeAccountNotReady {
			flow.setStatus("pending", "waiting for the account approval")
			continue
		}
		flow.setStatus("failed", fmt.Sprintf("the account request failed with status %d", status))
		return accountData{}, newError("request_failed", "login/account HTTP %d code=%d: %s", status, int(env.Code), envelopeMessage(env))
	}
}

// readStringField reads a string field, accepting a JSON number in its place:
// the backend returns timestamps in either form. Control characters (CR/LF,
// which the scope field has been observed to contain) are stripped and
// whitespace collapsed, because a raw multiline value breaks the stored JSON
// on re-read.
func readStringField(record map[string]any, key string) string {
	switch v := record[key].(type) {
	case string:
		return stripControlChars(v)
	case float64:
		return stripControlChars(strconv.FormatFloat(v, 'f', -1, 64))
	case json.Number:
		return stripControlChars(v.String())
	}
	return ""
}

func stripControlChars(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	prevSpace := false
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

func readNumberField(record map[string]any, key string) (float64, bool) {
	switch v := record[key].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		return f, err == nil
	}
	return 0, false
}

// absoluteExpiryMs resolves an expiry to a millisecond timestamp string. An
// absolute value wins; otherwise a relative seconds value is added to the
// token's iat claim, falling back to now. Live evidence says the token
// endpoint sends only the relative form, so the fallback path is the common
// one, not the edge.
func absoluteExpiryMs(record map[string]any, absoluteKey, relativeKey, accessToken string, now time.Time) string {
	absolute := readStringField(record, absoluteKey)
	if absolute != "" {
		if ms, ok := parseExpiryMs(absolute); ok {
			return strconv.FormatInt(ms, 10)
		}
		return absolute
	}
	relative, ok := readNumberField(record, relativeKey)
	if !ok {
		return ""
	}
	base := now.UnixMilli()
	if iat := jwtIssuedAtMs(accessToken); iat != nil {
		base = *iat
	}
	return strconv.FormatInt(base+int64(relative*1000), 10)
}

// parseExpiryMs accepts a millisecond timestamp, a second timestamp, or an
// ISO 8601 string. Anything else is not an expiry.
func parseExpiryMs(raw string) (int64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	if allDigits(trimmed) {
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		if n > 1_000_000_000_000 {
			return n, true
		}
		return n * 1000, true
	}
	if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// parseTokenData reads the token payload. A missing or non-string
// accessToken is a malformed grant and is rejected, never treated as pending:
// the vendor's own parser enforces the same.
func parseTokenData(data json.RawMessage, now time.Time) (tokenData, error) {
	var record map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&record); err != nil {
		return tokenData{}, newError("request_failed", "the token response was not readable")
	}
	rawAccess, present := record["accessToken"]
	if !present {
		return tokenData{}, newError("request_failed", "the token response contained no token")
	}
	access, ok := rawAccess.(string)
	if !ok || strings.TrimSpace(access) == "" {
		return tokenData{}, newError("request_failed", "the token response contained no token")
	}
	access = stripControlChars(access)
	tokenType := readStringField(record, "tokenType")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return tokenData{
		AccessToken:        access,
		RefreshToken:       readStringField(record, "refreshToken"),
		ExpiresAtMs:        absoluteExpiryMs(record, "expiresAt", "expiresIn", access, now),
		RefreshExpiresAtMs: absoluteExpiryMs(record, "refreshExpiresAt", "refreshExpiresIn", access, now),
		TokenType:          tokenType,
		Scope:              readStringField(record, "scope"),
		Domain:             readStringField(record, "domain"),
	}, nil
}

// parseAccountData reads the account payload. Live evidence says nickname is
// often empty here; the real nickname lives in the access_token JWT claims
// and is backfilled at store time.
func parseAccountData(data json.RawMessage) accountData {
	var record map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&record); err != nil {
		return accountData{}
	}
	accountType := readStringField(record, "type")
	if accountType == "" {
		accountType = "personal"
	}
	return accountData{
		UID:          readStringField(record, "uid"),
		Nickname:     readStringField(record, "nickname"),
		EnterpriseID: readStringField(record, "enterpriseId"),
		AccountType:  accountType,
	}
}

// jwtPayload decodes the token's payload without verifying any signature: the
// claims are used for display and renewal scheduling only.
func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload
}

func jwtExpiresAtMs(token string) *int64 {
	payload := jwtPayload(token)
	if payload == nil {
		return nil
	}
	exp, ok := payload["exp"].(float64)
	if !ok {
		return nil
	}
	ms := int64(exp * 1000)
	return &ms
}

func jwtIssuedAtMs(token string) *int64 {
	payload := jwtPayload(token)
	if payload == nil {
		return nil
	}
	iat, ok := payload["iat"].(float64)
	if !ok {
		return nil
	}
	ms := int64(iat * 1000)
	return &ms
}

// jwtNickname reads the nickname claim, falling back through the synonyms the
// reference observed. Live evidence: login/account omits the nickname and the
// JWT is the only source.
func jwtNickname(token string) string {
	payload := jwtPayload(token)
	if payload == nil {
		return ""
	}
	for _, key := range []string{"nickname", "preferred_username", "name"} {
		if name, ok := payload[key].(string); ok && strings.TrimSpace(name) != "" {
			return stripControlChars(name)
		}
	}
	return ""
}

func jwtSubject(token string) string {
	payload := jwtPayload(token)
	if payload == nil {
		return ""
	}
	sub, _ := payload["sub"].(string)
	return strings.TrimSpace(sub)
}

// credentialExpiresAtMs resolves the stored expiry: the stored value first,
// the access_token JWT exp as fallback. An unparseable expiry is not expired:
// refusing a credential nobody can prove is dead bricks a good login.
func (m *Manager) credentialExpiresAtMs() *int64 {
	raw, err := secret.Get(m.product.key(suffixExpiresAt))
	if err == nil && strings.TrimSpace(raw) != "" {
		if ms, ok := parseExpiryMs(raw); ok {
			return &ms
		}
	}
	stored, err := secret.Get(m.product.key(suffixAccessToken))
	if err != nil {
		return nil
	}
	return jwtExpiresAtMs(strings.TrimSpace(stored))
}

// storeCredential writes the token set plus identity to the encrypted store.
// The refresh token is replaced whenever a new one arrives, because providers
// rotate on use and the new one must replace the old. Identity is sticky: a
// response that omits it must not wipe what the login established. The
// nickname falls back to the JWT claims because login/account omits it.
func (m *Manager) storeCredential(token tokenData, account accountData) error {
	if strings.TrimSpace(token.AccessToken) == "" {
		return newError("request_failed", "the token response contained no token")
	}
	p := m.product
	if err := secret.Set(p.key(suffixAccessToken), token.AccessToken); err != nil {
		return err
	}
	if token.RefreshToken != "" {
		if err := secret.Set(p.key(suffixRefreshToken), token.RefreshToken); err != nil {
			return err
		}
	}
	if token.ExpiresAtMs != "" {
		if err := secret.Set(p.key(suffixExpiresAt), token.ExpiresAtMs); err != nil {
			return err
		}
	}
	if token.RefreshExpiresAtMs != "" {
		if err := secret.Set(p.key(suffixRefreshExpiresAt), token.RefreshExpiresAtMs); err != nil {
			return err
		}
	}
	if token.TokenType != "" {
		_ = secret.Set(p.key(suffixTokenType), token.TokenType)
	}
	if token.Scope != "" {
		_ = secret.Set(p.key(suffixScope), token.Scope)
	}
	if token.Domain != "" {
		_ = secret.Set(p.key(suffixDomain), token.Domain)
	}
	if uid := account.UID; strings.TrimSpace(uid) != "" {
		_ = secret.Set(p.key(suffixUserID), strings.TrimSpace(uid))
	} else if sub := jwtSubject(token.AccessToken); sub != "" {
		_ = secret.Set(p.key(suffixUserID), sub)
	}
	nickname := strings.TrimSpace(account.Nickname)
	if nickname == "" {
		nickname = jwtNickname(token.AccessToken)
	}
	if nickname != "" {
		_ = secret.Set(p.key(suffixNickname), nickname)
	}
	if strings.TrimSpace(account.EnterpriseID) != "" {
		_ = secret.Set(p.key(suffixEnterpriseID), strings.TrimSpace(account.EnterpriseID))
	}
	if strings.TrimSpace(account.AccountType) != "" {
		_ = secret.Set(p.key(suffixAccountType), strings.TrimSpace(account.AccountType))
	}
	return nil
}

var refreshMu sync.Mutex

// RefreshToken exchanges the stored refresh token for a new set.
//
// The refresh travels in the X-Refresh-Token header alongside a Bearer
// Authorization header, with X-Auth-Refresh-Source: ide-main; it is not a
// standard form-encoded grant. UNVERIFIED against a live server: the header
// shape is taken from the reference, which is the only evidence.
//
// Serialised, because a rotated refresh token invalidates its predecessor and
// two concurrent refreshes leave one of them holding a dead token.
func (m *Manager) RefreshToken(ctx context.Context) (string, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	p := m.product
	stored, err := secret.Get(p.key(suffixRefreshToken))
	if err != nil || strings.TrimSpace(stored) == "" {
		return "", newError("no_refresh_token", "no refresh token is stored; sign in again")
	}
	access, err := secret.Get(p.key(suffixAccessToken))
	if err != nil || strings.TrimSpace(access) == "" {
		return "", newError("no_refresh_token", "no refresh token is stored; sign in again")
	}
	headers := map[string]string{
		headerDomain:        p.APIDomain,
		"User-Agent":        p.UserAgent,
		"Authorization":     "Bearer " + strings.TrimSpace(access),
		headerRefreshToken:  strings.TrimSpace(stored),
		headerRefreshSource: authRefreshSource,
	}
	if enterprise, err := secret.Get(p.key(suffixEnterpriseID)); err == nil && strings.TrimSpace(enterprise) != "" {
		headers[headerEnterpriseID] = strings.TrimSpace(enterprise)
		headers[headerTenantID] = strings.TrimSpace(enterprise)
	}

	reqCtx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.refreshURL(), nil)
	if err != nil {
		return "", newError("request_failed", "could not build the refresh request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", newError("request_failed", "could not reach the login server: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", newError("request_failed", "could not read the refresh response")
	}
	env, _ := parseEnvelope(raw)
	if resp.StatusCode != http.StatusOK {
		message := envelopeMessage(env)
		// Terminal判定: HTTP 401/403, backend code 401/403, or a message
		// naming expiry. Anything else is a retryable failure, not a dead
		// refresh token: treating every failure as terminal would log the
		// user out on a blip.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			int(env.Code) == 401 || int(env.Code) == 403 ||
			strings.Contains(message, "expired") || strings.Contains(message, "invalid") {
			if message == "" {
				message = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return "", &Error{Code: "refresh_expired", Message: message}
		}
		return "", newError("refresh_failed", "refresh rejected with status %d: %s", resp.StatusCode, firstLine(raw))
	}
	if len(env.Data) == 0 {
		return "", newError("request_failed", "the refresh response carried no data")
	}
	token, err := parseTokenData(env.Data, m.now())
	if err != nil {
		return "", err
	}
	// Rotation: a new refresh token replaces the old one. When the response
	// omits it, the stored one is kept rather than wiped. DIVERGENCE FROM
	// REFERENCE, deliberate: the reference auth service overwrites the field
	// unconditionally, which would strand the credential if the backend ever
	// answers a refresh without rotation. Keeping the old value is the safe
	// direction; a truly dead predecessor surfaces as refresh_expired next.
	if token.RefreshToken == "" {
		token.RefreshToken = strings.TrimSpace(stored)
	}
	account := accountData{}
	if err := m.storeCredential(token, account); err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

func firstLine(raw []byte) string {
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	text := strings.TrimSpace(string(line))
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

// UID returns the stored user id. Inference attribution needs it, so a
// credential without it means signing in again once the account poll has had
// its chance.
func (m *Manager) UID() string {
	uid, err := secret.Get(m.product.key(suffixUserID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(uid)
}

// Nickname returns the stored display name, or "" when the account poll and
// the JWT claims both carried none.
func (m *Manager) Nickname() string {
	name, err := secret.Get(m.product.key(suffixNickname))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// SignedIn reports whether an access token is stored. Presence is not a claim
// the server has validated it.
func (m *Manager) SignedIn() bool {
	stored, err := secret.Get(m.product.key(suffixAccessToken))
	return err == nil && strings.TrimSpace(stored) != ""
}

// Authenticated reports whether a usable access token is stored: present and
// not past its recorded expiry. An unparseable expiry is not expired.
func (m *Manager) Authenticated() bool {
	if !m.SignedIn() {
		return false
	}
	expires := m.credentialExpiresAtMs()
	if expires == nil {
		return true
	}
	return time.Now().UnixMilli() < *expires
}

// Forget removes this product's local credential. There is no provider revoke
// endpoint in the reference, so local removal is the whole of sign-out. The
// sibling product's keys are untouched.
func (m *Manager) Forget() error {
	for _, suffix := range []string{
		suffixAccessToken, suffixRefreshToken, suffixExpiresAt,
		suffixRefreshExpiresAt, suffixTokenType, suffixScope, suffixDomain,
		suffixUserID, suffixNickname, suffixEnterpriseID, suffixAccountType,
	} {
		_ = secret.Delete(m.product.key(suffix))
	}
	return nil
}

// AccessToken returns a usable access token for inference, refreshing first
// when the stored one is expired. It satisfies llm.CodexTokenSource by
// signature, which is the seam the provider wiring uses. The refresh and the
// store share the refresh lock: refresh tokens are single-use, so two
// concurrent refreshes would spend one token twice and burn the chain.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	p := m.product
	stored, err := secret.Get(p.key(suffixAccessToken))
	if err != nil || strings.TrimSpace(stored) == "" {
		return "", newError("not_signed_in", "not signed in to %s; start a login first", p.ID)
	}
	if expires := m.credentialExpiresAtMs(); expires != nil && time.Now().UnixMilli() >= *expires {
		return m.RefreshToken(ctx)
	}
	return strings.TrimSpace(stored), nil
}

var _ llm.CodexTokenSource = (*Manager)(nil)
var _ llm.CodexRefresher = (*Manager)(nil)

var (
	defaultOnce   sync.Once
	defaultMgr    *Manager
	workbuddyOnce sync.Once
	workbuddyMgr  *Manager
)

func defaultManager() *Manager {
	defaultOnce.Do(func() { defaultMgr = NewManager() })
	return defaultMgr
}

func workbuddyManager() *Manager {
	workbuddyOnce.Do(func() { workbuddyMgr = NewWorkBuddyManager() })
	return workbuddyMgr
}

// NewTokenSource returns the transport's credential source for domestic
// CodeBuddy: a manager against the default endpoints, used as
// llm.ProviderConfig.CodexTokens or equivalent. The manager reads the store
// on every call rather than caching the grant, so a rotation committed by one
// call is visible to the next.
func NewTokenSource() llm.CodexTokenSource {
	return NewManager()
}

// NewWorkBuddyTokenSource returns the transport's credential source for
// international WorkBuddy.
func NewWorkBuddyTokenSource() llm.CodexTokenSource {
	return NewWorkBuddyManager()
}

// SignedIn reports whether a domestic CodeBuddy access token is stored.
func SignedIn() bool { return defaultManager().SignedIn() }

// Authenticated reports whether a usable domestic CodeBuddy token is stored.
func Authenticated() bool { return defaultManager().Authenticated() }

// AccessToken returns the stored domestic CodeBuddy token, refreshing first
// when it has expired.
func AccessToken(ctx context.Context) (string, error) {
	return defaultManager().AccessToken(ctx)
}

// Forget removes the domestic CodeBuddy credential.
func Forget() error { return defaultManager().Forget() }

// UID returns the stored domestic CodeBuddy user id.
func UID() string { return defaultManager().UID() }

// Nickname returns the stored domestic CodeBuddy display name.
func Nickname() string { return defaultManager().Nickname() }

// WorkBuddySignedIn reports whether an international WorkBuddy token is stored.
func WorkBuddySignedIn() bool { return workbuddyManager().SignedIn() }

// WorkBuddyAuthenticated reports whether a usable WorkBuddy token is stored.
func WorkBuddyAuthenticated() bool { return workbuddyManager().Authenticated() }

// WorkBuddyForget removes the international WorkBuddy credential.
func WorkBuddyForget() error { return workbuddyManager().Forget() }

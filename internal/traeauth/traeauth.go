package traeauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ceraa/internal/secret"
)

// Trae international (trae.ai) credential management.
//
// The user pastes a PAT (personal access token from their Trae account) once.
// It is exchanged at the OAuth endpoint for a short-lived ide token, which is
// what the raw-chat transport sends as `Cloud-IDE-JWT`. The PAT is the durable
// credential; the ide token is cached with its expiry and re-exchanged when
// stale. Everything lives in the encrypted secret store, never in config.

const (
	secretPAT       = "trae_pat"
	secretToken     = "trae_access_token"
	secretExpiresAt = "trae_access_expires_at"
	exchangePath    = "/cloudide/api/v3/trae/oauth/ExchangeToken"
)

// Client identity the token endpoint accepts. Fixed client constants,
// captured from the live client.
const (
	AppID          = "7b3f9dc2-8a4e-5c6d-2f1b-9e4a3c5b7df0"
	IDEVersionCode = "20260206"
)

var (
	mu       sync.Mutex
	baseURL  = "https://api.trae.ai"
	httpDo   = http.DefaultClient.Do
	nowMilli = func() int64 { return time.Now().UnixMilli() }
)

// SetBaseURL overrides the API root, for tests and for a region that is not
// the default.
func SetBaseURL(url string) {
	mu.Lock()
	defer mu.Unlock()
	baseURL = strings.TrimRight(strings.TrimSpace(url), "/")
}

func apiRoot() string {
	mu.Lock()
	defer mu.Unlock()
	if baseURL == "" {
		return "https://api.trae.ai"
	}
	return baseURL
}

// Authenticated reports whether a PAT is stored. The ide token itself is
// resolved lazily per request, so its absence here is not a sign-out.
func Authenticated() bool {
	pat, err := secret.Get(secretPAT)
	return err == nil && strings.TrimSpace(pat) != ""
}

// SavePAT stores the pasted PAT after proving it exchanges. The exchange is
// the verification: a PAT the endpoint rejects is reported here rather than
// on the user's first real task.
func SavePAT(ctx context.Context, pat string) error {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return fmt.Errorf("empty PAT")
	}
	token, expiresAt, err := exchange(ctx, apiRoot(), pat)
	if err != nil {
		return err
	}
	if err := secret.Set(secretPAT, pat); err != nil {
		return fmt.Errorf("store PAT: %w", err)
	}
	if err := secret.Set(secretToken, token); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	if err := secret.Set(secretExpiresAt, fmt.Sprintf("%d", expiresAt)); err != nil {
		return fmt.Errorf("store token expiry: %w", err)
	}
	return nil
}

// AccessToken returns a live ide token, re-exchanging the stored PAT when the
// cached one is missing or within a minute of expiry.
func AccessToken(ctx context.Context) (string, error) {
	token, _ := secret.Get(secretToken)
	if freshEnough() && strings.TrimSpace(token) != "" {
		return token, nil
	}
	pat, err := secret.Get(secretPAT)
	if err != nil || strings.TrimSpace(pat) == "" {
		return "", fmt.Errorf("not signed in to trae; paste a PAT from the provider menu first")
	}
	token, expiresAt, err := exchange(ctx, apiRoot(), strings.TrimSpace(pat))
	if err != nil {
		return "", err
	}
	_ = secret.Set(secretToken, token)
	_ = secret.Set(secretExpiresAt, fmt.Sprintf("%d", expiresAt))
	return token, nil
}

func freshEnough() bool {
	raw, err := secret.Get(secretExpiresAt)
	if err != nil || strings.TrimSpace(raw) == "" {
		return false
	}
	var expiresAt int64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &expiresAt); err != nil {
		return false
	}
	return expiresAt-nowMilli() > 60_000
}

// exchange trades a PAT for an ide token at the OAuth endpoint. The PAT
// travels as the refresh token, which is the same shape the plugin uses.
func exchange(ctx context.Context, root, pat string) (string, int64, error) {
	body, _ := json.Marshal(map[string]any{"RefreshToken": pat})
	return exchangeWithBody(ctx, root+exchangePath, body, "")
}

// RequestHeaders returns the identity headers the raw-chat endpoint expects
// for an ide token.
func RequestHeaders(token string) map[string]string {
	return map[string]string{
		"Authorization":      "Cloud-IDE-JWT " + token,
		"X-App-Id":           AppID,
		"X-Ide-Version-Code": IDEVersionCode,
		"X-Ide-Function":     "chat",
	}
}

// One-click browser login (international).
//
// There is no PAT to copy anywhere in Trae's UI, so the menu offers this
// instead: Ceraa builds a verification URI, the user opens it, approves, and
// pastes the redirect URL back. The redirect carries a refresh token, which
// is stored as the PAT — from there the durable-credential path above takes
// over unchanged. Values captured from the live client.
const (
	loginClientID = "ono9krqynydwx5"
	loginAppVer   = "3.5.54"
	loginTimeout  = 600
	loginChannel  = "native_ide"
	loginAuthFrom = "trae"
	callbackPath  = "/trae/callback"
	callbackPort  = "18787"
	exchByRefresh = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	guidancePath  = "/cloudide/api/v3/trae/GetLoginGuidance"
)

var loginGuidanceHosts = []string{
	"https://api.trae.ai",
	"https://www.trae.ai",
	"https://api.marscode.com",
}

var loginExchangeHosts = []string{
	"https://growsg-normal.trae.ai",
	"https://grow-normal.trae.ai",
	"https://api.trae.ai",
	"https://api.marscode.com",
}

// LoginFlow is a pending browser login: the URI to open and the verifier the
// exchange needs. The flow itself holds no server side — the redirect target
// is unserved on purpose and the user pastes its URL back.
type LoginFlow struct {
	VerificationURI string
	CodeVerifier    string
	LoginHost       string
}

// StartLogin asks the login service where to authorize and builds the
// verification URI. The callback URL names a loopback listener that does not
// exist: the browser cannot reach it, but its address bar carries the refresh
// token and the user pastes that URL back here.
func StartLogin(ctx context.Context) (*LoginFlow, error) {
	traceID := traeUUID()
	loginHost, err := requestLoginGuidance(ctx, traceID)
	if err != nil {
		return nil, err
	}
	loginHost = strings.TrimSpace(loginHost)
	verifier, challenge := newPKCEPair()
	callbackURL := "http://127.0.0.1:" + callbackPort + callbackPath
	uri, err := buildVerificationURI(loginHost, traceID, callbackURL, challenge, randomHexID(), randomHexID())
	if err != nil {
		return nil, err
	}
	return &LoginFlow{VerificationURI: uri, CodeVerifier: verifier, LoginHost: loginHost}, nil
}

// SaveCallbackURL parses a pasted redirect URL, extracts the refresh token,
// exchanges it, and stores it as the PAT. A raw PAT (no URL shape) is
// exchanged on the PAT path instead, so the onboarding stage accepts both.
func SaveCallbackURL(ctx context.Context, text string) error {
	refresh := ExtractRefreshToken(text)
	if refresh == "" {
		return SavePAT(ctx, strings.TrimSpace(text))
	}
	token, expiresAt, err := exchangeRefresh(ctx, refresh)
	if err != nil {
		return err
	}
	if err := secret.Set(secretPAT, refresh); err != nil {
		return fmt.Errorf("store trae credential: %w", err)
	}
	if err := secret.Set(secretToken, token); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	if err := secret.Set(secretExpiresAt, fmt.Sprintf("%d", expiresAt)); err != nil {
		return fmt.Errorf("store token expiry: %w", err)
	}
	return nil
}

// ExtractRefreshToken pulls the refresh token out of a pasted authorization
// redirect URL. Returns empty when the text is not a callback URL.
func ExtractRefreshToken(text string) string {
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || u.Host == "" {
		return ""
	}
	q := u.Query()
	for _, key := range []string{"refreshToken", "refresh_token", "RefreshToken"} {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			return v
		}
	}
	for _, key := range []string{"userJwt", "user_jwt", "authCodeInfo", "auth_code_info"} {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			continue
		}
		decoded := raw
		if unescaped, err := url.QueryUnescape(raw); err == nil {
			decoded = unescaped
		}
		var m map[string]any
		if json.Unmarshal([]byte(decoded), &m) == nil {
			if s, ok := m["RefreshToken"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			if s, ok := m["refreshToken"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func requestLoginGuidance(ctx context.Context, traceID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"loginTraceID": traceID, "login_trace_id": traceID})
	var errs []string
	for _, host := range loginGuidanceHosts {
		req, err := http.NewRequestWithContext(ctx, "POST", host+guidancePath, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Trae/1.0.0 ceraa")
		resp, err := httpDo(req)
		if err != nil {
			errs = append(errs, host+" => "+err.Error())
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			errs = append(errs, host+" => bad json")
			continue
		}
		if loginHost := pickNestedField(v, "LoginHost", "loginHost", "LoginURL", "loginUrl"); loginHost != "" {
			return loginHost, nil
		}
		errs = append(errs, host+" => no login host")
	}
	return "", fmt.Errorf("trae login guidance: %s", strings.Join(errs, " | "))
}

func buildVerificationURI(loginHost, traceID, callbackURL, challenge, machineID, deviceID string) (string, error) {
	rest := strings.TrimSpace(loginHost)
	if !strings.HasPrefix(rest, "http://") && !strings.HasPrefix(rest, "https://") {
		rest = "https://" + rest
	}
	u, err := url.Parse(rest)
	if err != nil {
		return "", err
	}
	u.Path = "/authorization"
	q := u.Query()
	q.Set("login_version", "1")
	q.Set("auth_from", loginAuthFrom)
	q.Set("login_channel", loginChannel)
	q.Set("plugin_version", "local")
	q.Set("auth_type", "local")
	q.Set("client_id", loginClientID)
	q.Set("redirect", "0")
	q.Set("login_trace_id", traceID)
	q.Set("auth_callback_url", callbackURL)
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("x_device_brand", "PC")
	q.Set("x_device_type", "Windows")
	q.Set("x_os_version", "Windows 11 Pro")
	q.Set("x_app_version", loginAppVer)
	q.Set("x_app_type", "stable")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// exchangeRefresh trades a callback refresh token for an ide token, fanning
// out over the known API origins.
func exchangeRefresh(ctx context.Context, refresh string) (string, int64, error) {
	body, _ := json.Marshal(map[string]any{
		"ClientID":     loginClientID,
		"RefreshToken": refresh,
		"ClientSecret": "-",
		"UserID":       "",
	})
	var lastErr string
	for _, host := range loginExchangeHosts {
		token, expiresAt, err := exchangeWithBody(ctx, host+exchByRefresh, body, "")
		if err == nil {
			return token, expiresAt, nil
		}
		lastErr = err.Error()
	}
	return "", 0, fmt.Errorf("trae token exchange: %s", lastErr)
}

func exchangeWithBody(ctx context.Context, endpoint string, body []byte, cloudToken string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cloudToken != "" {
		req.Header.Set("x-cloudide-token", cloudToken)
	}
	resp, err := httpDo(req)
	if err != nil {
		return "", 0, fmt.Errorf("%s => %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("%s => HTTP %d", endpoint, resp.StatusCode)
	}
	token, expiresAt, err := parseExchangeResponse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("%s => %w", endpoint, err)
	}
	return token, expiresAt, nil
}

func parseExchangeResponse(raw []byte) (string, int64, error) {
	var v struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token         string `json:"Token"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		} `json:"Data"`
		Result struct {
			Token         string `json:"Token"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		// The refresh endpoint answers in several casings; fall back to a
		// generic dig before giving up on the shape.
		var generic map[string]any
		if jerr := json.Unmarshal(raw, &generic); jerr != nil {
			return "", 0, fmt.Errorf("bad json")
		}
		token := pickField(generic, "AccessToken", "accessToken", "access_token", "Token", "token")
		if token == "" {
			for _, key := range []string{"Result", "result", "Data", "data"} {
				if m, ok := generic[key].(map[string]any); ok {
					if token = pickField(m, "AccessToken", "accessToken", "access_token", "Token", "token"); token != "" {
						break
					}
				}
			}
		}
		if token == "" {
			return "", 0, fmt.Errorf("no token in response")
		}
		return token, nowMilli() + 30*60_000, nil
	}
	if v.Code != 0 {
		msg := strings.TrimSpace(v.Message)
		if msg == "" {
			msg = "unknown error"
		}
		return "", 0, fmt.Errorf("code %d: %s", v.Code, msg)
	}
	token := strings.TrimSpace(v.Data.Token)
	expiresAt := v.Data.TokenExpireAt
	if token == "" {
		token = strings.TrimSpace(v.Result.Token)
		expiresAt = v.Result.TokenExpireAt
	}
	if token == "" {
		return "", 0, fmt.Errorf("no token in response")
	}
	if expiresAt <= 0 {
		expiresAt = nowMilli() + 30*60_000
	}
	return token, expiresAt, nil
}

func pickField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// pickNestedField digs through the (Result|result|data|root) containers the
// login service wraps its answers in.
func pickNestedField(v map[string]any, keys ...string) string {
	if s := pickField(v, keys...); s != "" {
		return s
	}
	for _, container := range []string{"Result", "result", "Data", "data"} {
		if m, ok := v[container].(map[string]any); ok {
			if s := pickField(m, keys...); s != "" {
				return s
			}
		}
	}
	return ""
}

func newPKCEPair() (verifier, challenge string) {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func traeUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// randomHexID mints a 32-hex-char device/machine id. The authorize page keys
// device identity off these: zeros fail the token call outright, while fresh
// random ids are what every native client sends on first login.
func randomHexID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

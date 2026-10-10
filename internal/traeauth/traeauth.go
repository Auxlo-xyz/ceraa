package traeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// exchange trades a PAT for an ide token at the OAuth endpoint. The response
// carries the token at Data.Token with its expiry at Data.TokenExpireAt.
func exchange(ctx context.Context, root, pat string) (string, int64, error) {
	body, _ := json.Marshal(map[string]any{"RefreshToken": pat})
	req, err := http.NewRequestWithContext(ctx, "POST", root+exchangePath, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cloudide-Token", "")
	resp, err := httpDo(req)
	if err != nil {
		return "", 0, fmt.Errorf("trae token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("trae token exchange: HTTP %d", resp.StatusCode)
	}
	var v struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token         string `json:"Token"`
			TokenExpireAt int64  `json:"TokenExpireAt"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", 0, fmt.Errorf("trae token exchange: bad json")
	}
	if v.Code != 0 {
		msg := strings.TrimSpace(v.Message)
		if msg == "" {
			msg = "unknown error"
		}
		return "", 0, fmt.Errorf("trae token exchange: code %d: %s", v.Code, msg)
	}
	if strings.TrimSpace(v.Data.Token) == "" {
		return "", 0, fmt.Errorf("trae token exchange: no token in response")
	}
	expiresAt := v.Data.TokenExpireAt
	if expiresAt <= 0 {
		expiresAt = nowMilli() + 30*60_000
	}
	return v.Data.Token, expiresAt, nil
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

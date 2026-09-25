package hermesauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sooraa/harness/flock"
)

const (
	defaultNousPortalURL = "https://portal.nousresearch.com"
	defaultNousClientID  = "hermes-cli"
	codexTokenURL        = "https://auth.openai.com/oauth/token"
	codexClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	xaiDiscoveryURL      = "https://auth.x.ai/.well-known/openid-configuration"
	xaiClientID          = "b1a00492-073a-47ea-816f-4c329264a828"
	qwenTokenURL         = "https://chat.qwen.ai/api/v1/oauth2/token"
	qwenClientID         = "f0304373b74a44d2b584a3fb70ca9e56"
	minimaxClientID      = "78257093-7e40-4613-99e0-527b14b39113"
)

func (s *Store) Refresh(ctx context.Context, providerID string) error {
	providerID = normalizeProvider(providerID)
	if providerID == "" {
		return ErrNoCredential
	}
	path := s.AuthPath()
	if path == "" {
		return errors.New("hermes home is not configured")
	}
	lock, err := flock.Acquire(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock hermes auth store: %w", err)
	}
	defer lock.Release()
	snapshot, err := s.Load()
	if err != nil {
		return err
	}
	credential, err := resolveFromSnapshot(snapshot, providerID)
	if err != nil {
		return err
	}
	refreshed, err := s.refreshCredential(ctx, credential)
	if err != nil {
		return err
	}
	return s.persistRefresh(credential, refreshed)
}

func (s *Store) refreshCredential(ctx context.Context, credential Credential) (Credential, error) {
	if credential.RefreshToken == "" {
		return Credential{}, fmt.Errorf("hermes provider %q has no refresh token", credential.Provider)
	}
	endpoint := credential.TokenEndpoint
	clientID := credential.ClientID
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}}
	headers := map[string]string{"Accept": "application/json"}
	switch credential.Provider {
	case "nous":
		endpoint = firstNonEmpty(credential.PortalBaseURL, defaultNousPortalURL) + "/api/oauth/token"
		clientID = firstNonEmpty(clientID, defaultNousClientID)
		form.Set("client_id", clientID)
		headers["x-nous-refresh-token"] = credential.RefreshToken
	case "openai-codex":
		endpoint = firstNonEmpty(endpoint, codexTokenURL)
		clientID = firstNonEmpty(clientID, codexClientID)
		form.Set("client_id", clientID)
	case "xai-oauth":
		if endpoint == "" {
			discovered, discoverErr := s.discoverXAITokenEndpoint(ctx)
			if discoverErr != nil {
				return Credential{}, discoverErr
			}
			endpoint = discovered
		}
		clientID = firstNonEmpty(clientID, xaiClientID)
		form.Set("client_id", clientID)
	case "qwen-oauth":
		endpoint = firstNonEmpty(endpoint, qwenTokenURL)
		clientID = firstNonEmpty(clientID, qwenClientID)
		form.Set("client_id", clientID)
	case "minimax-oauth":
		endpoint = firstNonEmpty(endpoint, firstNonEmpty(credential.PortalBaseURL, "https://api.minimax.io")+"/oauth/token")
		clientID = firstNonEmpty(clientID, minimaxClientID)
		form.Set("client_id", clientID)
	default:
		return Credential{}, fmt.Errorf("direct refresh is not implemented for hermes provider %q", credential.Provider)
	}
	payload, err := s.postToken(ctx, endpoint, form, headers)
	if err != nil {
		return Credential{}, err
	}
	accessToken, _ := payload["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return Credential{}, fmt.Errorf("hermes provider %q refresh response has no access_token", credential.Provider)
	}
	refreshToken, _ := payload["refresh_token"].(string)
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		refreshToken = credential.RefreshToken
	}
	expiresAt := expiryFromPayload(payload)
	if expiresAt.IsZero() {
		expiresAt = tokenExpiry(accessToken)
	}
	updated := credential
	updated.AccessToken = accessToken
	updated.RefreshToken = refreshToken
	updated.ExpiresAt = expiresAt
	if value, ok := payload["token_type"].(string); ok && strings.TrimSpace(value) != "" {
		updated.TokenType = strings.TrimSpace(value)
	}
	if credential.Provider == "nous" {
		if value, ok := payload["inference_base_url"].(string); ok && strings.TrimSpace(value) != "" {
			updated.BaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
		}
	}
	return updated, nil
}

func (s *Store) postToken(ctx context.Context, endpoint string, form url.Values, headers map[string]string) (map[string]any, error) {
	if err := validateTokenEndpoint(endpoint); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("token refresh request: %w", err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read token refresh response: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("token refresh returned HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode token refresh response: %w", err)
	}
	return payload, nil
}

func (s *Store) discoverXAITokenEndpoint(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiDiscoveryURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("xAI discovery: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("xAI discovery returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode xAI discovery: %w", err)
	}
	if err := validateTokenEndpoint(payload.TokenEndpoint); err != nil {
		return "", err
	}
	return payload.TokenEndpoint, nil
}

func validateTokenEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("refusing non-HTTPS token endpoint")
	}
	return nil
}

func expiryFromPayload(payload map[string]any) time.Time {
	value, ok := payload["expires_in"]
	if !ok {
		return time.Time{}
	}
	var seconds float64
	switch typed := value.(type) {
	case float64:
		seconds = typed
	case string:
		if _, err := fmt.Sscan(typed, &seconds); err != nil {
			return time.Time{}
		}
	default:
		return time.Time{}
	}
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(seconds * float64(time.Second)))
}

func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(claims.Exp), 0)
}

func expiryOr(explicit time.Time, token string) time.Time {
	if !explicit.IsZero() {
		return explicit
	}
	return tokenExpiry(token)
}

func (s *Store) persistRefresh(before, after Credential) error {
	path := s.AuthPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read hermes auth store for refresh persistence: %w", err)
	}
	var store map[string]any
	if err := json.Unmarshal(data, &store); err != nil {
		return fmt.Errorf("decode hermes auth store for refresh persistence: %w", err)
	}
	if providers, ok := store["providers"].(map[string]any); ok {
		if state, ok := providers[before.Provider].(map[string]any); ok {
			updateProviderState(state, before, after)
		}
	}
	if pool, ok := store["credential_pool"].(map[string]any); ok {
		if entries, ok := pool[before.Provider].([]any); ok {
			for _, raw := range entries {
				entry, ok := raw.(map[string]any)
				if !ok || !sameEntry(entry, before) {
					continue
				}
				entry["access_token"] = after.AccessToken
				entry["refresh_token"] = after.RefreshToken
				entry["expires_at"] = formatTime(after.ExpiresAt)
				entry["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
				break
			}
		}
	}
	encoded, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("encode refreshed hermes auth store: %w", err)
	}
	return atomicWriteAuth(path, encoded)
}

func updateProviderState(state map[string]any, before, after Credential) {
	if tokens, ok := state["tokens"].(map[string]any); ok {
		tokens["access_token"] = after.AccessToken
		tokens["refresh_token"] = after.RefreshToken
		if !after.ExpiresAt.IsZero() {
			tokens["expires_at"] = formatTime(after.ExpiresAt)
		}
	} else {
		state["access_token"] = after.AccessToken
		state["refresh_token"] = after.RefreshToken
		if !after.ExpiresAt.IsZero() {
			state["expires_at"] = formatTime(after.ExpiresAt)
		}
	}
	if before.Provider == "nous" {
		state["agent_key"] = after.AccessToken
		state["agent_key_expires_at"] = formatTime(after.ExpiresAt)
	}
	state["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
}

func sameEntry(entry map[string]any, credential Credential) bool {
	if credential.ID != "" {
		if value, _ := entry["id"].(string); value == credential.ID {
			return true
		}
	}
	if credential.Source == "" {
		return false
	}
	value, _ := entry["source"].(string)
	return value == credential.Source
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func atomicWriteAuth(path string, data []byte) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".auth.json-*")
	if err != nil {
		return fmt.Errorf("create hermes auth temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace hermes auth store: %w", err)
	}
	return nil
}

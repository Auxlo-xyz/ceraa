package hermesauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrNoCredential = errors.New("hermes credential not found")

type Credential struct {
	Provider      string
	ID            string
	AccessToken   string
	RefreshToken  string
	BaseURL       string
	TokenType     string
	Transport     string
	PortalBaseURL string
	ClientID      string
	TokenEndpoint string
	ExpiresAt     time.Time
	Source        string
}

type Store struct {
	Home         string
	PortalURL    string
	RefreshSkew  time.Duration
	RefreshLimit time.Duration
	HTTPClient   *http.Client
}

type Snapshot struct {
	ActiveProvider string
	Providers      map[string]ProviderState
	Pool           map[string][]PoolEntry
	Native         map[string]ProviderState
}

type ProviderState struct {
	AccessToken       string
	RefreshToken      string
	AgentKey          string
	AgentKeyExpiresAt time.Time
	BaseURL           string
	InferenceBaseURL  string
	PortalBaseURL     string
	ClientID          string
	TokenEndpoint     string
	TokenType         string
	ExpiresAt         time.Time
	Tokens            *TokenPair
}

type TokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type PoolEntry struct {
	ID               string
	Source           string
	AuthType         string
	AccessToken      string
	RefreshToken     string
	AgentKey         string
	BaseURL          string
	InferenceBaseURL string
	TokenType        string
	ExpiresAt        time.Time
	Status           string
}

type rawStore struct {
	ActiveProvider string                     `json:"active_provider"`
	Providers      map[string]json.RawMessage `json:"providers"`
	CredentialPool map[string][]rawEntry      `json:"credential_pool"`
}

type rawEntry struct {
	ID               string `json:"id"`
	Source           string `json:"source"`
	AuthType         string `json:"auth_type"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AgentKey         string `json:"agent_key"`
	BaseURL          string `json:"base_url"`
	InferenceBaseURL string `json:"inference_base_url"`
	TokenType        string `json:"token_type"`
	ExpiresAt        string `json:"expires_at"`
	ExpiresAtMS      int64  `json:"expires_at_ms"`
	Status           string `json:"last_status"`
}

type nativeRecord struct {
	Provider          string    `json:"provider"`
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	AgentKey          string    `json:"agent_key"`
	AgentKeyExpiresAt time.Time `json:"agent_key_expires_at"`
	BaseURL           string    `json:"base_url"`
	PortalBaseURL     string    `json:"portal_base_url"`
	ClientID          string    `json:"client_id"`
	TokenEndpoint     string    `json:"token_endpoint"`
	TokenType         string    `json:"token_type"`
	ExpiresAt         time.Time `json:"expires_at"`
	Scope             string    `json:"scope"`
}

type rawState struct {
	AccessToken       string     `json:"access_token"`
	RefreshToken      string     `json:"refresh_token"`
	AgentKey          string     `json:"agent_key"`
	AgentKeyExpiresAt string     `json:"agent_key_expires_at"`
	BaseURL           string     `json:"base_url"`
	InferenceBaseURL  string     `json:"inference_base_url"`
	PortalBaseURL     string     `json:"portal_base_url"`
	ClientID          string     `json:"client_id"`
	TokenEndpoint     string     `json:"token_endpoint"`
	TokenType         string     `json:"token_type"`
	ExpiresAt         string     `json:"expires_at"`
	ExpiresAtMS       int64      `json:"expires_at_ms"`
	Tokens            *TokenPair `json:"tokens"`
}

func NewStore() *Store {
	home := strings.TrimSpace(os.Getenv("HERMES_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err == nil {
			home = filepath.Join(userHome, ".hermes")
		}
	}
	return &Store{Home: home, RefreshSkew: 2 * time.Minute, RefreshLimit: 90 * time.Second, HTTPClient: &http.Client{Timeout: 20 * time.Second}}
}

func (s *Store) AuthPath() string {
	if s.Home == "" {
		return ""
	}
	return filepath.Join(s.Home, "auth.json")
}

func (s *Store) Load() (Snapshot, error) {
	snapshot := Snapshot{Providers: map[string]ProviderState{}, Pool: map[string][]PoolEntry{}, Native: s.loadNative()}
	path := s.AuthPath()
	if path == "" {
		if len(snapshot.Native) == 0 {
			return snapshot, ErrNoCredential
		}
		return snapshot, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if len(snapshot.Native) == 0 {
			return snapshot, ErrNoCredential
		}
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("read hermes auth store: %w", err)
	}
	if len(data) > 8<<20 {
		return snapshot, errors.New("hermes auth store is too large")
	}
	var raw rawStore
	if err := json.Unmarshal(data, &raw); err != nil {
		return snapshot, fmt.Errorf("decode hermes auth store: %w", err)
	}
	snapshot.ActiveProvider = strings.TrimSpace(raw.ActiveProvider)
	for provider, encoded := range raw.Providers {
		var state rawState
		if json.Unmarshal(encoded, &state) != nil {
			continue
		}
		providerState := ProviderState{
			AccessToken:       strings.TrimSpace(state.AccessToken),
			RefreshToken:      strings.TrimSpace(state.RefreshToken),
			AgentKey:          strings.TrimSpace(state.AgentKey),
			AgentKeyExpiresAt: parseTime(state.AgentKeyExpiresAt, 0),
			BaseURL:           strings.TrimSpace(state.BaseURL),
			InferenceBaseURL:  strings.TrimSpace(state.InferenceBaseURL),
			PortalBaseURL:     strings.TrimSpace(state.PortalBaseURL),
			ClientID:          strings.TrimSpace(state.ClientID),
			TokenEndpoint:     strings.TrimSpace(state.TokenEndpoint),
			TokenType:         strings.TrimSpace(state.TokenType),
			ExpiresAt:         parseTime(state.ExpiresAt, state.ExpiresAtMS),
		}
		if state.Tokens != nil {
			providerState.Tokens = &TokenPair{
				AccessToken:  strings.TrimSpace(state.Tokens.AccessToken),
				RefreshToken: strings.TrimSpace(state.Tokens.RefreshToken),
				ExpiresAt:    state.Tokens.ExpiresAt,
			}
		}
		snapshot.Providers[strings.ToLower(provider)] = providerState
	}
	for provider, entries := range raw.CredentialPool {
		converted := make([]PoolEntry, 0, len(entries))
		for _, entry := range entries {
			converted = append(converted, PoolEntry{
				ID:               strings.TrimSpace(entry.ID),
				Source:           strings.TrimSpace(entry.Source),
				AuthType:         strings.TrimSpace(entry.AuthType),
				AccessToken:      strings.TrimSpace(entry.AccessToken),
				RefreshToken:     strings.TrimSpace(entry.RefreshToken),
				AgentKey:         strings.TrimSpace(entry.AgentKey),
				BaseURL:          strings.TrimSpace(entry.BaseURL),
				InferenceBaseURL: strings.TrimSpace(entry.InferenceBaseURL),
				TokenType:        strings.TrimSpace(entry.TokenType),
				ExpiresAt:        parseTime(entry.ExpiresAt, entry.ExpiresAtMS),
				Status:           strings.ToLower(strings.TrimSpace(entry.Status)),
			})
		}
		snapshot.Pool[strings.ToLower(provider)] = converted
	}
	return snapshot, nil
}

func (s *Store) Resolve(ctx context.Context, providerID string) (Credential, error) {
	providerID = normalizeProvider(providerID)
	snapshot, err := s.Load()
	if err != nil {
		return Credential{}, err
	}
	credential, err := resolveFromSnapshot(snapshot, providerID)
	if err != nil {
		return Credential{}, err
	}
	if !needsRefresh(credential, s.RefreshSkew) {
		return credential, nil
	}
	if err := s.Refresh(ctx, credential.Provider); err != nil {
		return Credential{}, fmt.Errorf("refresh hermes credential for %s: %w", credential.Provider, err)
	}
	refreshed, err := s.Load()
	if err != nil {
		return Credential{}, err
	}
	return resolveFromSnapshot(refreshed, providerID)
}

func resolveFromSnapshot(snapshot Snapshot, providerID string) (Credential, error) {
	if providerID == "" {
		providerID = normalizeProvider(snapshot.ActiveProvider)
	}
	if providerID == "" {
		return Credential{}, ErrNoCredential
	}
	if state, ok := snapshot.Native[providerID]; ok {
		token := state.AccessToken
		expiresAt := state.ExpiresAt
		if providerID == "nous" && state.AgentKey != "" {
			token = state.AgentKey
			if !state.AgentKeyExpiresAt.IsZero() {
				expiresAt = state.AgentKeyExpiresAt
			}
		}
		if token != "" {
			return Credential{Provider: providerID, ID: "native", AccessToken: token, RefreshToken: state.RefreshToken, BaseURL: firstNonEmpty(state.InferenceBaseURL, state.BaseURL, defaultBaseURL(providerID)), TokenType: firstNonEmpty(state.TokenType, "Bearer"), Transport: transportForProvider(providerID), PortalBaseURL: firstNonEmpty(state.PortalBaseURL, defaultNousPortalURL), ClientID: state.ClientID, TokenEndpoint: state.TokenEndpoint, ExpiresAt: expiryOr(expiresAt, token), Source: "ceraa-native"}, nil
		}
	}
	for _, entry := range snapshot.Pool[providerID] {
		if entry.Status == "dead" || entry.Status == "exhausted" {
			continue
		}
		token := entry.AccessToken
		if providerID == "nous" && entry.AgentKey != "" {
			token = entry.AgentKey
		}
		if token == "" {
			continue
		}
		return Credential{Provider: providerID, ID: entry.ID, AccessToken: token, RefreshToken: entry.RefreshToken, BaseURL: firstNonEmpty(entry.InferenceBaseURL, entry.BaseURL, defaultBaseURL(providerID)), TokenType: firstNonEmpty(entry.TokenType, "Bearer"), Transport: transportForProvider(providerID), ExpiresAt: expiryOr(entry.ExpiresAt, token), Source: firstNonEmpty(entry.Source, entry.ID)}, nil
	}
	state, ok := snapshot.Providers[providerID]
	if !ok {
		return Credential{}, ErrNoCredential
	}
	token := state.AccessToken
	refreshToken := state.RefreshToken
	expiresAt := state.ExpiresAt
	if state.Tokens != nil {
		if state.Tokens.AccessToken != "" {
			token = state.Tokens.AccessToken
		}
		if state.Tokens.RefreshToken != "" {
			refreshToken = state.Tokens.RefreshToken
		}
		if !state.Tokens.ExpiresAt.IsZero() {
			expiresAt = state.Tokens.ExpiresAt
		}
	}
	if providerID == "nous" && state.AgentKey != "" {
		token = state.AgentKey
		if !state.AgentKeyExpiresAt.IsZero() {
			expiresAt = state.AgentKeyExpiresAt
		}
	}
	if token == "" {
		return Credential{}, ErrNoCredential
	}
	portalBaseURL := state.PortalBaseURL
	clientID := state.ClientID
	if providerID == "nous" {
		portalBaseURL = firstNonEmpty(portalBaseURL, defaultNousPortalURL)
		clientID = firstNonEmpty(clientID, defaultNousClientID)
	}
	return Credential{Provider: providerID, AccessToken: token, RefreshToken: refreshToken, BaseURL: firstNonEmpty(state.InferenceBaseURL, state.BaseURL, defaultBaseURL(providerID)), TokenType: firstNonEmpty(state.TokenType, "Bearer"), Transport: transportForProvider(providerID), PortalBaseURL: portalBaseURL, ClientID: clientID, TokenEndpoint: state.TokenEndpoint, ExpiresAt: expiryOr(expiresAt, token), Source: "auth.json"}, nil
}

func needsRefresh(credential Credential, skew time.Duration) bool {
	if credential.ExpiresAt.IsZero() || skew <= 0 {
		return false
	}
	return !credential.ExpiresAt.After(time.Now().Add(skew))
}

func parseTime(value string, milliseconds int64) time.Time {
	if milliseconds > 0 {
		return time.UnixMilli(milliseconds)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func normalizeProvider(providerID string) string {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	switch providerID {
	case "codex", "openai_codex":
		return "openai-codex"
	case "xai", "grok":
		return "xai-oauth"
	case "qwen":
		return "qwen-oauth"
	case "minimax":
		return "minimax-oauth"
	case "nous-portal":
		return "nous"
	}
	return providerID
}

func transportForProvider(providerID string) string {
	switch providerID {
	case "openai-codex":
		return "codex_responses"
	case "minimax-oauth":
		return "anthropic_messages"
	}
	return "chat_completions"
}

func defaultBaseURL(providerID string) string {
	switch providerID {
	case "openai-codex":
		return "https://chatgpt.com/backend-api/codex"
	case "xai-oauth":
		return "https://api.x.ai/v1"
	case "qwen-oauth":
		return "https://portal.qwen.ai/v1"
	case "nous":
		return "https://inference-api.nousresearch.com/v1"
	case "minimax-oauth":
		return "https://api.minimax.io/anthropic"
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimRight(strings.TrimSpace(value), "/")
		}
	}
	return ""
}

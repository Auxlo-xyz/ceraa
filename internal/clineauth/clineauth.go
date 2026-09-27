// Package clineauth holds the Cline Account device login and its stored credential.
//
// A Cline Account is not a provider. It is a hosted gateway: api.cline.bot serves
// chat completions for a set of models that Cline pays for, including a free tier,
// metered against the account's plan rather than an API key. Signing in yields a
// token for that gateway.
//
// The identity flow is WorkOS AuthKit, Cline's identity provider, and it is a
// plain RFC 8628 device authorization grant: no PKCE, no browser tab, no cookie
// harvesting. That is what makes this wireable, and it is why this package needs
// no dialect and no new transport. The free models are served by an
// OpenAI-compatible /chat/completions, which Ceraa already speaks.
//
// The endpoints and client ids were read from the Cline SDK and then confirmed
// against the live service: a device code request returned 200 with a user code
// and a verification URI, and the recommended-models catalog is readable with no
// credential at all. Nothing past the device code has been exercised against a
// live account.
package clineauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ceraa/internal/oauthdevice"
	"ceraa/internal/secret"
)

// ProviderID is the id this package registers under.
const ProviderID = "cline"

// Endpoints, from the Cline SDK's auth module and confirmed live.
const (
	// WorkOSAPIBase is Cline's identity provider, not Cline itself.
	WorkOSAPIBase = "https://api.workos.com"
	// GatewayBase serves chat completions for an authenticated account.
	GatewayBase = "https://api.cline.bot/api/v1"
	// CatalogURL lists the models an account may use. It needs no credential,
	// which is how the free tier is discovered.
	CatalogURL = "https://api.cline.bot/api/v1/ai/cline/recommended-models"
)

// WorkOSClientID is Cline's public OAuth client.
//
// Cline publishes one per environment in its SDK rather than keeping it private,
// which is the same posture xAI and MiniMax take: a shared client meant to be used
// by third-party clients. It is a client id and not a secret.
const WorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

// ErrNoCredential is returned when nothing usable is stored.
var ErrNoCredential = errors.New("cline credential not found")

// secretName namespaces the credential in the encrypted store.
const secretName = "ceraa_cline_account"

// Credential is a resolved Cline Account credential.
type Credential struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    time.Time
	Email        string
}

type record struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Email        string `json:"email"`
	ExpiresAt    string `json:"expires_at"`
}

// Store reads and writes the stored credential.
type Store struct {
	HTTPClient  *http.Client
	RefreshSkew time.Duration
	// RefreshEndpoint overrides where a refresh is sent. Empty means Cline's own
	// API. A test needs this because that host is absolute.
	RefreshEndpoint string
}

// NewStore returns a Store with production defaults.
func NewStore() *Store {
	return &Store{HTTPClient: &http.Client{Timeout: 20 * time.Second}, RefreshSkew: 2 * time.Minute}
}

// SignedIn reports whether a usable credential is stored.
//
// A record that exists but carries no access token does not count, or the menu
// offers a switch that then fails.
func SignedIn() bool {
	_, err := Load()
	return err == nil
}

// Load returns the stored credential, or ErrNoCredential.
func Load() (Credential, error) {
	value, err := secret.Get(secretName)
	if err != nil || strings.TrimSpace(value) == "" {
		return Credential{}, ErrNoCredential
	}
	var stored record
	if json.Unmarshal([]byte(value), &stored) != nil {
		return Credential{}, ErrNoCredential
	}
	credential := Credential{
		AccessToken:  strings.TrimSpace(stored.AccessToken),
		RefreshToken: strings.TrimSpace(stored.RefreshToken),
		TokenType:    strings.TrimSpace(stored.TokenType),
		Email:        strings.TrimSpace(stored.Email),
	}
	if credential.AccessToken == "" {
		return Credential{}, ErrNoCredential
	}
	if expires, err := time.Parse(time.RFC3339, strings.TrimSpace(stored.ExpiresAt)); err == nil {
		credential.ExpiresAt = expires
	}
	return credential, nil
}

// Save writes a credential to the encrypted store.
func Save(credential Credential) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("refusing to store a cline credential with no access token")
	}
	encoded, err := json.Marshal(record{
		AccessToken:  credential.AccessToken,
		RefreshToken: credential.RefreshToken,
		TokenType:    firstNonEmpty(credential.TokenType, "Bearer"),
		Email:        credential.Email,
		ExpiresAt:    credential.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return secret.Set(secretName, string(encoded))
}

// DeviceEndpoints returns the RFC 8628 endpoints for Cline's identity provider.
//
// Both live on WorkOS rather than on Cline, so the gateway host is never sent a
// device code.
func DeviceEndpoints() oauthdevice.Endpoints {
	return oauthdevice.Endpoints{
		DeviceCode: WorkOSAPIBase + "/user_management/authorize/device",
		Token:      WorkOSAPIBase + "/user_management/authenticate",
	}
}

// ManagerConfig is the device-login configuration.
//
// The dialect is left standard: the live service answered a form-encoded request
// with 200 and a user code, so there is nothing to diverge on. MiniMax needed
// seven deviations and this needs none.
func ManagerConfig(client *http.Client) oauthdevice.Config {
	return oauthdevice.Config{
		ProviderID: ProviderID,
		ClientID:   WorkOSClientID,
		Endpoints:  DeviceEndpoints(),
		HTTPClient: client,
	}
}

// Login runs the device login and persists the token.
type Login struct {
	manager *oauthdevice.Manager
}

// NewLogin returns a Login that saves into the encrypted store.
func NewLogin(client *http.Client) *Login { return NewLoginWithEndpoints(client, DeviceEndpoints()) }

// NewLoginWithEndpoints builds a Login pointed at explicit endpoints.
//
// Exists because the endpoints are absolute constants, so a test server cannot
// intercept a login by passing an HTTP client alone: the request would go to the
// real WorkOS with a client that only trusts the stub's certificate.
func NewLoginWithEndpoints(client *http.Client, endpoints oauthdevice.Endpoints) *Login {
	config := ManagerConfig(client)
	config.Endpoints = endpoints
	manager := oauthdevice.NewManager(config)
	login := &Login{manager: manager}
	manager.SetOnToken(func(flowID string, token oauthdevice.Token) error {
		return Save(Credential{
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			ExpiresAt:    token.ExpiresAt,
		})
	})
	return login
}

// Manager exposes the underlying device-login manager.
func (l *Login) Manager() *oauthdevice.Manager { return l.manager }

// Start begins the device login.
func (l *Login) Start(ctx context.Context) (*oauthdevice.Flow, error) { return l.manager.Start(ctx) }

// defaultRefreshEndpoint is where a refresh goes when no override is set.
//
// A function rather than a constant so a test can pin it. A test that overrides
// the endpoint proves nothing about the default, which is what production uses,
// and a refresh sent to the identity provider instead would be a credential on
// the wrong host.
func defaultRefreshEndpoint() string { return GatewayBase + "/auth/refresh" }

// ChatCompletionsURL is the gateway's chat endpoint.
//
// Standard OpenAI-compatible path, so the base is stored with the profile and the
// existing chat transport appends nothing.
func ChatCompletionsURL() string { return GatewayBase + "/chat/completions" }

// Tier is one group of models the gateway serves.
type Tier string

const (
	// TierFree needs only a signed-in account.
	TierFree Tier = "free"
	// TierRecommended is the highlighted set, which may need a paid plan.
	TierRecommended Tier = "recommended"
	// TierClinePass and TierClineCloud need the matching Cline plan.
	TierClinePass  Tier = "clinePass"
	TierClineCloud Tier = "clineCloud"
)

// Model is one gateway model.
type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Catalog is the gateway's model list.
type Catalog struct {
	Recommended []Model
	Free        []Model
	ClinePass   []Model
	ClineCloud  []Model
}

// FetchCatalog reads the gateway's model list.
//
// Takes no credential, which is how the free tier is discoverable before signing
// in. A gateway that required auth here would make it impossible to show a user
// what signing in would get them.
func FetchCatalog(ctx context.Context, client *http.Client) (Catalog, error) {
	return fetchCatalogFrom(ctx, client, CatalogURL)
}

// fetchCatalogFrom is FetchCatalog against an explicit URL, so a test can point
// it at a stub rather than at the live gateway.
func fetchCatalogFrom(ctx context.Context, client *http.Client, catalogURL string) (Catalog, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return Catalog{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return Catalog{}, fmt.Errorf("fetch cline catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Catalog{}, fmt.Errorf("fetch cline catalog: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Recommended []Model `json:"recommended"`
		Free        []Model `json:"free"`
		ClinePass   []Model `json:"clinePass"`
		ClineCloud  []Model `json:"clineCloud"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Catalog{}, fmt.Errorf("fetch cline catalog: %w", err)
	}
	return Catalog{
		Recommended: payload.Recommended,
		Free:        payload.Free,
		ClinePass:   payload.ClinePass,
		ClineCloud:  payload.ClineCloud,
	}, nil
}

// Resolve returns a usable credential, refreshing it when close to expiry.
func Resolve(ctx context.Context, store *Store) (Credential, error) {
	if store == nil {
		store = NewStore()
	}
	credential, err := Load()
	if err != nil {
		return Credential{}, err
	}
	skew := store.RefreshSkew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	if credential.ExpiresAt.IsZero() || credential.ExpiresAt.After(time.Now().Add(skew)) {
		return credential, nil
	}
	if credential.RefreshToken == "" {
		return Credential{}, errors.New("cline credential has expired and no refresh token is stored; sign in again")
	}
	refreshed, err := refresh(ctx, credential, store)
	if err != nil {
		return Credential{}, err
	}
	if err := Save(refreshed); err != nil {
		return Credential{}, err
	}
	return refreshed, nil
}

func refresh(ctx context.Context, credential Credential, store *Store) (Credential, error) {
	// The refresh lives on Cline's own API rather than on the identity provider,
	// so it cannot go through the device manager, whose token endpoint is WorkOS.
	endpoint := defaultRefreshEndpoint()
	var client *http.Client
	if store != nil {
		if store.RefreshEndpoint != "" {
			endpoint = store.RefreshEndpoint
		}
		client = store.HTTPClient
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(""))
	if err != nil {
		return Credential{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+credential.RefreshToken)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return Credential{}, fmt.Errorf("refresh cline credential: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Credential{}, fmt.Errorf("refresh cline credential: HTTP %d", response.StatusCode)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	if json.NewDecoder(response.Body).Decode(&payload) != nil {
		return Credential{}, errors.New("refresh cline credential: unreadable response")
	}
	refreshed := Credential{
		// A refresh often omits the refresh token, meaning keep the one held.
		RefreshToken: firstNonEmpty(payload.RefreshToken, credential.RefreshToken),
		TokenType:    firstNonEmpty(payload.TokenType, credential.TokenType),
		Email:        credential.Email,
		AccessToken:  strings.TrimSpace(payload.AccessToken),
	}
	if refreshed.AccessToken == "" {
		return Credential{}, errors.New("cline refresh returned no access token")
	}
	// A refreshed access token is assumed short-lived rather than read from a
	// field the gateway may not send, so it is refreshed on the skew instead of
	// being treated as never expiring.
	refreshed.ExpiresAt = time.Now().Add(30 * time.Minute)
	return refreshed, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// TokenSource adapts the stored credential to the transport's credential
// interface, so the llm package does not know how Cline stores anything.
type TokenSource struct {
	Store *Store
}

// NewTokenSource returns a TokenSource for the streaming transport.
func NewTokenSource() *TokenSource { return &TokenSource{Store: NewStore()} }

// AccessToken returns a currently valid access token, refreshing if needed.
func (t *TokenSource) AccessToken(ctx context.Context) (string, error) {
	store := t.Store
	if store == nil {
		store = NewStore()
	}
	credential, err := Resolve(ctx, store)
	if err != nil {
		return "", err
	}
	return credential.AccessToken, nil
}

// RefreshToken forces a refresh, used when a request is rejected before its
// recorded expiry.
func (t *TokenSource) RefreshToken(ctx context.Context) (string, error) {
	store := t.Store
	if store == nil {
		store = NewStore()
	}
	credential, err := Load()
	if err != nil {
		return "", err
	}
	refreshed, err := refresh(ctx, credential, store)
	if err != nil {
		return "", err
	}
	if err := Save(refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

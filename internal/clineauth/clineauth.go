// Package clineauth holds the Cline Account device login and its stored credential.
//
// A Cline Account is not a provider. It is a hosted gateway: api.cline.bot serves
// chat completions for a set of models that Cline pays for, including a free tier,
// metered against the account's plan rather than an API key. The device login
// only proves the user to WorkOS; exchanging those identity tokens at Cline's
// own registration endpoint yields the account token for that gateway.
//
// The identity flow is WorkOS AuthKit, Cline's identity provider, and it is a
// plain RFC 8628 device authorization grant: no PKCE, no browser tab, no cookie
// harvesting. That is what makes this wireable. The account registration and
// refresh are JSON calls on Cline's own API, and the free models are served by an
// OpenAI-compatible /chat/completions, which Ceraa already speaks.
//
// The endpoints and client ids were read from the Cline SDK and then confirmed
// against the live service: a device code request returned 200 with a user code
// and a verification URI, and the recommended-models catalog is readable with no
// credential at all. Nothing past the device code has been exercised against a
// live account.
package clineauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ceraa/internal/oauthdevice"
	"ceraa/internal/secret"
)

// ProviderID is the id this package registers under.
const ProviderID = "cline"

// Endpoints, from the Cline SDK's auth module and confirmed live.
const (
	// APIBase is Cline's own API, not the identity provider and not the chat
	// path. Registration and refresh live directly under it.
	APIBase = "https://api.cline.bot"
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

// CredentialKindAccount marks a credential the Cline API issued. A stored
// credential without a kind is a WorkOS identity token saved before the
// registration step existed, never a gateway credential.
const CredentialKindAccount = "cline-account"

// TokenPrefix is the marker Cline requires in front of a gateway credential.
const TokenPrefix = "workos:"

// Credential is a resolved Cline Account credential.
type Credential struct {
	Kind         string
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresAt    time.Time
	Email        string
	AccountID    string
}

// APIKey returns the credential in the form the gateway accepts. Cline stores
// the account token bare and prefixes it on the wire.
func (c Credential) APIKey() string {
	token := strings.TrimSpace(c.AccessToken)
	if strings.HasPrefix(strings.ToLower(token), TokenPrefix) {
		return token
	}
	return TokenPrefix + token
}

type record struct {
	Kind         string `json:"kind"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Email        string `json:"email"`
	AccountID    string `json:"account_id"`
	ExpiresAt    string `json:"expires_at"`
}

// Store reads and writes the stored credential.
type Store struct {
	HTTPClient  *http.Client
	RefreshSkew time.Duration
	// RefreshEndpoint overrides where a refresh is sent. Empty means Cline's own
	// API. A test needs this because that host is absolute.
	RefreshEndpoint string
	// RegisterEndpoint overrides where a WorkOS token is exchanged. Empty means
	// Cline's own API. A test needs this because that host is absolute.
	RegisterEndpoint string
	// DeviceEndpoints overrides where a legacy WorkOS credential is refreshed.
	// Empty means the live identity provider. A test needs this because that
	// host is absolute.
	DeviceEndpoints oauthdevice.Endpoints
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
		Kind:         strings.TrimSpace(stored.Kind),
		AccessToken:  strings.TrimSpace(stored.AccessToken),
		RefreshToken: strings.TrimSpace(stored.RefreshToken),
		TokenType:    strings.TrimSpace(stored.TokenType),
		Email:        strings.TrimSpace(stored.Email),
		AccountID:    strings.TrimSpace(stored.AccountID),
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
	if strings.TrimSpace(credential.Kind) == "" {
		credential.Kind = CredentialKindAccount
	}
	encoded, err := json.Marshal(record{
		Kind:         credential.Kind,
		AccessToken:  credential.AccessToken,
		RefreshToken: credential.RefreshToken,
		TokenType:    firstNonEmpty(credential.TokenType, "Bearer"),
		Email:        credential.Email,
		AccountID:    credential.AccountID,
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

// Login runs the device login and persists the account credential.
type Login struct {
	manager          *oauthdevice.Manager
	client           *http.Client
	registerEndpoint string
}

// NewLogin returns a Login that saves into the encrypted store.
func NewLogin(client *http.Client) *Login {
	return NewLoginWithRegistration(client, DeviceEndpoints(), "")
}

// NewLoginWithEndpoints builds a Login pointed at explicit endpoints.
//
// Exists because the endpoints are absolute constants, so a test server cannot
// intercept a login by passing an HTTP client alone: the request would go to the
// real WorkOS with a client that only trusts the stub's certificate. The
// registration endpoint stays on Cline's API unless the registration-aware
// constructor is used.
func NewLoginWithEndpoints(client *http.Client, endpoints oauthdevice.Endpoints) *Login {
	return NewLoginWithRegistration(client, endpoints, "")
}

// NewLoginWithRegistration builds a Login pointed at explicit device and
// registration endpoints. Tests use it because both hosts are absolute.
func NewLoginWithRegistration(client *http.Client, endpoints oauthdevice.Endpoints, registerEndpoint string) *Login {
	if registerEndpoint == "" {
		registerEndpoint = defaultRegisterEndpoint()
	}
	config := ManagerConfig(client)
	config.Endpoints = endpoints
	manager := oauthdevice.NewManager(config)
	login := &Login{manager: manager, client: client, registerEndpoint: registerEndpoint}
	manager.SetOnToken(func(flowID string, token oauthdevice.Token) error {
		account, err := registerAccount(context.Background(), login.clientOrDefault(), login.registerEndpoint, RequestHeaders(flowID), token.AccessToken, token.RefreshToken)
		if err != nil {
			return err
		}
		return Save(account)
	})
	return login
}

func (l *Login) clientOrDefault() *http.Client {
	if l.client != nil {
		return l.client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Manager exposes the underlying device-login manager.
func (l *Login) Manager() *oauthdevice.Manager { return l.manager }

// Start begins the device login.
func (l *Login) Start(ctx context.Context) (*oauthdevice.Flow, error) { return l.manager.Start(ctx) }

// defaultRegisterEndpoint is where a WorkOS token is exchanged for a Cline
// account credential when no override is set.
//
// A function rather than a constant so a test can pin it. A test that overrides
// the endpoint proves nothing about the default, which is what production uses,
// and an exchange sent to the identity provider instead would ask WorkOS for a
// credential only Cline can issue.
func defaultRegisterEndpoint() string { return APIBase + "/api/v1/auth/register" }

// defaultRefreshEndpoint is where a refresh goes when no override is set.
//
// A function rather than a constant so a test can pin it. A test that overrides
// the endpoint proves nothing about the default, which is what production uses,
// and a refresh sent to the identity provider instead would be a credential on
// the wrong host.
func defaultRefreshEndpoint() string { return APIBase + "/api/v1/auth/refresh" }

// accountResponse is Cline's envelope for an issued account credential.
type accountResponse struct {
	Success bool `json:"success"`
	Data    *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresAt    string `json:"expiresAt"`
		UserInfo     *struct {
			Email       string `json:"email"`
			ClineUserID string `json:"clineUserId"`
		} `json:"userInfo"`
	} `json:"data"`
}

// registerAccount exchanges WorkOS identity tokens for the Cline account
// credential the gateway accepts. The device flow alone is not a sign-in: it
// proves who the user is to WorkOS, while this call creates the account token
// Cline bills against.
func registerAccount(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, workosAccess, workosRefresh string) (Credential, error) {
	workosAccess = strings.TrimSpace(workosAccess)
	workosRefresh = strings.TrimSpace(workosRefresh)
	if workosAccess == "" || workosRefresh == "" {
		return Credential{}, errors.New("cline device login did not include both WorkOS tokens; sign in again")
	}
	body, err := json.Marshal(map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	})
	if err != nil {
		return Credential{}, err
	}
	payload, err := postAccountJSON(ctx, client, endpoint, headers, body, "register cline credential")
	if err != nil {
		return Credential{}, err
	}
	return accountCredential(payload)
}

// accountCredential reads Cline's issued credential out of its envelope.
func accountCredential(payload accountResponse) (Credential, error) {
	if !payload.Success || payload.Data == nil {
		return Credential{}, errors.New("register cline credential: unsuccessful response")
	}
	access := strings.TrimSpace(payload.Data.AccessToken)
	refresh := strings.TrimSpace(payload.Data.RefreshToken)
	if access == "" || refresh == "" {
		return Credential{}, errors.New("register cline credential: response did not include both account tokens")
	}
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(payload.Data.ExpiresAt))
	if err != nil {
		return Credential{}, errors.New("register cline credential: unreadable expiry")
	}
	credential := Credential{
		Kind:         CredentialKindAccount,
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    firstNonEmpty(payload.Data.TokenType, "Bearer"),
		ExpiresAt:    expires,
	}
	if payload.Data.UserInfo != nil {
		credential.Email = strings.TrimSpace(payload.Data.UserInfo.Email)
		credential.AccountID = strings.TrimSpace(payload.Data.UserInfo.ClineUserID)
	}
	return credential, nil
}

// postAccountJSON sends one JSON call to Cline's own API and decodes its
// account envelope.
func postAccountJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, body []byte, action string) (accountResponse, error) {
	if err := validateAPIEndpoint(endpoint); err != nil {
		return accountResponse{}, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return accountResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	for key, value := range headers {
		if strings.TrimSpace(value) == "" {
			continue
		}
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return accountResponse{}, fmt.Errorf("%s: %w", action, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, oauthdevice.MaxResponseBytes))
	if err != nil {
		return accountResponse{}, fmt.Errorf("%s: %w", action, err)
	}
	var payload accountResponse
	if json.Unmarshal(raw, &payload) != nil {
		return accountResponse{}, fmt.Errorf("%s: unreadable response", action)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return accountResponse{}, fmt.Errorf("%s: HTTP %d", action, response.StatusCode)
	}
	return payload, nil
}

// validateAPIEndpoint refuses anything that is not HTTPS with a host.
//
// An account token is sent to the registration and refresh calls. Being handed
// one over plain HTTP, or with no host at all, is a configuration error worth
// failing on rather than sending a credential anyway.
func validateAPIEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("refusing non-HTTPS Cline API endpoint")
	}
	return nil
}

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
	credential, err := resolveStoredCredential(ctx, store)
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

// resolveStoredCredential loads the credential and completes a legacy one.
// Credentials stored before registration existed are WorkOS identity tokens, so
// they must never be sent to the gateway as if they were account tokens.
func resolveStoredCredential(ctx context.Context, store *Store) (Credential, error) {
	credential, err := Load()
	if err != nil {
		return Credential{}, err
	}
	if credential.Kind == CredentialKindAccount {
		return credential, nil
	}
	return upgradeLegacyCredential(ctx, store, credential)
}

// upgradeLegacyCredential exchanges a stored WorkOS identity token for the
// account credential Cline issues for it. This repairs a sign-in that stopped
// after the device flow, without asking the user to approve another code while
// the identity token is still usable.
func upgradeLegacyCredential(ctx context.Context, store *Store, credential Credential) (Credential, error) {
	client := storeClient(store)
	skew := store.RefreshSkew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	workosAccess := strings.TrimSpace(credential.AccessToken)
	workosRefresh := strings.TrimSpace(credential.RefreshToken)
	if workosAccess == "" {
		return Credential{}, errors.New("the stored cline sign-in has no identity token; sign in again")
	}
	if !credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(time.Now().Add(skew)) {
		if workosRefresh == "" {
			return Credential{}, errors.New("the stored cline sign-in has expired and no refresh token is stored; sign in again")
		}
		refreshed, err := workOSManager(client, storeDeviceEndpoints(store)).Refresh(ctx, workosRefresh)
		if err != nil {
			return Credential{}, fmt.Errorf("the stored cline sign-in could not be refreshed: %w; sign in again", err)
		}
		workosAccess = strings.TrimSpace(refreshed.AccessToken)
		workosRefresh = firstNonEmpty(refreshed.RefreshToken, workosRefresh)
		if workosAccess == "" {
			return Credential{}, errors.New("the stored cline sign-in refreshed without an identity token; sign in again")
		}
	}
	account, err := registerAccount(ctx, client, storeRegisterEndpoint(store), RequestHeaders(""), workosAccess, workosRefresh)
	if err != nil {
		return Credential{}, fmt.Errorf("the stored cline sign-in could not be completed: %w; sign in again", err)
	}
	if err := Save(account); err != nil {
		return Credential{}, err
	}
	return account, nil
}

func storeClient(store *Store) *http.Client {
	if store != nil && store.HTTPClient != nil {
		return store.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func storeRegisterEndpoint(store *Store) string {
	if store != nil && strings.TrimSpace(store.RegisterEndpoint) != "" {
		return strings.TrimSpace(store.RegisterEndpoint)
	}
	return defaultRegisterEndpoint()
}

func storeDeviceEndpoints(store *Store) oauthdevice.Endpoints {
	if store != nil && store.DeviceEndpoints.DeviceCode != "" && store.DeviceEndpoints.Token != "" {
		return store.DeviceEndpoints
	}
	return DeviceEndpoints()
}

func workOSManager(client *http.Client, endpoints oauthdevice.Endpoints) *oauthdevice.Manager {
	config := ManagerConfig(client)
	config.HTTPClient = client
	config.Endpoints = endpoints
	return oauthdevice.NewManager(config)
}

func refresh(ctx context.Context, credential Credential, store *Store) (Credential, error) {
	// The refresh lives on Cline's own API rather than on the identity provider,
	// so it cannot go through the device manager, whose token endpoint is WorkOS.
	// The gateway's refresh is a JSON call carrying the account refresh token,
	// not a bearer on an empty form post.
	endpoint := storeRefreshEndpoint(store)
	client := storeClient(store)
	body, err := json.Marshal(map[string]string{
		"refreshToken": credential.RefreshToken,
		"grantType":    "refresh_token",
	})
	if err != nil {
		return Credential{}, err
	}
	payload, err := postAccountJSON(ctx, client, endpoint, RequestHeaders(""), body, "refresh cline credential")
	if err != nil {
		return Credential{}, err
	}
	refreshed, err := accountCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	// A refresh response carries the account; keep the stored identity fields
	// only when it omits them rather than wiping what sign-in learned.
	if refreshed.Email == "" {
		refreshed.Email = credential.Email
	}
	if refreshed.AccountID == "" {
		refreshed.AccountID = credential.AccountID
	}
	return refreshed, nil
}

func storeRefreshEndpoint(store *Store) string {
	if store != nil && strings.TrimSpace(store.RefreshEndpoint) != "" {
		return strings.TrimSpace(store.RefreshEndpoint)
	}
	return defaultRefreshEndpoint()
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

// AccessToken returns the currently valid gateway credential, refreshing if needed.
func (t *TokenSource) AccessToken(ctx context.Context) (string, error) {
	store := t.Store
	if store == nil {
		store = NewStore()
	}
	credential, err := Resolve(ctx, store)
	if err != nil {
		return "", err
	}
	return credential.APIKey(), nil
}

// RefreshToken forces a refresh, used when a request is rejected before its
// recorded expiry.
func (t *TokenSource) RefreshToken(ctx context.Context) (string, error) {
	store := t.Store
	if store == nil {
		store = NewStore()
	}
	credential, err := resolveStoredCredential(ctx, store)
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
	return refreshed.APIKey(), nil
}

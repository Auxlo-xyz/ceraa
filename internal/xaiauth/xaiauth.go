// Package xaiauth holds the xAI (Grok) device login and its stored credential.
//
// xAI authenticates two ways: a Console API key, which is an ordinary OpenAI-style
// provider, and an OAuth login against a SuperGrok or X Premium subscription,
// which needs no key at all. Only the OAuth path lives here, because the API-key
// path is already covered by the generic custom provider.
//
// Every constant below was read from the OpenClaw xAI plugin
// (extensions/xai/xai-oauth.ts) and then confirmed against the live xAI issuer, so
// they are not guesses. The device-code request was issued for real and returned
// 200, and the token endpoint returned the expected authorization_pending for an
// unapproved code. Nothing past that point has been exercised against a live
// credential.
package xaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ceraa/internal/oauthdevice"
	"ceraa/internal/secret"
)

// ProviderID is the id this package registers under.
const ProviderID = "xai"

// Endpoints and the OAuth client, all confirmed against the live issuer.
const (
	// Issuer is the xAI OAuth issuer.
	Issuer = "https://auth.x.ai"
	// DiscoveryURL is the OIDC discovery document. Endpoints are read from it
	// rather than hardcoded, because this issuer publishes them and they are the
	// authority; the constants below are the values it returned.
	DiscoveryURL = Issuer + "/.well-known/openid-configuration"
	// ClientID is xAI's shared OAuth client id, the same one OpenClaw uses.
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// Scope includes offline_access, without which xAI issues no refresh token
	// and the login dies an hour later with nothing to refresh.
	Scope = "openid profile email offline_access grok-cli:access api:access"
	// InferenceBaseURL is where Grok requests go. It is an OpenAI-compatible
	// Responses API host, not chat/completions.
	InferenceBaseURL = "https://api.x.ai/v1"
)

// ErrNoCredential is returned when nothing usable is stored.
var ErrNoCredential = errors.New("xai credential not found")

// secretPrefix namespaces xAI credentials in the encrypted store.
const secretPrefix = "ceraa_xai_"

func secretName() string { return secretPrefix + ProviderID }

// Credential is a resolved xAI credential.
type Credential struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	BaseURL      string
	Scope        string
	ExpiresAt    time.Time
}

type record struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	BaseURL      string `json:"base_url"`
	ExpiresAt    string `json:"expires_at"`
}

// Store reads and writes the stored xAI credential.
type Store struct {
	HTTPClient *http.Client
	// RefreshSkew is how long before expiry a refresh is attempted.
	RefreshSkew time.Duration
	// Endpoints overrides the device-flow URLs. Empty means discover, then fall
	// back to the known-good constants. A test needs this to point a login at a
	// stub server, because the real endpoints are absolute.
	Endpoints oauthdevice.Endpoints
}

// NewStore returns a Store with production defaults.
func NewStore() *Store {
	return &Store{HTTPClient: &http.Client{Timeout: 20 * time.Second}, RefreshSkew: 2 * time.Minute}
}

// SignedIn reports whether a usable credential is stored.
//
// A record that exists but carries no access token does not count: the menu would
// offer a switch that then fails.
func SignedIn() bool {
	_, err := Load()
	return err == nil
}

// Load returns the stored credential, or ErrNoCredential.
func Load() (Credential, error) {
	value, err := secret.Get(secretName())
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
		Scope:        strings.TrimSpace(stored.Scope),
		BaseURL:      firstNonEmpty(stored.BaseURL, InferenceBaseURL),
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
		return errors.New("refusing to store an xai credential with no access token")
	}
	encoded, err := json.Marshal(record{
		AccessToken:  credential.AccessToken,
		RefreshToken: credential.RefreshToken,
		TokenType:    firstNonEmpty(credential.TokenType, "Bearer"),
		Scope:        credential.Scope,
		BaseURL:      firstNonEmpty(credential.BaseURL, InferenceBaseURL),
		ExpiresAt:    credential.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return secret.Set(secretName(), string(encoded))
}

// Discovery is the subset of the OIDC document this package needs.
type Discovery struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// Discover reads the issuer's OIDC document.
//
// Endpoints are taken from the document rather than hardcoded, but the issuer is
// fixed: a discovery document fetched from one host can name a different host for
// the endpoints, and a login that then posts a user code and refresh token to
// that host has been redirected. Both are checked for the expected origin below.
func Discover(ctx context.Context, client *http.Client) (Discovery, error) {
	return DiscoverFrom(ctx, DiscoveryURL, client)
}

// DiscoverFrom reads a discovery document from an explicit URL.
//
// Split out because DiscoveryURL is an absolute constant, so a test cannot point
// Discover at a stub by passing an HTTP client: it would read the real issuer and
// pass whatever the stub claimed. That made the off-origin check look covered
// while nothing was actually exercised.
func DiscoverFrom(ctx context.Context, discoveryURL string, client *http.Client) (Discovery, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return Discovery{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return Discovery{}, fmt.Errorf("xai oauth discovery: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Discovery{}, fmt.Errorf("xai oauth discovery: HTTP %d", response.StatusCode)
	}
	var document Discovery
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return Discovery{}, fmt.Errorf("xai oauth discovery: %w", err)
	}
	if document.DeviceAuthorizationEndpoint == "" || document.TokenEndpoint == "" {
		return Discovery{}, errors.New("xai oauth discovery is missing device code endpoints")
	}
	for _, endpoint := range []string{document.DeviceAuthorizationEndpoint, document.TokenEndpoint} {
		if !sameOrigin(endpoint, Issuer) {
			return Discovery{}, fmt.Errorf("xai oauth discovery names an endpoint on a different host: %s", endpoint)
		}
	}
	return document, nil
}

func sameOrigin(endpoint, issuer string) bool {
	base := strings.TrimSuffix(issuer, "/")
	if !strings.HasPrefix(endpoint, base+"/") {
		return false
	}
	rest := strings.TrimPrefix(endpoint, base+"/")
	// A path is fine, another scheme or authority is not.
	return !strings.Contains(strings.SplitN(rest, "/", 2)[0], ".")
}

// EndpointsOrDefault returns the discovered endpoints, or the known-good
// constants when discovery is unavailable.
func EndpointsOrDefault(ctx context.Context, client *http.Client) oauthdevice.Endpoints {
	endpoints, err := Endpoints(ctx, client)
	if err != nil {
		return ManagerConfig().Endpoints
	}
	return endpoints
}

// Endpoints returns the verified device-flow endpoints for xAI.
func Endpoints(ctx context.Context, client *http.Client) (oauthdevice.Endpoints, error) {
	document, err := Discover(ctx, client)
	if err != nil {
		// Fall back to the paths the issuer is known to publish, so a discovery
		// outage does not block sign-in. They are the same paths discovery
		// returned when it was reachable.
		return oauthdevice.Endpoints{
			DeviceCode: Issuer + "/oauth2/device/code",
			Token:      Issuer + "/oauth2/token",
		}, nil
	}
	return oauthdevice.Endpoints{DeviceCode: document.DeviceAuthorizationEndpoint, Token: document.TokenEndpoint}, nil
}

// NewManager builds a device-login manager for xAI.
func NewManager(client *http.Client) *oauthdevice.Manager {
	endpoints, _ := Endpoints(context.Background(), client)
	return oauthdevice.NewManager(withEndpoints(ManagerConfig(), endpoints, client))
}

// ManagerConfig is the xAI device-flow configuration.
//
// Exported so a caller can inject a test HTTP client without a package-level
// global to mutate.
func ManagerConfig() oauthdevice.Config {
	return oauthdevice.Config{
		ProviderID: ProviderID,
		ClientID:   ClientID,
		Scope:      Scope,
		Endpoints: oauthdevice.Endpoints{
			DeviceCode: Issuer + "/oauth2/device/code",
			Token:      Issuer + "/oauth2/token",
		},
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Login is a thin wrapper that persists the token on success, so a caller cannot
// accidentally leave a completed login with nowhere to put it.
type Login struct {
	manager *oauthdevice.Manager
	once    sync.Once
}

// NewLogin returns a Login that saves into the encrypted store.
func NewLogin(client *http.Client) *Login {
	return NewLoginWithConfig(withEndpoints(ManagerConfig(), EndpointsOrDefault(context.Background(), client), client))
}

// NewLoginWithConfig builds a Login from an explicit configuration.
//
// Exists because the endpoints are absolute constants, so a test server cannot
// intercept a login by passing an HTTP client alone: the request would go to the
// real issuer with a client that only trusts the stub's certificate. Overriding
// the endpoints is the only way to exercise the flow without touching xAI.
func NewLoginWithConfig(config oauthdevice.Config) *Login {
	manager := oauthdevice.NewManager(config)
	login := &Login{manager: manager}
	manager.SetOnToken(func(flowID string, token oauthdevice.Token) error {
		return Save(Credential{
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			Scope:        token.Scope,
			BaseURL:      InferenceBaseURL,
			ExpiresAt:    token.ExpiresAt,
		})
	})
	return login
}

func withEndpoints(config oauthdevice.Config, endpoints oauthdevice.Endpoints, client *http.Client) oauthdevice.Config {
	config.Endpoints = endpoints
	config.HTTPClient = client
	return config
}

// Manager exposes the underlying device-login manager.
func (l *Login) Manager() *oauthdevice.Manager { return l.manager }

// Start begins the device login.
func (l *Login) Start(ctx context.Context) (*oauthdevice.Flow, error) { return l.manager.Start(ctx) }

// Resolve returns a usable credential, refreshing it if it is close to expiry.
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
		// No refresh token means the scope was refused, or the login predates it.
		// Saying so is more useful than sending a token that is about to 401.
		return Credential{}, errors.New("xai credential has expired and no refresh token is stored; sign in again")
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
	client := store.HTTPClient
	endpoints := store.Endpoints
	if endpoints.DeviceCode == "" || endpoints.Token == "" {
		endpoints = EndpointsOrDefault(ctx, client)
	}
	manager := oauthdevice.NewManager(withEndpoints(ManagerConfig(), endpoints, client))
	token, err := manager.Refresh(ctx, credential.RefreshToken)
	if err != nil {
		return Credential{}, fmt.Errorf("refresh xai credential: %w", err)
	}
	refreshed := Credential{
		AccessToken: token.AccessToken,
		// A refresh response often omits refresh_token, meaning "keep using the
		// one you have". Dropping it would sign the user out at the next expiry.
		RefreshToken: firstNonEmpty(token.RefreshToken, credential.RefreshToken),
		TokenType:    firstNonEmpty(token.TokenType, credential.TokenType),
		Scope:        firstNonEmpty(token.Scope, credential.Scope),
		BaseURL:      firstNonEmpty(credential.BaseURL, InferenceBaseURL),
		ExpiresAt:    token.ExpiresAt,
	}
	if refreshed.AccessToken == "" {
		return Credential{}, errors.New("xai refresh returned no access token")
	}
	return refreshed, nil
}

// TokenSource adapts the stored credential to the transport's credential
// interface, so the llm package does not have to know how xAI stores anything.
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

// RefreshToken forces a refresh and returns the new access token.
//
// Used by the transport when a request comes back 401 before the recorded expiry,
// which means the credential was revoked server-side.
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

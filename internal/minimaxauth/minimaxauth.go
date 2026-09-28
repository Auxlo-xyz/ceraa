// Package minimaxauth holds the MiniMax OAuth device login and its stored
// credential.
//
// MiniMax is a subscription provider: the credential comes from an account login
// rather than a Console API key, which is why it sits beside codexauth, hermesauth
// and xaiauth rather than in the plain custom provider.
//
// Every constant was read from the OpenClaw MiniMax plugin
// (extensions/minimax/oauth.ts) and then confirmed against the live service. The
// device-code request was issued for real and returned 200 with a user code, and
// the token endpoint answered {"status":"pending"} for an unapproved code, which is
// the behaviour the dialect exists to handle. Nothing past that point has been
// exercised against a live credential.
//
// Two regions exist and they are separate accounts, not aliases: minimax.io is the
// global service and minimaxi.com is the Chinese one, with their own login hosts.
// Region is therefore part of the configuration rather than a fallback.
package minimaxauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ceraa/internal/oauthdevice"
	"ceraa/internal/secret"
)

// ProviderID is the id this package registers under.
const ProviderID = "minimax"

// OAuthClientID is MiniMax's own OAuth client, read from its official CLI
// (github.com/MiniMax-AI/cli, src/auth/oauth.ts).
//
// This was previously OpenClaw's client id, and that was wrong in a way the token
// itself could not show: authorizing it lands on a consent page reading "Sign in
// with MiniMax to OpenClaw" whose action button is "Authorize & Start 1-Day Free
// Trial". The user was granting a stranger's registration a one-day trial, not
// their own plan. MiniMax publishes this client for coding tools, so use it.
const OAuthClientID = "659cf4c1-615c-45f6-a5f6-4bf15eb476e5"

// Scope is what the login asks for.
//
// coding_plan is the scope that names a subscription rather than a trial, and it
// is the whole reason the previous scope was wrong: `group_id profile
// model.completion` is the API-key provider's scope, so asking for it on the
// portal is what produced the free-trial offer.
const Scope = "openid profile coding_plan"

// Region selects which MiniMax deployment an account belongs to.
type Region string

const (
	// RegionGlobal is the international service.
	RegionGlobal Region = "global"
	// RegionCN is the Chinese service. It is a different account system, so
	// credentials are not interchangeable between the two.
	RegionCN Region = "cn"
)

// ErrNoCredential is returned when nothing usable is stored.
var ErrNoCredential = errors.New("minimax credential not found")

type endpoints struct {
	OAuthBase string
	Base      string
}

// endpointsFor returns the hosts for a region.
//
// From the OpenClaw plugin's MINIMAX_OAUTH_CONFIG. The inference base for the
// OAuth path is the /anthropic suffix, because the subscription is served by the
// Messages API and the plain /v1 is the OpenAI-compatible host that belongs to the
// API-key provider.
func endpointsFor(region Region) endpoints {
	if region == RegionCN {
		return endpoints{
			OAuthBase: "https://account.minimaxi.com",
			Base:      "https://api.minimaxi.com/anthropic",
		}
	}
	return endpoints{
		OAuthBase: "https://account.minimax.io",
		Base:      "https://api.minimax.io/anthropic",
	}
}

// secretPrefix namespaces MiniMax credentials in the encrypted store.
const secretPrefix = "ceraa_minimax_"

func secretName(region Region) string { return secretPrefix + string(region) }

// Credential is a resolved MiniMax credential.
type Credential struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	BaseURL      string
	Region       Region
	ExpiresAt    time.Time
}

type record struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	BaseURL      string `json:"base_url"`
	Region       string `json:"region"`
	ExpiresAt    string `json:"expires_at"`
}

// Store reads and writes the stored credential for one region.
type Store struct {
	Region      Region
	HTTPClient  *http.Client
	RefreshSkew time.Duration
	// Endpoints overrides the device-flow URLs. Empty means the region's own
	// hosts. A test needs this because those hosts are absolute.
	Endpoints oauthdevice.Endpoints
}

// NewStore returns a Store for a region with production defaults.
func NewStore(region Region) *Store {
	if region == "" {
		region = RegionGlobal
	}
	return &Store{
		Region:      region,
		HTTPClient:  &http.Client{Timeout: 20 * time.Second},
		RefreshSkew: 2 * time.Minute,
	}
}

// SignedIn reports whether a usable credential is stored for a region.
//
// A record that exists but carries no access token does not count, or the menu
// offers a switch that then fails.
func SignedIn(region Region) bool {
	_, err := Load(region)
	return err == nil
}

// Load returns the stored credential for a region, or ErrNoCredential.
func Load(region Region) (Credential, error) {
	if region == "" {
		region = RegionGlobal
	}
	value, err := secret.Get(secretName(region))
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
		BaseURL:      firstNonEmpty(stored.BaseURL, endpointsFor(region).Base),
		Region:       region,
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
		return errors.New("refusing to store a minimax credential with no access token")
	}
	region := credential.Region
	if region == "" {
		region = RegionGlobal
	}
	encoded, err := json.Marshal(record{
		AccessToken:  credential.AccessToken,
		RefreshToken: credential.RefreshToken,
		TokenType:    firstNonEmpty(credential.TokenType, "Bearer"),
		BaseURL:      firstNonEmpty(credential.BaseURL, endpointsFor(region).Base),
		Region:       string(region),
		ExpiresAt:    credential.ExpiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return secret.Set(secretName(region), string(encoded))
}

// pkcePair is a verifier and its S256 challenge.
//
// Required on the device-code request. RFC 8628 has no such field, which is why it
// has to be generated here and passed through the dialect: without it MiniMax
// rejects the code request.
type pkcePair struct {
	Verifier  string
	Challenge string
}

func newPKCE() (pkcePair, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return pkcePair{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return pkcePair{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// DeviceEndpoints returns the verified device-flow endpoints for a region.
func DeviceEndpoints(region Region) oauthdevice.Endpoints {
	hosts := endpointsFor(region)
	return oauthdevice.Endpoints{
		DeviceCode: hosts.OAuthBase + "/oauth2/device/code",
		Token:      hosts.OAuthBase + "/oauth2/token",
	}
}

// InferenceBaseURL returns the Messages API endpoint for a region.
func InferenceBaseURL(region Region) string { return endpointsFor(region).Base }

// Dialect is MiniMax's device-flow dialect, including both halves of this
// particular login's PKCE pair.
//
// Generated rather than a constant because the pair is per-login, and a fixed
// challenge would be a replayable secret published in the source. The whole pair
// is taken rather than the challenge alone: the official CLI sends code_challenge
// on the device-code request and code_verifier when the token is collected, and
// sending only the first is answered with "code_verifier is required".
//
// GrantType is left at the RFC 8628 device-code grant, which is what the official
// CLI posts to /oauth2/token. The user-code grant belonged to the older
// /oauth/code endpoint that shipped with OpenClaw's client.
func Dialect(pair pkcePair) oauthdevice.Dialect {
	return oauthdevice.Dialect{
		CodeField: "user_code",
		// Stated rather than inherited. The default happens to be the same value,
		// but this is MiniMax's contract and the old user-code grant belonged to
		// the endpoint this flow no longer uses.
		GrantType:                  oauthdevice.GrantTypeDeviceCode,
		ExpiryField:                "expired_in",
		ExpiryAbsoluteMilliseconds: true,
		IntervalIsMilliseconds:     true,
		PendingStatusField:         "status",
		PendingStatusValue:         "pending",
		ErrorStatusField:           "base_resp.status_code",
		ErrorMessageField:          "base_resp.status_msg",
		ExtraCodeParams: url.Values{
			"code_challenge":        {pair.Challenge},
			"code_challenge_method": {"S256"},
		},
		ExtraTokenParams: url.Values{
			"code_verifier": {pair.Verifier},
		},
	}
}

// ManagerConfig is the device-login configuration for a region.
func ManagerConfig(region Region, pair pkcePair, client *http.Client) oauthdevice.Config {
	return oauthdevice.Config{
		ProviderID: ProviderID,
		ClientID:   OAuthClientID,
		Scope:      Scope,
		Endpoints:  DeviceEndpoints(region),
		Dialect:    Dialect(pair),
		HTTPClient: client,
	}
}

// Login runs the device login and persists the token.
type Login struct {
	manager *oauthdevice.Manager
	region  Region
	once    sync.Once
}

// NewLoginWithEndpoints builds a Login pointed at explicit endpoints.
//
// Exists because the device endpoints are absolute constants, so a test server
// cannot intercept a login by passing an HTTP client alone: the request would go to
// the real MiniMax host with a client that only trusts the stub's certificate, and
// the failure looks like a TLS problem rather than a missing seam. This is the
// second time this trap has cost an hour; the override is the fix.
func NewLoginWithEndpoints(region Region, client *http.Client, endpoints oauthdevice.Endpoints) *Login {
	if region == "" {
		region = RegionGlobal
	}
	pair, err := newPKCE()
	if err != nil {
		pair = pkcePair{}
	}
	config := ManagerConfig(region, pair, client)
	config.Endpoints = endpoints
	login := &Login{manager: oauthdevice.NewManager(config), region: region}
	login.manager.SetOnToken(func(flowID string, token oauthdevice.Token) error {
		return Save(Credential{
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			BaseURL:      endpointsFor(region).Base,
			Region:       region,
			ExpiresAt:    token.ExpiresAt,
		})
	})
	return login
}

// NewLogin returns a Login for a region.
func NewLogin(region Region, client *http.Client) *Login {
	if region == "" {
		region = RegionGlobal
	}
	pair, err := newPKCE()
	if err != nil {
		// A login that cannot generate a challenge cannot be started, and the
		// caller sees this from Start rather than from a rejected request.
		pair = pkcePair{}
	}
	login := &Login{
		manager: oauthdevice.NewManager(ManagerConfig(region, pair, client)),
		region:  region,
	}
	login.manager.SetOnToken(func(flowID string, token oauthdevice.Token) error {
		return Save(Credential{
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			BaseURL:      endpointsFor(region).Base,
			Region:       region,
			ExpiresAt:    token.ExpiresAt,
		})
	})
	return login
}

// Region returns the region this login targets.
func (l *Login) Region() Region { return l.region }

// Manager exposes the underlying device-login manager.
func (l *Login) Manager() *oauthdevice.Manager { return l.manager }

// Start begins the device login.
func (l *Login) Start(ctx context.Context) (*oauthdevice.Flow, error) {
	return l.manager.Start(ctx)
}

// Resolve returns a usable credential for a region, refreshing when it is close to
// expiry.
func Resolve(ctx context.Context, store *Store) (Credential, error) {
	if store == nil {
		store = NewStore(RegionGlobal)
	}
	credential, err := Load(store.Region)
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
		// No refresh token means the scope was refused or the login predates it.
		// Saying so beats sending a credential that is about to 401.
		return Credential{}, errors.New("minimax credential has expired and no refresh token is stored; sign in again")
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
	region := credential.Region
	if region == "" {
		region = RegionGlobal
	}
	// The refresh uses a fresh challenge: the verifier from the original login was
	// never retained, and a token refresh with a stale verifier is rejected.
	pair, err := newPKCE()
	if err != nil {
		return Credential{}, err
	}
	config := ManagerConfig(region, pair, store.HTTPClient)
	if store.Endpoints.DeviceCode != "" && store.Endpoints.Token != "" {
		config.Endpoints = store.Endpoints
	}
	manager := oauthdevice.NewManager(config)
	token, err := manager.Refresh(ctx, credential.RefreshToken)
	if err != nil {
		return Credential{}, fmt.Errorf("refresh minimax credential: %w", err)
	}
	refreshed := Credential{
		AccessToken: token.AccessToken,
		// A refresh response often omits refresh_token, meaning "keep the one you
		// have". Dropping it signs the user out at the next expiry.
		RefreshToken: firstNonEmpty(token.RefreshToken, credential.RefreshToken),
		TokenType:    firstNonEmpty(token.TokenType, credential.TokenType),
		BaseURL:      firstNonEmpty(credential.BaseURL, endpointsFor(region).Base),
		Region:       region,
		ExpiresAt:    token.ExpiresAt,
	}
	if refreshed.AccessToken == "" {
		return Credential{}, errors.New("minimax refresh returned no access token")
	}
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
// interface, so the llm package does not know how MiniMax stores anything.
type TokenSource struct {
	Store *Store
}

// NewTokenSource returns a TokenSource for a region.
func NewTokenSource(region Region) *TokenSource { return &TokenSource{Store: NewStore(region)} }

// AccessToken returns a currently valid access token, refreshing if needed.
func (t *TokenSource) AccessToken(ctx context.Context) (string, error) {
	store := t.Store
	if store == nil {
		store = NewStore(RegionGlobal)
	}
	credential, err := Resolve(ctx, store)
	if err != nil {
		return "", err
	}
	return credential.AccessToken, nil
}

// RefreshToken forces a refresh and returns the new access token, used by the
// transport when a request is rejected before its recorded expiry.
func (t *TokenSource) RefreshToken(ctx context.Context) (string, error) {
	store := t.Store
	if store == nil {
		store = NewStore(RegionGlobal)
	}
	credential, err := Load(store.Region)
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

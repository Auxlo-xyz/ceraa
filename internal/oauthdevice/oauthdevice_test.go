package oauthdevice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A token endpoint receives a client id, a device code and, on refresh, a refresh
// token. Anything that is not HTTPS with a host must be refused rather than sent
// to, so the scheme check is a security control and not a formality.
func TestNonHTTPSEndpointsAreRefused(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
	}{
		{"plain http", "http://auth.x.ai/oauth2/token"},
		{"no scheme", "auth.x.ai/oauth2/token"},
		{"no host", "https:///oauth2/token"},
		{"empty", ""},
		{"file scheme", "file:///etc/passwd"},
		{"relative", "/oauth2/token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateEndpoint(tc.endpoint); err == nil {
				t.Errorf("validateEndpoint(%q) accepted it", tc.endpoint)
			}
		})
	}
}

func TestHTTPSEndpointsWithAHostAreAccepted(t *testing.T) {
	for _, endpoint := range []string{
		"https://auth.x.ai/oauth2/token",
		"https://auth.x.ai/oauth2/device/code",
	} {
		if err := validateEndpoint(endpoint); err != nil {
			t.Errorf("validateEndpoint(%q) refused it: %v", endpoint, err)
		}
	}
}

// The token response is provider-supplied, so a malformed one must not take the
// process down or be reported as a success.
func TestMalformedTokenResponseIsNotASuccess(t *testing.T) {
	m := NewManager(Config{
		ProviderID: "test",
		ClientID:   "cid",
		Scope:      "scope",
		Endpoints:  Endpoints{DeviceCode: "https://example.test/d", Token: "https://example.test/t"},
	})
	token, err := m.postForm(context.Background(), "https://example.test/t", nil)
	if err == nil {
		t.Errorf("a non-JSON body was accepted: %v", token)
	}
}

// A device-code response missing its essentials is not a usable login.
func TestIncompleteDeviceCodeResponseIsRejected(t *testing.T) {
	manager := &Manager{config: Config{
		ProviderID: "test", ClientID: "cid", Scope: "s",
		Endpoints:  Endpoints{DeviceCode: "https://example.test/d", Token: "https://example.test/t"},
		HTTPClient: &http.Client{Transport: deadTransport{}},
	}, flows: map[string]*Flow{}}
	if _, err := manager.requestDeviceCode(context.Background()); err == nil {
		t.Error("a device code response with no device_code was accepted")
	}
}

// deadTransport fails every request, standing in for a portal that is unreachable.
type deadTransport struct{}

func (deadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network unavailable")
}

// Expired must treat an unknown expiry as "not expired", because a provider that
// omits expires_in still issues a usable token and treating it as expired makes
// every request try to refresh a credential it cannot refresh.
func TestUnknownExpiryIsNotTreatedAsExpired(t *testing.T) {
	if (Token{}).Expired(0) {
		t.Error("a token with no recorded expiry was reported as expired")
	}
	if !(Token{ExpiresAt: time.Now().Add(-time.Hour)}).Expired(0) {
		t.Error("a token past its expiry was reported as current")
	}
	if (Token{ExpiresAt: time.Now().Add(time.Hour)}).Expired(time.Minute) {
		t.Error("a token well inside its lifetime was reported as expired")
	}
}

func TestPollErrorSeparatesPendingFromFailure(t *testing.T) {
	pending := &PollError{Code: "authorization_pending", Message: "pending"}
	if !pending.IsPending() {
		t.Error("authorization_pending is not treated as pending")
	}
	slow := &PollError{Code: "slow_down", Message: "slow"}
	if !slow.IsPending() {
		t.Error("slow_down is not treated as pending")
	}
	denied := &PollError{Code: "access_denied", Message: "denied"}
	if denied.IsPending() {
		t.Error("access_denied is treated as pending, so a refused login would poll until it expired")
	}
	if !strings.Contains(denied.Error(), "denied") {
		t.Errorf("the error message lost its detail: %q", denied.Error())
	}
}

// Testing validateEndpoint directly proved nothing about whether it is called.
// Both mutations that removed the call site passed every test, because the check
// was exercised in isolation while the request path that depends on it was not.
func TestPostFormRefusesANonHTTPSEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://auth.x.ai/oauth2/token",
		"file:///etc/passwd",
		"auth.x.ai/oauth2/token",
	} {
		manager := NewManager(Config{
			ProviderID: "test", ClientID: "cid", Scope: "s",
			Endpoints:  Endpoints{DeviceCode: "https://example.test/d", Token: endpoint},
			HTTPClient: &http.Client{Transport: &countingTransport{}},
		})
		if _, err := manager.postForm(context.Background(), endpoint, nil); err == nil {
			t.Errorf("postForm(%q) was allowed through", endpoint)
		}
	}
}

// And a request that reaches postForm with a good endpoint must actually be sent,
// so the check cannot be "pass" by refusing everything.
func TestPostFormSendsToAnHTTPSEndpoint(t *testing.T) {
	transport := &countingTransport{}
	manager := NewManager(Config{
		ProviderID: "test", ClientID: "cid", Scope: "s",
		Endpoints:  Endpoints{DeviceCode: "https://example.test/d", Token: "https://example.test/t"},
		HTTPClient: &http.Client{Transport: transport},
	})
	if _, err := manager.postForm(context.Background(), "https://example.test/t", nil); err != nil {
		t.Fatalf("an HTTPS endpoint was refused: %v", err)
	}
	if transport.calls == 0 {
		t.Error("no request was made; postForm is refusing everything")
	}
}

// countingTransport records that a request left the process and answers with a
// minimal JSON body.
type countingTransport struct {
	calls int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls++
	// A 200 with a usable body, so this transport exercises the success path
	// rather than the error path the pending tests cover.
	body := strings.NewReader(`{"access_token":"a"}`)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(body),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

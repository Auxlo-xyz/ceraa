package oauthdevice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func minimaxDialect() Dialect {
	return Dialect{
		CodeField:                  "user_code",
		GrantType:                  "urn:ietf:params:oauth:grant-type:user_code",
		ExpiryField:                "expired_in",
		ExpiryAbsoluteMilliseconds: true,
		IntervalIsMilliseconds:     true,
		PendingStatusField:         "status",
		PendingStatusValue:         "pending",
		ErrorStatusField:           "base_resp.status_code",
		ErrorStatusValue:           0,
		ErrorMessageField:          "base_resp.status_msg",
		ExtraCodeParams:            url.Values{"code_challenge": {"abc"}, "code_challenge_method": {"S256"}},
	}
}

// The bug this exists to stop: MiniMax answers an unapproved poll with HTTP 200
// and {"status":"pending"}. A standard reader treats that as a token response, so
// the login reports success and then fails on a missing access token, which looks
// like a broken provider rather than a poll still in progress.
func TestPendingOnA200IsNotASuccess(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/d"):
			_, _ = w.Write([]byte(`{"user_code":"U-1","verification_uri":"https://example.test/v","expired_in":1790519361298,"interval":3000}`))
		default:
			// 200 with a pending body, exactly as MiniMax answers.
			_, _ = w.Write([]byte(`{"status":"pending"}`))
		}
	}))
	defer srv.Close()

	manager := NewManager(Config{
		ProviderID: "minimax", ClientID: "cid", Scope: "s",
		Dialect:    minimaxDialect(),
		Endpoints:  Endpoints{DeviceCode: srv.URL + "/d", Token: srv.URL + "/t"},
		HTTPClient: srv.Client(),
	})
	manager.SetOnToken(func(string, Token) error { return nil })
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := flow.Snapshot().Status; got != StatusPending {
		t.Errorf("status = %q after a 200 pending body, want %q; a 2xx was read as a token", got, StatusPending)
	}
	_ = manager.Cancel(flow.ID)
}

// An absolute epoch-millisecond expiry read as seconds is the year 58709, so a
// token would look permanently valid and never refresh.
func TestAbsoluteMillisecondExpiryIsNotReadAsSeconds(t *testing.T) {
	// The value the live service returned.
	const live = 1790519361298
	manager := NewManager(Config{ProviderID: "minimax", ClientID: "c", Dialect: minimaxDialect()})
	manager.config.Now = func() time.Time { return time.UnixMilli(live) }

	token := manager.tokenFromPayload(map[string]any{"expired_in": float64(live)})
	if token.ExpiresAt.IsZero() {
		t.Fatal("the absolute expiry was dropped")
	}
	// Read as seconds this lands centuries out; as an epoch it is about now.
	if d := token.ExpiresAt.Sub(time.UnixMilli(live)); d > time.Minute || d < -time.Minute {
		t.Errorf("expiry resolved to %s relative to the timestamp, want roughly now", d)
	}
	if token.ExpiresAt.Year() > 2100 {
		t.Errorf("expiry year is %d, so it was read as seconds", token.ExpiresAt.Year())
	}
}

// The poll interval is milliseconds, so 3000 is five seconds rather than fifty
// minutes.
func TestMillisecondIntervalIsConverted(t *testing.T) {
	if got := durationSeconds(3000.0, true); got != 3 {
		t.Errorf("3000ms became %vs, want 3s", got)
	}
	if got := durationSeconds(5.0, false); got != 5 {
		t.Errorf("5s became %vs, want 5s", got)
	}
}

// The code field is user_code, and it is echoed under that name in the poll.
func TestCodeFieldIsUsedForThePoll(t *testing.T) {
	var pollForm url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := r.ParseForm(); err == nil && strings.HasSuffix(r.URL.Path, "/t") {
			pollForm = r.PostForm
			_, _ = w.Write([]byte(`{"status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"user_code":"U-9","verification_uri":"https://example.test/v","expired_in":1790519361298,"interval":3000}`))
	}))
	defer srv.Close()

	manager := NewManager(Config{
		ProviderID: "minimax", ClientID: "cid", Scope: "s",
		Dialect:    minimaxDialect(),
		Endpoints:  Endpoints{DeviceCode: srv.URL + "/d", Token: srv.URL + "/t"},
		HTTPClient: srv.Client(),
	})
	manager.SetOnToken(func(string, Token) error { return nil })
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	_ = manager.Cancel(flow.ID)

	if pollForm == nil {
		t.Fatal("no poll was made")
	}
	if got := pollForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:user_code" {
		t.Errorf("grant_type = %q", got)
	}
	if got := pollForm.Get("user_code"); got != "U-9" {
		t.Errorf("the code was not sent as user_code; form was %v", pollForm)
	}
	if pollForm.Has("device_code") {
		t.Errorf("the standard field was also sent: %v", pollForm)
	}
	// PKCE is required by this provider on the code request.
	_ = json.Valid
}

// A failure wrapped in a 2xx envelope must be reported as a failure.
func TestFailureInASuccessEnvelopeIsReported(t *testing.T) {
	if err := minimaxDialect().pending(map[string]any{
		"base_resp": map[string]any{"status_code": float64(1004), "status_msg": "invalid client"},
	}); err == nil {
		t.Error("a wrapped failure on a 200 was treated as pending, so the login would poll until it expired")
	}
	if err := minimaxDialect().pending(map[string]any{
		"base_resp": map[string]any{"status_code": float64(0), "status_msg": "success"},
	}); err != nil {
		t.Errorf("a wrapped success was treated as a failure: %v", err)
	}
}

// The standard flow must be unchanged by all of this.
func TestStandardDialectIsUnaffected(t *testing.T) {
	standard := Standard().withDefaults()
	if standard.CodeField != "device_code" {
		t.Errorf("standard code field = %q", standard.CodeField)
	}
	if standard.GrantType != GrantTypeDeviceCode {
		t.Errorf("standard grant = %q", standard.GrantType)
	}
	var zero Dialect
	if got := zero.withDefaults().CodeField; got != "device_code" {
		t.Errorf("a zero dialect became %q, want the standard flow", got)
	}
}

// A provider that requires PKCE on the device-code request rejects the request
// without it, so the challenge has to be sent. RFC 8628 has no such field, which
// is why it has to be expressed as a dialect.
func TestPkceChallengeReachesTheCodeRequest(t *testing.T) {
	var codeForm url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := r.ParseForm(); err == nil && strings.HasSuffix(r.URL.Path, "/d") {
			codeForm = r.PostForm
			_, _ = w.Write([]byte(`{"user_code":"U-1","verification_uri":"https://example.test/v","expired_in":1790519361298,"interval":3000}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"pending"}`))
	}))
	defer srv.Close()

	manager := NewManager(Config{
		ProviderID: "minimax", ClientID: "cid", Scope: "s",
		Dialect:    minimaxDialect(),
		Endpoints:  Endpoints{DeviceCode: srv.URL + "/d", Token: srv.URL + "/t"},
		HTTPClient: srv.Client(),
	})
	manager.SetOnToken(func(string, Token) error { return nil })
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	_ = manager.Cancel(flow.ID)

	if codeForm == nil {
		t.Fatal("no code request was made")
	}
	if got := codeForm.Get("code_challenge"); got != "abc" {
		t.Errorf("code_challenge = %q, want the configured challenge; form was %v", got, codeForm)
	}
	if got := codeForm.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
}

// The device response's absolute expiry sets how long the flow waits. Read as a
// duration instead, a value of 1790519361298 is a wait of about 56,000 years, so
// the flow would never expire and the poll would run forever.
func TestAbsoluteExpirySetsTheFlowTimeout(t *testing.T) {
	// An absolute expiry two seconds out.
	expiry := time.Now().Add(2 * time.Second).UnixMilli()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/d") {
			_, _ = w.Write([]byte(`{"user_code":"U-1","verification_uri":"https://example.test/v","expired_in":` +
				itoa(expiry) + `,"interval":300}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"pending"}`))
	}))
	defer srv.Close()

	manager := NewManager(Config{
		ProviderID: "minimax", ClientID: "cid", Scope: "s",
		Dialect:    minimaxDialect(),
		Endpoints:  Endpoints{DeviceCode: srv.URL + "/d", Token: srv.URL + "/t"},
		HTTPClient: srv.Client(),
	})
	manager.SetOnToken(func(string, Token) error { return nil })
	flow, err := manager.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	wait := time.Until(flow.Snapshot().ExpiresAt)
	if wait <= 0 || wait > 10*time.Second {
		t.Errorf("the flow waits %s, want about 2s; the absolute expiry was not applied", wait)
	}
	_ = manager.Cancel(flow.ID)
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Join([]string{formatInt(n)}, ""))
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

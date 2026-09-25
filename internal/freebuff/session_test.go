package freebuff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newSessionServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *Client) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	client := NewClient()
	client.WebBaseURL = server.URL
	client.APIBaseURL = server.URL
	client.HTTPClient = server.Client()
	return server, client
}

// decodeHeaders flattens request headers into a case-insensitive lookup, since
// net/http canonicalizes header names ("x-freebuff-model" becomes
// "X-Freebuff-Model") and a literal map miss would hide a real header.
func decodeHeaders(r *http.Request) http.Header {
	return r.Header.Clone()
}

// TestSessionHeaderSets is the golden table for callFreebuffSession. Getting a
// header wrong is silent and hard to debug, so each branch is pinned.
func TestSessionHeaderSets(t *testing.T) {
	cases := []struct {
		name    string
		req     sessionRequest
		path    string
		want    map[string]string
		notWant []string
	}{
		{
			name: "single-session POST sends model, timezone, first-tab and spend limit but no instance id",
			req: sessionRequest{
				method: "POST", token: "t", instanceID: "inst-1", model: "m1", firstTabDiscount: false,
			},
			path: sessionAdmissionPath,
			want: map[string]string{
				"Authorization":        "Bearer t",
				headerModel:            "m1",
				headerWalletSpendLimit: "0",
				headerFirstTabDiscount: "0",
			},
			// Never on a plain POST, and never the multi-session opt-ins.
			notWant: []string{headerInstance, headerMultiSession, headerPurchaseContinuity, headerHeartbeat, headerCompactSession},
		},
		{
			name: "single-session GET sends the instance id and no compact flag",
			req:  sessionRequest{method: "GET", token: "t", instanceID: "inst-1", compact: false},
			path: sessionPath,
			want: map[string]string{
				"Authorization":        "Bearer t",
				headerInstance:         "inst-1",
				headerFirstTabDiscount: "0",
			},
			notWant: []string{headerCompactSession, headerModel, headerMultiSession, headerHeartbeat},
		},
		{
			name: "compact GET adds the compact header",
			req:  sessionRequest{method: "GET", token: "t", instanceID: "inst-1", compact: true},
			path: sessionPath,
			want: map[string]string{
				headerInstance:       "inst-1",
				headerCompactSession: "1",
			},
			notWant: []string{headerModel, headerWalletSpendLimit},
		},
		{
			name: "multi-session GET beats and asks for the quota snapshot",
			req:  sessionRequest{method: "GET", token: "t", instanceID: "cli:abc", multi: true, compact: false},
			path: sessionPath,
			want: map[string]string{
				headerInstance:             "cli:abc",
				headerMultiSession:         "1",
				headerPurchaseContinuity:   "1",
				headerHeartbeat:            "1",
				headerIncludeUnusedRateLim: "1",
			},
			notWant: []string{headerCompactSession},
		},
		{
			name: "multi-session compact GET omits the quota snapshot ask",
			req:  sessionRequest{method: "GET", token: "t", instanceID: "cli:abc", multi: true, compact: true},
			path: sessionPath,
			want: map[string]string{
				headerHeartbeat:      "1",
				headerCompactSession: "1",
			},
			notWant: []string{headerIncludeUnusedRateLim},
		},
		{
			// Unlike a single-session POST, a multi-session POST does carry the
			// instance id: the reference sends it whenever multiSession is set,
			// because the attempt id alone would not identify the row.
			name: "multi-session non-GET sends the attempt id, never the heartbeat",
			req:  sessionRequest{method: "POST", token: "t", instanceID: "cli:abc", attemptID: "abc", model: "m1"},
			path: sessionAdmissionPath,
			want: map[string]string{
				headerMultiSession:       "1",
				headerPurchaseContinuity: "1",
				headerDesktopAttempt:     "abc",
				headerModel:              "m1",
				headerInstance:           "cli:abc",
			},
			notWant: []string{headerHeartbeat, headerIncludeUnusedRateLim, headerCompactSession},
		},
		{
			name: "DELETE with an attempt id targets the attempt route",
			req:  sessionRequest{method: "DELETE", token: "t", instanceID: "cli:abc", attemptID: "abc"},
			path: sessionAttemptPath,
			want: map[string]string{headerInstance: "cli:abc"},
		},
		{
			name: "DELETE without an attempt id targets the session route",
			req:  sessionRequest{method: "DELETE", token: "t", instanceID: "inst-1"},
			path: sessionPath,
			want: map[string]string{headerInstance: "inst-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var got http.Header
			_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				got = decodeHeaders(r)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "active"})
			})
			if _, err := client.callSession(context.Background(), tc.req); err != nil {
				t.Fatalf("callSession: %v", err)
			}
			if gotPath != tc.path {
				t.Fatalf("path = %q, want %q", gotPath, tc.path)
			}
			for header, want := range tc.want {
				if got.Get(header) != want {
					t.Errorf("header %s = %q, want %q", header, got.Get(header), want)
				}
			}
			for _, header := range tc.notWant {
				if value := got.Get(header); value != "" {
					t.Errorf("header %s should be absent, got %q", header, value)
				}
			}
		})
	}
}

// TestSessionStatusMappings pins the status-code-to-body mapping, which decides
// whether a refusal is data (recoverable) or an error (stop).
func TestSessionStatusMappings(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		method      string
		body        string
		wantStatus  string
		wantErrCode string
	}{
		{"GET 404 means no session", 404, "GET", "", "none", ""},
		{"403 country_blocked is data", 403, "GET", `{"status":"country_blocked"}`, "country_blocked", ""},
		{"403 banned is data", 403, "GET", `{"status":"banned"}`, "banned", ""},
		{"409 model_locked is data", 409, "POST", `{"status":"model_locked"}`, "model_locked", ""},
		{"429 spend_limited is data", 429, "POST", `{"status":"spend_limited"}`, "spend_limited", ""},
		{"429 rate_limited is data", 429, "POST", `{"status":"rate_limited"}`, "rate_limited", ""},
		{"403 without a known status is an error", 403, "GET", `{"error":"weird"}`, "", "weird"},
		{"409 without a known status is an error", 409, "POST", `{"error":"weird"}`, "", "weird"},
		{"429 without a known status is an error", 429, "POST", `{"error":"weird"}`, "", "weird"},
		{"500 is an error", 500, "GET", `{"error":"boom"}`, "", "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			})
			got, err := client.callSession(context.Background(), sessionRequest{method: tc.method, token: "t"})
			if tc.wantErrCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Status != tc.wantStatus {
					t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got status %q", got.Status)
			}
			var sessionErr *SessionError
			if !asSessionError(err, &sessionErr) {
				t.Fatalf("error type = %T", err)
			}
			if sessionErr.ErrorCode != tc.wantErrCode {
				t.Fatalf("errorCode = %q, want %q", sessionErr.ErrorCode, tc.wantErrCode)
			}
		})
	}
}

// TestAdmission404FailsClosed pins that a server predating the dedicated route
// is never worked around by falling back to the legacy path.
func TestAdmission404FailsClosed(t *testing.T) {
	var paths []string
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := client.callSession(context.Background(), sessionRequest{method: "POST", token: "t", model: "m"})
	if err == nil {
		t.Fatal("expected an unsupported error")
	}
	var unsupported *UnsupportedError
	if !asUnsupported(err, &unsupported) {
		t.Fatalf("error type = %T, want *UnsupportedError", err)
	}
	if unsupported.Message != sessionUnsupportedMessage {
		t.Fatalf("message = %q, want the server's own wording", unsupported.Message)
	}
	if len(paths) != 1 || paths[0] != sessionAdmissionPath {
		t.Fatalf("paths = %v, want a single admission attempt", paths)
	}
}

func TestAdmission405FailsClosed(t *testing.T) {
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	_, err := client.callSession(context.Background(), sessionRequest{method: "POST", token: "t", model: "m"})
	var unsupported *UnsupportedError
	if !asUnsupported(err, &unsupported) {
		t.Fatalf("error = %v (%T), want *UnsupportedError", err, err)
	}
}

// TestPostFailureClassification pins the rule that a POST is retried only when
// the failure provably happened before the session mutation committed.
func TestPostFailureClassification(t *testing.T) {
	cases := []struct {
		status int
		want   failureDisposition
	}{
		{http.StatusRequestTimeout, dispositionRetry},
		{http.StatusTooManyRequests, dispositionRetry},
		{http.StatusServiceUnavailable, dispositionRetry},
		{http.StatusBadRequest, dispositionStop},
		{http.StatusUnauthorized, dispositionStop},
		{http.StatusConflict, dispositionStop},
		{http.StatusNotFound, dispositionStop},
		{http.StatusBadGateway, dispositionUnknown},
		{http.StatusInternalServerError, dispositionUnknown},
	}
	for _, tc := range cases {
		err := &SessionError{Message: "x", StatusCode: tc.status}
		if got := classifySessionFailure("POST", err); got != tc.want {
			t.Errorf("POST %d = %v, want %v", tc.status, got, tc.want)
		}
	}
	// A network failure on POST is unknown: the request may have committed.
	if got := classifySessionFailure("POST", &SessionError{Message: "net"}); got != dispositionUnknown {
		t.Errorf("POST network failure = %v, want unknown", got)
	}
	// GET is read-only, so anything transient is safe to repeat.
	for _, status := range []int{408, 429, 500, 502} {
		if got := classifySessionFailure("GET", &SessionError{StatusCode: status}); got != dispositionRetry {
			t.Errorf("GET %d = %v, want retry", status, got)
		}
	}
	if got := classifySessionFailure("GET", &SessionError{Message: "net"}); got != dispositionRetry {
		t.Errorf("GET network failure = %v, want retry", got)
	}
	if got := classifySessionFailure("GET", &SessionError{StatusCode: 400}); got != dispositionStop {
		t.Errorf("GET 400 = %v, want stop", got)
	}
}

// TestGateCodeRequiresBothHalves pins that a gate rejection is identified by
// the PAIR (code, status). Matching the code with the wrong status is not a gate.
func TestGateCodeRequiresBothHalves(t *testing.T) {
	if _, ok := LookupGate("session_expired", 410); !ok {
		t.Fatal("session_expired/410 must be a gate")
	}
	if _, ok := LookupGate("session_expired", 409); ok {
		t.Fatal("session_expired with the wrong status must not match")
	}
	if _, ok := LookupGate("toString", 0); ok {
		t.Fatal("an inherited name must not match")
	}
	if _, ok := LookupGate("", 410); ok {
		t.Fatal("an empty code must not match")
	}
	if _, ok := LookupGate("not_a_real_code", 500); ok {
		t.Fatal("an unknown code must not match")
	}
	endsSession := map[string]bool{
		"waiting_room_required":  true,
		"session_expired":        true,
		"session_superseded":     true,
		"session_model_mismatch": true,
		"session_limit_reached":  false,
		"waiting_room_queued":    false,
		"model_unavailable":      false,
	}
	for code, want := range endsSession {
		gate, ok := LookupGate(code, gateCodes[code].Status)
		if !ok {
			t.Fatalf("gate %q not found", code)
		}
		if gate.EndsTheSession != want {
			t.Errorf("%s endsTheSession = %v, want %v", code, gate.EndsTheSession, want)
		}
	}
}

// TestMergeCompactSessionKeepsQuota pins that a compact poll cannot blank the
// allowance meter mid-run.
func TestMergeCompactSessionKeepsQuota(t *testing.T) {
	quota := &FreebucksInfo{Balance: 42}
	rates := map[string]RateLimitInfo{"m1": {Model: "m1", Limit: 6, RecentCount: 1}}
	current := SessionResponse{Status: "active", InstanceID: "i1", Model: "m1", Freebucks: quota, RateLimitsByModel: rates}
	next := SessionResponse{Status: "active", InstanceID: "i1", Model: "m1"}
	merged := mergeCompactSession(current, next)
	if merged.Freebucks == nil || merged.Freebucks.Balance != 42 {
		t.Fatal("freebucks balance was dropped by a compact poll")
	}
	if merged.RateLimitsByModel == nil {
		t.Fatal("rate limits were dropped by a compact poll")
	}
	// A different instance must NOT inherit the previous quota snapshot.
	other := SessionResponse{Status: "active", InstanceID: "i2", Model: "m1"}
	if merged := mergeCompactSession(current, other); merged.Freebucks != nil {
		t.Fatal("quota leaked across instances")
	}
	// A different model must not inherit either.
	otherModel := SessionResponse{Status: "active", InstanceID: "i1", Model: "m2"}
	if merged := mergeCompactSession(current, otherModel); merged.Freebucks != nil {
		t.Fatal("quota leaked across models")
	}
}

// TestParseRetryAfter pins both Retry-After forms.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfterMs("5", now); got != 5000 {
		t.Fatalf("seconds form = %d, want 5000", got)
	}
	if got := parseRetryAfterMs("", now); got != 0 {
		t.Fatalf("empty = %d, want 0", got)
	}
	if got := parseRetryAfterMs("garbage", now); got != 0 {
		t.Fatalf("garbage = %d, want 0", got)
	}
	past := now.Add(-time.Hour).Format(http.TimeFormat)
	if got := parseRetryAfterMs(past, now); got != 0 {
		t.Fatalf("past date = %d, want 0", got)
	}
	future := now.Add(30 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfterMs(future, now); got < 29000 || got > 31000 {
		t.Fatalf("future date = %d, want ~30000", got)
	}
}

// TestEnsureProbesBeforeCommitting pins "probe before committing": a live
// session on the same model is adopted rather than rotating the slot.
func TestEnsureProbesBeforeCommitting(t *testing.T) {
	var posts int
	var gets int
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			posts++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": "m1"})
		case r.Method == "GET":
			gets++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": "m1"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	manager := NewSessionManager(client)
	session, err := manager.Ensure(context.Background(), "token", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != "active" {
		t.Fatalf("status = %q", session.Status)
	}
	if gets != 1 {
		t.Fatalf("gets = %d, want a probe first", gets)
	}
	if posts != 0 {
		t.Fatalf("posts = %d, want 0 when a live session is adoptable", posts)
	}
}

// TestEnsureAdmitsWhenNoSessionExists pins the commit path.
func TestEnsureAdmitsWhenNoSessionExists(t *testing.T) {
	var posts int
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": "m1"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	manager := NewSessionManager(client)
	if _, err := manager.Ensure(context.Background(), "token", "m1"); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}
}

// TestEnsureReAdmitsWhenModelChanges pins that a model switch takes a new slot
// rather than silently running the old session's model.
func TestEnsureReAdmitsWhenModelChanges(t *testing.T) {
	var posts int
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": r.Header.Get(headerModel)})
			return
		}
		// The existing session is on a different model.
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": "old-model"})
	})
	manager := NewSessionManager(client)
	session, err := manager.Ensure(context.Background(), "token", "new-model")
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 || session.Model != "new-model" {
		t.Fatalf("posts = %d model = %q, want a re-admission on the new model", posts, session.Model)
	}
}

// TestReleaseFreesTheSlot pins that DELETE is issued so the quota slot returns
// immediately rather than at expiry.
func TestReleaseFreesTheSlot(t *testing.T) {
	var deletes int
	_, client := newSessionServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "none"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "active", "instanceId": "i1", "model": "m1"})
	})
	manager := NewSessionManager(client)
	if _, err := manager.Ensure(context.Background(), "token", "m1"); err != nil {
		t.Fatal(err)
	}
	manager.Release(context.Background(), "token")
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1", deletes)
	}
	if manager.Current().Status != "none" {
		t.Fatalf("status after release = %q", manager.Current().Status)
	}
	// A second release with no live slot must not issue another DELETE.
	manager.Release(context.Background(), "token")
	if deletes != 1 {
		t.Fatalf("deletes = %d, want no repeat", deletes)
	}
}

// TestNextDelayClampsToExpiry pins that the poll lands just after expires_at so
// the countdown does not look stuck, and that terminal states stop the loop.
func TestNextDelayClampsToExpiry(t *testing.T) {
	manager := NewSessionManager(NewClient())
	if got := manager.nextDelay(SessionResponse{Status: "active", RemainingMs: 2000}); got > 5*time.Second {
		t.Fatalf("delay = %s, want a clamp near expiry", got)
	}
	if got := manager.nextDelay(SessionResponse{Status: "active", RemainingMs: 600000}); got < 20*time.Second {
		t.Fatalf("delay = %s, want the normal cadence", got)
	}
	if got := manager.nextDelay(SessionResponse{Status: "ended"}); got != 0 {
		t.Fatalf("ended-without-instance delay = %s, want 0 (stop)", got)
	}
	if got := manager.nextDelay(SessionResponse{Status: "ended", InstanceID: "i1"}); got <= 0 {
		t.Fatalf("grace-window delay = %s, want to keep polling", got)
	}
	if got := manager.nextDelay(SessionResponse{Status: "spend_limited"}); got <= 0 {
		t.Fatal("a terminal status must not keep the loop alive forever")
	}
}

// TestTerminalStatusCoverage pins that no quota refusal keeps the loop polling.
func TestTerminalStatusCoverage(t *testing.T) {
	terminal := []string{"none", "superseded", "country_blocked", "banned", "model_locked",
		"model_unavailable", "rate_limited", "spend_limited", "ip_capped", "consent_required"}
	for _, status := range terminal {
		if !(SessionResponse{Status: status}).TerminalStatus() {
			t.Errorf("%q should be terminal", status)
		}
	}
	for _, status := range []string{"active", "ended", ""} {
		if (SessionResponse{Status: status}).TerminalStatus() {
			t.Errorf("%q should not be terminal", status)
		}
	}
}

// TestQuotaReadout pins the quota extraction used by /freebuff-status.
func TestQuotaReadout(t *testing.T) {
	manager := NewSessionManager(NewClient())
	manager.adopt(SessionResponse{
		Status: "active",
		Model:  "m1",
		RateLimitsByModel: map[string]RateLimitInfo{
			"m1": {Model: "m1", Limit: 6, RecentCount: 1.5, Pool: "limited"},
		},
		Freebucks: &FreebucksInfo{Balance: 20, Daily: &DailyPool{Limit: 25, Remaining: 20, ResetTimeZone: "Africa/Lagos"}},
	})
	hours, ok := manager.QuotaRemainingHours()
	if !ok || hours != 4.5 {
		t.Fatalf("hours=%v ok=%v, want 4.5 remaining of a 6h pool", hours, ok)
	}
	// The single-model form is the fallback when the map is absent.
	manager.adopt(SessionResponse{Status: "active", Model: "m1", RateLimit: &RateLimitInfo{Model: "m1", Limit: 6, RecentCount: 0}})
	if hours, ok := manager.QuotaRemainingHours(); !ok || hours != 6 {
		t.Fatalf("hours=%v ok=%v, want the single-model fallback", hours, ok)
	}
	// A model with no pool is unknown, not zero: zero would read as exhausted.
	manager.adopt(SessionResponse{Status: "active", Model: "m3"})
	if hours, ok := manager.QuotaRemainingHours(); ok {
		t.Fatalf("hours=%v ok=%v, want unknown for a model with no pool", hours, ok)
	}
}

// TestRemainingHoursNeverGoesNegative pins the floor: a server that reports more
// hours consumed than the limit must not surface as negative headroom.
func TestRemainingHoursNeverGoesNegative(t *testing.T) {
	if got := (RateLimitInfo{Limit: 6, RecentCount: 9}).RemainingHours(); got != 0 {
		t.Fatalf("RemainingHours = %v, want 0", got)
	}
}

// TestParsesRealLimitedTierSessionResponse is a regression test built from an
// actual production response for a region-limited account. The earlier struct
// guessed that rateLimitsByModel was an array of meters with remainingHours, so
// every real response failed to decode: the keep-alive loop silently stopped
// updating and the quota readout showed nothing.
func TestParsesRealLimitedTierSessionResponse(t *testing.T) {
	body := `{"status":"none","accessTier":"limited",
	  "freebucks":{"balance":25,"daily":{"limit":25,"spent":5,"remaining":20,
	    "resetAt":"2026-09-25T23:00:00.000Z","resetTimeZone":"Africa/Lagos"},
	    "wallet":{"balance":0,"monthlyBonus":0},"planId":null,
	    "prices":{"deepseek/deepseek-v4-flash":15,"stealth/space-bunny-alpha":0},
	    "priceNotices":{"deepseek/deepseek-v4-flash":"Peak pricing"},
	    "offPeak":{"deepseek/deepseek-v4-flash":{"startHourUtc":22,"endHourUtc":6,"price":10,"regularPrice":15}},
	    "priceChanges":[{"at":"2026-09-25T22:00:00.000Z","modelId":"deepseek/deepseek-v4-flash","price":10,"tagline":"Off-peak"}]},
	  "rateLimit":{"model":"deepseek/deepseek-v4-flash",
	    "entitlementBreakdown":{"base":6,"referral":0,"streak":0},"limit":6,
	    "pool":"limited","poolLabel":"Daily","period":"pacific_day",
	    "resetTimeZone":"America/Los_Angeles","resetAt":"2026-09-26T07:00:00.000Z",
	    "windowHours":24,"recentCount":1},
	  "rateLimitsByModel":{
	    "deepseek/deepseek-v4-flash":{"model":"deepseek/deepseek-v4-flash",
	      "entitlementBreakdown":{"base":6,"referral":0,"streak":0},"limit":6,
	      "pool":"limited","poolLabel":"Daily","period":"pacific_day",
	      "resetTimeZone":"America/Los_Angeles","resetAt":"2026-09-26T07:00:00.000Z",
	      "windowHours":24,"recentCount":1},
	    "mimo/mimo-v2.5":{"model":"mimo/mimo-v2.5","entitlementBreakdown":{"base":6},
	      "limit":6,"pool":"limited","period":"pacific_day","windowHours":24,"recentCount":1}},
	  "countryCode":"US","countryBlockReason":"recent_limited_country",
	  "ipPrivacySignals":["hosting"]}`
	var session SessionResponse
	if err := json.Unmarshal([]byte(body), &session); err != nil {
		t.Fatalf("a real limited-tier response must decode: %v", err)
	}
	if session.AccessTier != "limited" {
		t.Fatalf("accessTier = %q", session.AccessTier)
	}
	if len(session.RateLimitsByModel) != 2 {
		t.Fatalf("rateLimitsByModel has %d entries, want 2", len(session.RateLimitsByModel))
	}
	entry := session.RateLimitsByModel["deepseek/deepseek-v4-flash"]
	if entry.Limit != 6 || entry.RecentCount != 1 || entry.RemainingHours() != 5 {
		t.Fatalf("entry = %+v", entry)
	}
	if session.RateLimit == nil || session.RateLimit.Pool != "limited" {
		t.Fatalf("rateLimit = %+v", session.RateLimit)
	}
	if session.Freebucks == nil || session.Freebucks.Daily == nil || session.Freebucks.Daily.Remaining != 20 {
		t.Fatalf("freebucks = %+v", session.Freebucks)
	}
	if session.Freebucks.Daily.ResetTimeZone != "Africa/Lagos" {
		t.Fatalf("reset timezone = %q", session.Freebucks.Daily.ResetTimeZone)
	}
	if session.Freebucks.Prices["stealth/space-bunny-alpha"] != 0 {
		t.Fatal("expected a zero-priced model in the price map")
	}
	if session.CountryBlockReason != "recent_limited_country" || len(session.IPPrivacySignals) != 1 {
		t.Fatalf("region signals = %+v", session)
	}
}

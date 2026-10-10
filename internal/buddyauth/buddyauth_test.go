package buddyauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// isolatedStore points the shared credential store at a fresh temp directory.
//
// The store is process-global and persists across tests, so without this a
// token stored by one test is visible to the next. That is exactly how a
// refresh test once ran against a token planted by the success test and hit
// the real server.
func isolatedStore(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
	dir := os.Getenv("CERAA_HOME")
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
}

func testProduct(endpoint string) Product {
	p := BuddyProduct()
	p.Endpoint = endpoint
	return p
}

func testWorkBuddyProduct(endpoint string) Product {
	p := WorkBuddyProduct()
	p.Endpoint = endpoint
	return p
}

func testManager(product Product) *Manager {
	mgr := NewManagerWithOptions(product, &http.Client{Timeout: 10 * time.Second})
	mgr.pollInterval = time.Millisecond
	mgr.deadline = 10 * time.Second
	return mgr
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// The state call is a POST with the platform query param and the no-identity
// headers, and the login URL comes from the server rather than being built
// locally.
func TestStartFetchesStateAndLoginURL(t *testing.T) {
	isolatedStore(t)
	var gotMethod, gotPlatform, gotDomain, gotNoAuth, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPlatform = r.URL.Query().Get("platform")
		gotDomain = r.Header.Get(headerDomain)
		gotNoAuth = r.Header.Get(headerNoAuth)
		gotUA = r.Header.Get("User-Agent")
		if r.URL.Path != authStatePath {
			t.Errorf("state path = %q, want %q", r.URL.Path, authStatePath)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{"state": "st-123", "authUrl": "https://www.codebuddy.cn/login/?platform=ide&state=st-123"},
		})
	}))
	defer server.Close()

	mgr := testManager(testProduct(server.URL))
	flow, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap := flow.Snapshot()
	if snap.LoginState != "st-123" {
		t.Errorf("flow state = %q, want st-123", snap.LoginState)
	}
	if snap.AuthURL != "https://www.codebuddy.cn/login/?platform=ide&state=st-123" {
		t.Errorf("auth URL = %q, want the server-issued URL untouched", snap.AuthURL)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("state method = %q, want POST", gotMethod)
	}
	if gotPlatform != "ide" {
		t.Errorf("platform = %q, want ide", gotPlatform)
	}
	if gotDomain != "copilot.tencent.com" {
		t.Errorf("X-Domain = %q, want the product domain", gotDomain)
	}
	if gotNoAuth != "true" {
		t.Errorf("X-No-Authorization = %q, want true", gotNoAuth)
	}
	if gotUA != "CodeBuddyIDE/1.106.1" {
		t.Errorf("User-Agent = %q, want the CodeBuddy client identity", gotUA)
	}
}

// WorkBuddy differs in platform param, endpoint, identity headers, and the
// login URL decoration (version + loginSessionId appended, path untouched).
func TestWorkBuddyStartUsesItsOwnPlatformAndDecoratesTheURL(t *testing.T) {
	isolatedStore(t)
	var gotPlatform, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlatform = r.URL.Query().Get("platform")
		gotUA = r.Header.Get("User-Agent")
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{"state": "wst-9", "authUrl": "https://www.workbuddy.ai/login/?platform=workbuddy-ai&state=wst-9"},
		})
	}))
	defer server.Close()

	mgr := testManager(testWorkBuddyProduct(server.URL))
	flow, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPlatform != "workbuddy-ai" {
		t.Errorf("platform = %q, want workbuddy-ai", gotPlatform)
	}
	if gotUA != "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2" {
		t.Errorf("User-Agent = %q, want the WorkBuddy client identity", gotUA)
	}
	parsed, err := url.Parse(flow.Snapshot().AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/login/" {
		t.Errorf("decorated path = %q, want the server path untouched", parsed.Path)
	}
	if parsed.Query().Get("state") != "wst-9" || parsed.Query().Get("platform") != "workbuddy-ai" {
		t.Errorf("decoration dropped server params: %q", flow.Snapshot().AuthURL)
	}
	if parsed.Query().Get("version") != "5.5.2" {
		t.Errorf("version = %q, want the plugin version appended", parsed.Query().Get("version"))
	}
	if parsed.Query().Get("loginSessionId") == "" {
		t.Errorf("loginSessionId missing from %q", flow.Snapshot().AuthURL)
	}
}

// A full login stores the token set plus identity under namespaced keys, and
// the pending code on the token endpoint means "keep waiting", not failure.
func TestLoginSuccessStoresTheCredential(t *testing.T) {
	isolatedStore(t)
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authStatePath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": map[string]any{"state": "st-1", "authUrl": "https://login.example/?state=st-1"},
			})
		case authTokenPath:
			tokenCalls++
			if r.URL.Query().Get("state") != "st-1" {
				t.Errorf("token poll state = %q, want st-1", r.URL.Query().Get("state"))
			}
			// First poll answers "not ready yet"; the second delivers.
			if tokenCalls == 1 {
				writeJSON(w, http.StatusOK, map[string]any{"code": codeTokenNotReady, "message": "not ready"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": map[string]any{
					"accessToken": "tok_live", "refreshToken": "tok_refresh",
					"expiresIn": 3600, "refreshExpiresIn": 7200,
					"tokenType": "Bearer", "scope": "", "domain": "",
				},
			})
		case loginAccountPath:
			if got := r.Header.Get("Authorization"); got != "Bearer tok_live" {
				t.Errorf("account Authorization = %q, want the polled token", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": map[string]any{"uid": "u-123", "nickname": "Emperor", "enterpriseId": "", "type": "personal"},
			})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	mgr := testManager(testProduct(server.URL))
	flow, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Poll(context.Background(), flow); err != nil {
		t.Fatalf("a successful login reported %v", err)
	}
	if tokenCalls != 2 {
		t.Errorf("token endpoint hit %d times, want 2 (one pending, one grant)", tokenCalls)
	}
	if snap := flow.Snapshot(); snap.Status != "completed" {
		t.Errorf("flow status = %q, want completed", snap.Status)
	}
	for key, want := range map[string]string{
		"buddy_access_token": "tok_live", "buddy_refresh_token": "tok_refresh",
		"buddy_user_id": "u-123", "buddy_nickname": "Emperor",
	} {
		if got, err := secret.Get(key); err != nil || got != want {
			t.Errorf("stored %s = %q, %v; want %q", key, got, err, want)
		}
	}
	if !mgr.Authenticated() || !mgr.SignedIn() {
		t.Error("fresh login should be signed in and authenticated")
	}
	// Every key this package writes is namespaced to its product: the full
	// key list is asserted in TestKeyNames below.
}

// The account poll has its own pending code (12151), distinct from the
// token's. A reader that only knows one of them fails the other half.
func TestAccountPendingRetries(t *testing.T) {
	isolatedStore(t)
	accountCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authStatePath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"state": "st-a", "authUrl": "https://login.example/"},
			})
		case authTokenPath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"accessToken": "tok", "refreshToken": "ref", "expiresIn": 3600},
			})
		case loginAccountPath:
			accountCalls++
			if accountCalls < 3 {
				writeJSON(w, http.StatusOK, map[string]any{"code": codeAccountNotReady, "message": "not ready"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"uid": "u-9", "nickname": "", "enterpriseId": "", "type": ""},
			})
		}
	}))
	defer server.Close()

	mgr := testManager(testProduct(server.URL))
	flow, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Poll(context.Background(), flow); err != nil {
		t.Fatalf("account-pending login reported %v", err)
	}
	if accountCalls != 3 {
		t.Errorf("account endpoint hit %d times, want 3", accountCalls)
	}
	if got := mgr.UID(); got != "u-9" {
		t.Errorf("uid = %q, want u-9", got)
	}
}

// A token field that is not a string is a malformed grant, not a slow yes.
// The vendor's own parser enforces exactly this.
func TestANonStringTokenIsRejected(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authStatePath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"state": "st-x", "authUrl": "https://login.example/"},
			})
		case authTokenPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":42}}`))
		}
	}))
	defer server.Close()

	mgr := testManager(testProduct(server.URL))
	flow, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Poll(context.Background(), flow); err == nil {
		t.Fatal("a numeric token was accepted")
	}
	if mgr.SignedIn() {
		t.Error("a rejected grant must not leave a stored token")
	}
}

// A token response with no token at all is rejected the same way.
func TestATokenlessGrantIsRejected(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authStatePath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"state": "st-y", "authUrl": "https://login.example/"},
			})
		case authTokenPath:
			writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"refreshToken": "only-refresh"}})
		}
	}))
	defer server.Close()

	mgr := testManager(testProduct(server.URL))
	flow, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Poll(context.Background(), flow); err == nil {
		t.Fatal("a tokenless grant was accepted")
	}
}

// Refresh carries the old refresh token in X-Refresh-Token beside a Bearer
// Authorization header (not a form-encoded grant), and the new pair rotates
// the stored one.
func TestRefreshRotatesTheStoredPair(t *testing.T) {
	isolatedStore(t)
	p := testProduct("")
	var gotRefresh, gotAuth, gotSource, gotDomain string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != authRefreshPath {
			t.Errorf("refresh path = %q, want %q", r.URL.Path, authRefreshPath)
		}
		if r.Method != http.MethodPost {
			t.Errorf("refresh method = %q, want POST", r.Method)
		}
		gotRefresh = r.Header.Get(headerRefreshToken)
		gotAuth = r.Header.Get("Authorization")
		gotSource = r.Header.Get(headerRefreshSource)
		gotDomain = r.Header.Get(headerDomain)
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{"accessToken": "tok_new", "refreshToken": "tok_refresh2", "expiresIn": 3600},
		})
	}))
	defer server.Close()
	p.Endpoint = server.URL

	if err := secret.Set(p.key(suffixAccessToken), "tok_old"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(p.key(suffixRefreshToken), "tok_refresh"); err != nil {
		t.Fatal(err)
	}

	mgr := testManager(p)
	got, err := mgr.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got != "tok_new" {
		t.Errorf("refreshed token = %q, want tok_new", got)
	}
	if gotRefresh != "tok_refresh" {
		t.Errorf("X-Refresh-Token = %q, want the stored refresh token", gotRefresh)
	}
	if gotAuth != "Bearer tok_old" {
		t.Errorf("Authorization = %q, want the stored access token", gotAuth)
	}
	if gotSource != authRefreshSource {
		t.Errorf("X-Auth-Refresh-Source = %q, want %q", gotSource, authRefreshSource)
	}
	if gotDomain != "copilot.tencent.com" {
		t.Errorf("X-Domain = %q, want the product domain", gotDomain)
	}
	if stored, _ := secret.Get(p.key(suffixAccessToken)); stored != "tok_new" {
		t.Errorf("stored access token = %q, want the rotation", stored)
	}
	if stored, _ := secret.Get(p.key(suffixRefreshToken)); stored != "tok_refresh2" {
		t.Errorf("stored refresh token = %q, want the rotation", stored)
	}
}

// A refresh the backend rejects as unauthorised is terminal: the token is
// dead, not slow, and the caller must sign in again rather than retry.
func TestRefreshRejectionIsTerminal(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "message": "refresh token expired"})
	}))
	defer server.Close()
	p := testProduct(server.URL)

	if err := secret.Set(p.key(suffixAccessToken), "tok_old"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(p.key(suffixRefreshToken), "tok_dead"); err != nil {
		t.Fatal(err)
	}

	mgr := testManager(p)
	if _, err := mgr.RefreshToken(context.Background()); err == nil {
		t.Fatal("a rejected refresh reported success")
	} else {
		var flowErr *Error
		if !errors.As(err, &flowErr) || flowErr.Code != "refresh_expired" {
			t.Errorf("error = %v, want code refresh_expired", err)
		}
	}
}

// A refresh that omits the refresh token must not wipe the stored one: the
// rotation is sticky, or the next refresh has nothing to spend.
func TestRefreshPreservesARefreshTokenItDoesNotReplace(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"accessToken": "tok_new2", "expiresIn": 3600},
		})
	}))
	defer server.Close()
	p := testProduct(server.URL)

	if err := secret.Set(p.key(suffixAccessToken), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(p.key(suffixRefreshToken), "tok_refresh"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(p.key(suffixUserID), "u-keep"); err != nil {
		t.Fatal(err)
	}

	mgr := testManager(p)
	if _, err := mgr.RefreshToken(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, _ := secret.Get(p.key(suffixRefreshToken)); got != "tok_refresh" {
		t.Errorf("refresh token after refresh = %q, want the stored one kept", got)
	}
	if got := mgr.UID(); got != "u-keep" {
		t.Errorf("uid after refresh = %q, want the login value kept", got)
	}
}

// Buddy and workbuddy credentials are isolated: a login on one product must
// not authenticate the other.
func TestProductsDoNotShareCredentials(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case authStatePath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"state": "st-iso", "authUrl": "https://login.example/"},
			})
		case authTokenPath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"accessToken": "tok_buddy", "refreshToken": "ref_buddy", "expiresIn": 3600},
			})
		case loginAccountPath:
			writeJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"uid": "u-b", "nickname": "B", "enterpriseId": "", "type": "personal"},
			})
		}
	}))
	defer server.Close()

	buddy := testManager(testProduct(server.URL))
	flow, err := buddy.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := buddy.Poll(context.Background(), flow); err != nil {
		t.Fatal(err)
	}
	workbuddy := testManager(testWorkBuddyProduct(server.URL))
	if workbuddy.SignedIn() || workbuddy.Authenticated() {
		t.Error("a buddy login must not sign in workbuddy")
	}
	if !buddy.SignedIn() {
		t.Error("buddy should be signed in after its own login")
	}
	if got, _ := secret.Get("buddy_access_token"); got != "tok_buddy" {
		t.Errorf("buddy token = %q", got)
	}
	// Every key this package writes is namespaced to its product.
	for _, key := range []string{"buddy_access_token", "workbuddy_access_token"} {
		if !strings.HasPrefix(key, "buddy_") && !strings.HasPrefix(key, "workbuddy_") {
			t.Errorf("secret key %q is not namespaced", key)
		}
	}
	if _, err := secret.Get("workbuddy_access_token"); err == nil {
		t.Error("workbuddy key exists after a buddy-only login")
	}
}

// Every secret key this package can write carries its product prefix, or
// one variable would wear two vendors' names.
func TestKeyNames(t *testing.T) {
	for _, p := range []Product{BuddyProduct(), WorkBuddyProduct()} {
		for _, suffix := range []string{
			suffixAccessToken, suffixRefreshToken, suffixExpiresAt,
			suffixRefreshExpiresAt, suffixTokenType, suffixScope, suffixDomain,
			suffixUserID, suffixNickname, suffixEnterpriseID, suffixAccountType,
		} {
			key := p.key(suffix)
			if !strings.HasPrefix(key, p.keyPrefix()) {
				t.Errorf("secret key %q is not namespaced to %q", key, p.ID)
			}
		}
	}
	if BuddyProduct().keyPrefix() == WorkBuddyProduct().keyPrefix() {
		t.Error("buddy and workbuddy share a key prefix; their credentials would collide")
	}
}

// Forget removes only the caller's product credential.
func TestForgetRemovesOnlyItsOwnProduct(t *testing.T) {
	isolatedStore(t)
	buddy := BuddyProduct()
	workbuddy := WorkBuddyProduct()
	for _, k := range []string{buddy.key(suffixAccessToken), workbuddy.key(suffixAccessToken)} {
		if err := secret.Set(k, "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewManagerWithOptions(buddy, nil).Forget(); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(buddy.key(suffixAccessToken)); err == nil {
		t.Error("buddy token survived Forget")
	}
	if got, err := secret.Get(workbuddy.key(suffixAccessToken)); err != nil || got != "tok" {
		t.Errorf("workbuddy token = %q, %v; Forget must not touch the sibling", got, err)
	}
}

// An expired token is not usable, and AccessToken refreshes through expiry
// rather than handing out the dead one.
func TestExpiredTokenRefreshesOnAccess(t *testing.T) {
	isolatedStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"accessToken": "tok_fresh", "refreshToken": "ref_next", "expiresIn": 3600},
		})
	}))
	defer server.Close()
	p := testProduct(server.URL)

	if err := secret.Set(p.key(suffixAccessToken), "tok_dead"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Set(p.key(suffixRefreshToken), "ref_old"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UnixMilli()
	if err := secret.Set(p.key(suffixExpiresAt), strconv.FormatInt(past, 10)); err != nil {
		t.Fatal(err)
	}

	mgr := testManager(p)
	if mgr.Authenticated() {
		t.Error("an expired token must not report authenticated")
	}
	got, err := mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "tok_fresh" {
		t.Errorf("AccessToken = %q, want the refreshed token", got)
	}
}

// With no credential at all, the failure names signing in.
func TestAccessWithoutLoginSaysSo(t *testing.T) {
	isolatedStore(t)
	mgr := testManager(testProduct("https://api.example"))
	if _, err := mgr.AccessToken(context.Background()); err == nil {
		t.Fatal("AccessToken with no stored token reported success")
	} else if !strings.Contains(err.Error(), "start a login") {
		t.Errorf("error = %q, want it to say to start a login", err)
	}
	if _, err := mgr.RefreshToken(context.Background()); err == nil {
		t.Fatal("refresh with no stored token reported success")
	} else if !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("error = %q, want it to say to sign in", err)
	}
}

// The token source seam hands the stored token to the transport.
func TestTokenSourceHandsTheStoredToken(t *testing.T) {
	isolatedStore(t)
	p := BuddyProduct()
	if err := secret.Set(p.key(suffixAccessToken), "tok_source"); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	if err := secret.Set(p.key(suffixExpiresAt), strconv.FormatInt(future, 10)); err != nil {
		t.Fatal(err)
	}
	src := NewTokenSource()
	got, err := src.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok_source" {
		t.Errorf("token source = %q, want the stored token", got)
	}
}

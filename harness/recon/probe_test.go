package recon

// probe_test.go exercises the probe engine over httptest with a plain
// client: the tests set probeClient so nothing here dials through netguard,
// and receiptsBase so nothing here writes into the operator's data dir.
// Budget and scope are driven through recon's public API (SetActive/Stop)
// only; the Engagement semantics themselves belong to the scope file's own
// tests, not this one.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// usePlainProbe installs a plain client plus a temp receipt root and restores
// both when the test ends.
func usePlainProbe(t *testing.T) {
	t.Helper()
	oldClient, oldBase := probeClient, receiptsBase
	t.Cleanup(func() {
		probeClient, receiptsBase = oldClient, oldBase
	})
	probeClient = &http.Client{}
	receiptsBase = t.TempDir()
}

// probeTestServer starts a counting test server. The count reflects what
// actually arrived, which is how tests prove a request was never dialed.
func probeTestServer(t *testing.T, handler http.HandlerFunc) (srv *httptest.Server, hits func() int) {
	t.Helper()
	var count atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s, func() int { return int(count.Load()) }
}

func probeHost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func TestProbeCaptureFillsReceiptWithMatchingHash(t *testing.T) {
	usePlainProbe(t)
	srv, _ := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Server", "conehead/1.0")
		w.Header().Set("X_Correlator", "noise")                // underscore-prefixed: must be dropped
		w.Header().Set("Access-Control-Allow-X", "irrelevant") // not in the subset
		w.Header().Add("Set-Cookie", "session=SECRET-VALUE; Path=/")
		fmt.Fprintf(w, "hello ceraa\n")
	})

	res, err := Probe(context.Background(), Request{URL: srv.URL, Method: "GET"})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if res.Status != http.StatusOK || res.Method != http.MethodGet {
		t.Fatalf("status = %d method = %s", res.Status, res.Method)
	}
	if res.BodyHead != "hello ceraa\n" {
		t.Errorf("BodyHead = %q", res.BodyHead)
	}
	if res.BodyBytes != len("hello ceraa\n") {
		t.Errorf("BodyBytes = %d", res.BodyBytes)
	}
	sum := sha256.Sum256([]byte("hello ceraa\n"))
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %s, want %s", res.SHA256, hex.EncodeToString(sum[:]))
	}
	if res.RedirectedTo != "" || res.ElapsedMS < 0 {
		t.Errorf("redirect = %q elapsed = %d", res.RedirectedTo, res.ElapsedMS)
	}
	if res.Headers["content-type"] != "text/plain" || res.Headers["server"] != "conehead/1.0" {
		t.Errorf("subset headers = %v", res.Headers)
	}
	if res.Headers["set-cookie"] != setCookiePresence {
		t.Errorf("set-cookie = %q, want redacted presence", res.Headers["set-cookie"])
	}
	if _, ok := res.Headers["access-control-allow-x"]; ok {
		t.Error("off-set header leaked into the subset")
	}
	if res.ReceiptID == "" {
		t.Fatal("receipt was not recorded")
	}
	if !strings.HasSuffix(res.ReceiptID, "-"+res.SHA256[:12]+".json") {
		t.Errorf("receipt name %q not <timestamp>-<hash>.json", res.ReceiptID)
	}
	path := filepath.Join(receiptsBase, "recon", "receipts", res.ReceiptID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("receipt missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("receipt mode = %o, want 600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("receipt is not parsable: %v\n%s", err, raw)
	}
	if doc["sha256"] != res.SHA256 {
		t.Errorf("receipt sha256 = %v", doc["sha256"])
	}
	if doc["body_head"] != "hello ceraa\n" {
		t.Errorf("receipt body_head = %v", doc["body_head"])
	}
	if doc["method"] != "GET" || doc["status"] != float64(200) {
		t.Errorf("receipt method/status = %v/%v", doc["method"], doc["status"])
	}
}

func TestProbeLeavesRedirectUnfollowedAndNamesIt(t *testing.T) {
	usePlainProbe(t)
	srvLoc := ""
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvLoc+"/next", http.StatusFound)
	})
	srvLoc = srv.URL

	res, err := Probe(context.Background(), Request{URL: srv.URL + "/from"})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if res.Status != http.StatusFound {
		t.Errorf("status = %d, want the 3xx itself", res.Status)
	}
	if res.RedirectedTo != srvLoc+"/next" {
		t.Errorf("RedirectedTo = %q, want the Location value", res.RedirectedTo)
	}
	if n := hits(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 (redirect not chased)", n)
	}
	if res.ReceiptID == "" {
		t.Error("redirect observation was not receipted")
	}
}

func TestProbeTruncatesBodyAtTheCapAndHashesTheBoundedSlice(t *testing.T) {
	usePlainProbe(t)
	over := make([]byte, (512*1024)+777)
	for i := range over {
		over[i] = byte('a' + i%26)
	}
	srv, _ := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(over)
	})

	res, err := Probe(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if res.BodyBytes != 512*1024 {
		t.Errorf("BodyBytes = %d, want the 512KB cap", res.BodyBytes)
	}
	sum := sha256.Sum256(over[:512*1024])
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Error("hash is not over the truncated slice")
	}
	if runes := []rune(res.BodyHead); len(runes) != bodyHeadRunes {
		t.Errorf("BodyHead holds %d runes, want %d", len(runes), bodyHeadRunes)
	}
}

func TestRunSpecPathListStopsAtTheRateBudget(t *testing.T) {
	usePlainProbe(t)
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "saw %s", r.URL.Path)
	})
	SetActive(Engagement{RPM: 2, InScope: []string{probeHost(t, srv)}})
	defer Stop()

	out, err := RunSpec(context.Background(), map[string]any{
		"url":   srv.URL,
		"mode":  "path_list",
		"paths": []string{"/a", "/b", "/c"},
	})
	if err != nil {
		t.Fatalf("RunSpec failed: %v", err)
	}
	if n := hits(); n != 2 {
		t.Errorf("server saw %d requests, want 2 (third never dialed)", n)
	}
	if !strings.Contains(out, "engagement rate limit reached") {
		t.Errorf("output does not name the budget stop:\n%s", out)
	}
	if strings.Count(out, "status 200") != 2 {
		t.Errorf("output should carry exactly two result blocks:\n%s", out)
	}
	if strings.Contains(out, "/c") {
		t.Errorf("the undialed path leaked into the output:\n%s", out)
	}
}

func TestRunSpecGateRefusesEachPathWithoutDialing(t *testing.T) {
	usePlainProbe(t)
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a gated-out request was dialed")
	})
	SetActive(Engagement{RPM: 100, InScope: []string{"allowed.example.com"}})
	defer Stop()

	out, err := RunSpec(context.Background(), map[string]any{
		"url":   srv.URL + "/base",
		"mode":  "path_list",
		"paths": []string{"/a", "/b"},
	})
	if err != nil {
		t.Fatalf("RunSpec failed: %v", err)
	}
	if n := hits(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
	if got := strings.Count(out, "refused, not dialed"); got != 2 {
		t.Errorf("expected one refusal block per path, got %d:\n%s", got, out)
	}
	// The refusal text itself is Gate's own contract and its own tests; here
	// it only has to be a perrequest refusal that never dialed.
	if !strings.Contains(out, "refused, not dialed") {
		t.Errorf("refusal does not name its cause:\n%s", out)
	}
	if strings.Contains(out, "engagement rate limit reached") {
		t.Error("refusals must not burn or report budget")
	}
}

func TestRunSpecOriginVariantsFlagsCORSReflection(t *testing.T) {
	usePlainProbe(t)
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			t.Errorf("request missing Origin header")
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		fmt.Fprint(w, "reflection target")
	})
	SetActive(Engagement{RPM: 100, InScope: []string{probeHost(t, srv)}})
	defer Stop()

	out, err := RunSpec(context.Background(), map[string]any{
		"url":     srv.URL,
		"mode":    "origin_variants",
		"origins": []string{"https://app.example.com"},
	})
	if err != nil {
		t.Fatalf("RunSpec failed: %v", err)
	}
	if n := hits(); n != 2 {
		t.Errorf("server saw %d requests, want 2 (one origin + the null probe)", n)
	}
	if strings.Count(out, "REFLECTS with allow-credentials") != 1 {
		t.Errorf("the echoed origin should be flagged REFLECTS:\n%s", out)
	}
	if !strings.Contains(out, "allow-credentials true (actionable)") {
		t.Errorf("the actionable credential pair is not named:\n%s", out)
	}
	if !strings.Contains(out, "null origin reflected") {
		t.Errorf("the null-origin probe result is not noted:\n%s", out)
	}
}

func TestProbeRefusesBadSchemesMethodsHeaderCRLFAndOversizedBody(t *testing.T) {
	usePlainProbe(t)

	if _, err := Probe(context.Background(), Request{URL: "ftp://example.com"}); err == nil {
		t.Error("non-HTTP scheme accepted")
	}
	if _, err := Probe(context.Background(), Request{URL: "notaurl"}); err == nil {
		t.Error("schemeless url accepted")
	}
	if _, err := Probe(context.Background(), Request{URL: "http://example.com", Method: "trace"}); err == nil {
		t.Error("TRACE accepted")
	}
	if _, err := Probe(context.Background(), Request{URL: "http://example.com", Headers: map[string]string{"Bad Name": "v"}}); err == nil {
		t.Error("space-bearing header name accepted")
	}
	if _, err := Probe(context.Background(), Request{URL: "http://example.com", Headers: map[string]string{"X-Smug": "v\r\nHost: evil"}}); err == nil {
		t.Error("CRLF in a header value accepted")
	}
	if _, err := Probe(context.Background(), Request{URL: "http://example.com", Body: strings.Repeat("x", 64*1024+1)}); err == nil {
		t.Error("oversized body accepted")
	}
}

func TestRunSpecPostSendsJSONObjectBody(t *testing.T) {
	usePlainProbe(t)
	var gotMethod, gotType, gotBody string
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		fmt.Fprint(w, "accepted")
	})
	SetActive(Engagement{RPM: 10, InScope: []string{probeHost(t, srv)}})
	defer Stop()

	out, err := RunSpec(context.Background(), map[string]any{
		"url":    srv.URL,
		"method": "post",
		"body":   map[string]any{"event": "charge.success", "amount": 100},
	})
	if err != nil {
		t.Fatalf("RunSpec failed: %v", err)
	}
	if n := hits(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("server saw method %q, want POST", gotMethod)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, gotBody)
	}
	if payload["event"] != "charge.success" {
		t.Errorf("payload = %v", payload)
	}
	if !strings.Contains(out, "POST "+srv.URL) {
		t.Errorf("summary does not name the POST:\n%s", out)
	}
}

func TestRunSpecExplicitContentTypeWins(t *testing.T) {
	usePlainProbe(t)
	var gotType string
	srv, _ := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		fmt.Fprint(w, "ok")
	})
	SetActive(Engagement{RPM: 10, InScope: []string{probeHost(t, srv)}})
	defer Stop()

	if _, err := RunSpec(context.Background(), map[string]any{
		"url":     srv.URL,
		"method":  "POST",
		"headers": map[string]any{"content-type": "application/x-custom"},
		"body":    map[string]any{"a": 1},
	}); err != nil {
		t.Fatalf("RunSpec failed: %v", err)
	}
	if gotType != "application/x-custom" {
		t.Errorf("Content-Type = %q, want the explicit value", gotType)
	}
}

func TestRunSpecRefusesDestructiveMethodWithoutAuthorization(t *testing.T) {
	usePlainProbe(t)
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a destructive request was dialed without authorization")
	})
	SetActive(Engagement{RPM: 10, InScope: []string{probeHost(t, srv)}})
	defer Stop()

	_, err := RunSpec(context.Background(), map[string]any{"url": srv.URL, "method": "DELETE"})
	if err == nil || !strings.Contains(err.Error(), "AllowDestructive") {
		t.Fatalf("DELETE without authorization: error = %v", err)
	}
	if n := hits(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

func TestRunSpecAllowsDestructiveMethodWhenAuthorized(t *testing.T) {
	usePlainProbe(t)
	var gotMethod string
	srv, hits := probeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		fmt.Fprint(w, "deleted")
	})
	SetActive(Engagement{RPM: 10, InScope: []string{probeHost(t, srv)}, AllowDestructive: true})
	defer Stop()

	if _, err := RunSpec(context.Background(), map[string]any{"url": srv.URL, "method": "DELETE"}); err != nil {
		t.Fatalf("RunSpec failed: %v", err)
	}
	if n := hits(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("server saw method %q, want DELETE", gotMethod)
	}
}

func TestRunSpecRefusesBodyOnGet(t *testing.T) {
	_, err := RunSpec(context.Background(), map[string]any{"url": "https://example.com", "body": "x"})
	if err == nil || !strings.Contains(err.Error(), "requires POST") {
		t.Fatalf("GET with a body: error = %v", err)
	}
}

func TestRunSpecRefusesPostOriginVariants(t *testing.T) {
	_, err := RunSpec(context.Background(), map[string]any{
		"url": "https://example.com", "mode": "origin_variants", "method": "POST",
	})
	if err == nil || !strings.Contains(err.Error(), "GET-only") {
		t.Fatalf("POST origin_variants: error = %v", err)
	}
}

func TestHeaderArgsCoercesScalars(t *testing.T) {
	headers, err := headerArgs(map[string]any{"headers": map[string]any{
		"X-Count": float64(1), "X-Flag": true, "X-Name": "v",
	}})
	if err != nil {
		t.Fatalf("headerArgs failed: %v", err)
	}
	if headers["X-Count"] != "1" || headers["X-Flag"] != "true" || headers["X-Name"] != "v" {
		t.Errorf("headers = %v", headers)
	}
	if _, err := headerArgs(map[string]any{"headers": map[string]any{"X-Bad": map[string]any{"a": 1}}}); err == nil {
		t.Error("nested header value accepted")
	}
}

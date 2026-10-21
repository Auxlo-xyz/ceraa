package scan

// discover_test.go exercises the content discovery sweep over httptest
// servers. The request seam is injected with a plain client, because the
// egress wall refuses the loopback address the test server listens on and
// that refusal is the behaviour the production path wants.
//
// The helpers here are shared by the package's tests: the plain probe, the
// scope the loopback needs, and the wordlist file.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/recon"
)

// useLoopbackScope installs an engagement that authorizes the loopback
// address the test server listens on.
//
// The scope gate refuses the host's own machine with no engagement on
// record, and that refusal is the behaviour under test elsewhere. A test
// that wants requests to leave the test server has to say so the way an
// operator would.
func useLoopbackScope(t *testing.T) {
	t.Helper()
	useScopeFor(t, "127.0.0.1")
}

// useScopeFor installs an engagement that authorizes one host.
func useScopeFor(t *testing.T, host string) {
	t.Helper()
	// The scope record is written under the data directory; a temp one
	// keeps the suite out of the operator's files.
	t.Setenv("CERAA_HOME", t.TempDir())
	if err := recon.SetActive(recon.Engagement{InScope: []string{host}}); err != nil {
		t.Fatalf("set engagement: %v", err)
	}
	t.Cleanup(recon.Stop)
}

// plainProbe performs a real HTTP request with an unguarded client. It is
// the injected request seam: the sweep logic runs against a live server,
// and nothing here touches the network beyond the test process.
func plainProbe(ctx context.Context, req recon.Request) (recon.Result, error) {
	result, _, err := plainRequest(ctx, req, false)
	return result, err
}

// plainCapture is plainProbe with the body handed back, for the marker
// search.
func plainCapture(ctx context.Context, req recon.Request, bodyCap int) (recon.Result, []byte, error) {
	return plainRequest(ctx, req, true)
}

func plainRequest(ctx context.Context, req recon.Request, keepBody bool) (recon.Result, []byte, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var payload io.Reader
	if req.Body != "" {
		payload = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, payload)
	if err != nil {
		return recon.Result{}, nil, err
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return recon.Result{}, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return recon.Result{}, nil, err
	}
	sum := sha256.Sum256(body)
	result := recon.Result{
		URL:       req.URL,
		Method:    method,
		Status:    resp.StatusCode,
		Headers:   map[string]string{"content-type": resp.Header.Get("Content-Type")},
		BodyBytes: len(body),
		SHA256:    hex.EncodeToString(sum[:]),
		ElapsedMS: 1,
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		result.RedirectedTo = resp.Header.Get("Location")
		result.Headers["location"] = result.RedirectedTo
	}
	if keepBody {
		return result, body, nil
	}
	return result, nil, nil
}

// countingServer starts a test server that records the paths it saw.
func countingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

// writeWordlist writes entries into a temp file and returns its path.
func writeWordlist(t *testing.T, entries ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wordlist.txt")
	content := strings.Join(entries, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDiscoverRefusesUnsafePaths: a path is relative. A scheme inside it is
// a second request hiding in a list item, whitespace is a request line
// break, and a traversal sequence walks out of the base the gate authorized.
func TestDiscoverRefusesUnsafePaths(t *testing.T) {
	useLoopbackScope(t)
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a refused path was dialed")
	})

	for _, path := range []string{
		"http://example.com/x",
		"https://example.com/x",
		"/admin panel",
		"../etc/passwd",
		"admin/../../etc/passwd",
		"..",
		"",
	} {
		_, err := Discover(context.Background(), DiscoverOptions{
			BaseURL: srv.URL,
			Paths:   []string{path},
			Probe:   plainProbe,
		})
		if err == nil {
			t.Errorf("path %q was accepted", path)
			continue
		}
		if path != "" && !strings.Contains(err.Error(), path) {
			t.Errorf("error for path %q does not name the path: %v", path, err)
		}
	}
	if len(seen()) != 0 {
		t.Errorf("the server saw %d requests, want 0", len(seen()))
	}
}

// TestDiscoverFindsPathsAndOrdersThem: the hits come back ordered by path so
// a report reads the same twice.
func TestDiscoverFindsPathsAndOrdersThem(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>admin</html>")
		case "/backup.zip":
			w.Header().Set("Content-Type", "application/zip")
			fmt.Fprint(w, "PK-not-really")
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "nope")
		}
	})

	wordlist := writeWordlist(t, "backup.zip", "admin", "missing")
	hits, err := Discover(context.Background(), DiscoverOptions{
		BaseURL:  srv.URL,
		Wordlist: wordlist,
		Probe:    plainProbe,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3: %+v", len(hits), hits)
	}
	if hits[0].Path != "/admin" || hits[1].Path != "/backup.zip" || hits[2].Path != "/missing" {
		t.Errorf("hits are not ordered by path: %+v", hits)
	}
	if hits[0].Status != 200 || hits[0].ContentType != "text/html" {
		t.Errorf("admin hit = %+v", hits[0])
	}
	if hits[0].Size != len("<html>admin</html>") {
		t.Errorf("admin size = %d", hits[0].Size)
	}
	if hits[0].SHA256 == "" {
		t.Error("the hit carries no digest")
	}
}

// TestDiscoverExpandsExtensions: each word is tried bare and with every
// suffix.
func TestDiscoverExpandsExtensions(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "body")
	})

	wordlist := writeWordlist(t, "config")
	hits, err := Discover(context.Background(), DiscoverOptions{
		BaseURL:    srv.URL,
		Wordlist:   wordlist,
		Extensions: []string{".php", ".bak"},
		Probe:      plainProbe,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3 (bare plus two suffixes): %+v", len(hits), hits)
	}
	paths := []string{hits[0].Path, hits[1].Path, hits[2].Path}
	want := map[string]bool{"/config": true, "/config.php": true, "/config.bak": true}
	for _, path := range paths {
		if !want[path] {
			t.Errorf("unexpected path %q in %v", path, paths)
		}
	}
}

// TestDiscoverKeepsOnlyTheMatchingStatus: the status filter drops what the
// caller did not ask to keep.
func TestDiscoverKeepsOnlyTheMatchingStatus(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "ok")
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "no")
		}
	})

	wordlist := writeWordlist(t, "ok", "forbidden")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:     srv.URL,
		Wordlist:    wordlist,
		MatchStatus: []int{200},
		Probe:       plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Path != "/ok" {
		t.Fatalf("hits = %+v, want only /ok", result.Hits)
	}
	if result.ExcludedStatus != 1 {
		t.Errorf("ExcludedStatus = %d, want 1", result.ExcludedStatus)
	}
}

// TestDiscoverDropsTheExplicitSize: the explicit soft-404 rule.
func TestDiscoverDropsTheExplicitSize(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/real" {
			fmt.Fprint(w, "a real page")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "not found")
	})

	wordlist := writeWordlist(t, "real", "missing")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:     srv.URL,
		Wordlist:    wordlist,
		ExcludeSize: len("not found"),
		Probe:       plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Path != "/real" {
		t.Fatalf("hits = %+v, want only /real", result.Hits)
	}
	if result.ExcludedSize != 1 {
		t.Errorf("ExcludedSize = %d, want 1", result.ExcludedSize)
	}
}

// TestDiscoverDetectsTheSoft404Size: a server that answers every miss with
// the same page makes every path look like a hit. A size that repeats across
// three or more paths is the not-found size, and the summary says so.
func TestDiscoverDetectsTheSoft404Size(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/real" {
			fmt.Fprint(w, "a real page with other content")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "not found")
	})

	wordlist := writeWordlist(t, "real", "missing1", "missing2", "missing3")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:  srv.URL,
		Wordlist: wordlist,
		Probe:    plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Path != "/real" {
		t.Fatalf("hits = %+v, want only /real", result.Hits)
	}
	if result.ExcludedSoft404 != 3 {
		t.Errorf("ExcludedSoft404 = %d, want 3", result.ExcludedSoft404)
	}
	if len(result.Soft404Sizes) != 1 || result.Soft404Sizes[0] != len("not found") {
		t.Errorf("Soft404Sizes = %v, want [%d]", result.Soft404Sizes, len("not found"))
	}
}

// TestDiscoverKeepsTheSoft404SizeWhenDetectionIsOff: the rule is on by
// default and can be turned off explicitly.
func TestDiscoverKeepsTheSoft404SizeWhenDetectionIsOff(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "not found")
	})

	off := false
	wordlist := writeWordlist(t, "missing1", "missing2", "missing3")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:       srv.URL,
		Wordlist:      wordlist,
		DetectSoft404: &off,
		Probe:         plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if len(result.Hits) != 3 {
		t.Fatalf("hits = %+v, want the three 404s kept", result.Hits)
	}
	if result.ExcludedSoft404 != 0 {
		t.Errorf("ExcludedSoft404 = %d, want 0", result.ExcludedSoft404)
	}
}

// TestDiscoverStopsAtTheRequestCeiling: the ceiling is on requests fired,
// and it is enforced before the request goes out.
func TestDiscoverStopsAtTheRequestCeiling(t *testing.T) {
	useLoopbackScope(t)
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "body")
	})

	wordlist := writeWordlist(t, "a", "b", "c", "d", "e", "f", "g", "h")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:     srv.URL,
		Wordlist:    wordlist,
		MaxRequests: 3,
		Probe:       plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if n := len(seen()); n != 3 {
		t.Errorf("the server saw %d requests, want 3 (the ceiling)", n)
	}
	if result.Requests != 3 {
		t.Errorf("Requests = %d, want 3", result.Requests)
	}
	if !result.Truncated || result.Stopped == "" {
		t.Errorf("the sweep did not report the ceiling: %+v", result)
	}
}

// TestDiscoverRecursesIntoADirectory: a path that answers with a redirect
// to itself plus a slash is a directory, and the sweep looks inside it.
func TestDiscoverRecursesIntoADirectory(t *testing.T) {
	useLoopbackScope(t)
	var base string
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin":
			http.Redirect(w, r, base+"/admin/", http.StatusMovedPermanently)
		case "/admin/index.php":
			fmt.Fprint(w, "the index page")
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "not found")
		}
	})
	base = srv.URL

	wordlist := writeWordlist(t, "admin")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:  srv.URL,
		Wordlist: wordlist,
		Depth:    1,
		Probe:    plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	found := false
	for _, path := range seen() {
		if path == "/admin/index.php" {
			found = true
		}
	}
	if !found {
		t.Errorf("the sweep did not look inside the directory: %v", seen())
	}
	paths := []string{}
	for _, hit := range result.Hits {
		paths = append(paths, hit.Path)
	}
	if !containsString(paths, "/admin/index.php") {
		t.Errorf("the directory child is not among the hits: %v", paths)
	}
}

// TestDiscoverRefusesToFireWhenScopeRefuses: the gate is per request, and a
// refused request is never dialed.
func TestDiscoverRefusesToFireWhenScopeRefuses(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request was dialed although the scope refused it")
	})

	wordlist := writeWordlist(t, "admin")
	result, err := DiscoverDetailed(context.Background(), DiscoverOptions{
		BaseURL:  srv.URL,
		Wordlist: wordlist,
		Probe:    plainProbe,
	})
	if err != nil {
		t.Fatalf("DiscoverDetailed: %v", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("the server saw %d requests, want 0", n)
	}
	if result.Refused == 0 {
		t.Error("the refusal was not counted")
	}
	if len(result.Hits) != 0 {
		t.Errorf("hits = %+v, want none", result.Hits)
	}
}

// containsString reports whether a list holds a value.
func containsString(list []string, want string) bool {
	for _, entry := range list {
		if entry == want {
			return true
		}
	}
	return false
}

// TestCollapseCatchAllFoldsTheDominantResponse covers the single page
// application case: every path answers with the same document, so a sweep
// would otherwise report a hundred hits that are one page.
func TestCollapseCatchAllFoldsTheDominantResponse(t *testing.T) {
	hits := make([]Hit, 0, 60)
	for i := 0; i < 60; i++ {
		hits = append(hits, Hit{Path: fmt.Sprintf("/path-%d", i), Status: 200, Size: 11910})
	}
	hits = append(hits, Hit{Path: "/admin", Status: 403, Size: 42})

	kept, collapsed := collapseCatchAll(hits, 61)
	if collapsed == nil {
		t.Fatal("the dominant response was not collapsed")
	}
	if collapsed.Paths != 60 || collapsed.Status != 200 || collapsed.Size != 11910 {
		t.Errorf("collapsed = %+v", collapsed)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d hits, want 2", len(kept))
	}
	found := false
	for _, hit := range kept {
		if hit.Path == "/admin" {
			found = true
		}
	}
	if !found {
		t.Errorf("the odd path out was lost: %v", kept)
	}
}

// TestCollapseCatchAllKeepsAMinorityResponse is the other side: a handful of
// same-sized pages is not a catch-all, and folding them would hide pages.
func TestCollapseCatchAllKeepsAMinorityResponse(t *testing.T) {
	hits := make([]Hit, 0, 40)
	for i := 0; i < 40; i++ {
		hits = append(hits, Hit{Path: fmt.Sprintf("/page-%d", i), Status: 200, Size: 500 + i})
	}
	for i := 0; i < 3; i++ {
		hits = append(hits, Hit{Path: fmt.Sprintf("/same-%d", i), Status: 200, Size: 100})
	}
	kept, collapsed := collapseCatchAll(hits, 43)
	if collapsed != nil {
		t.Fatalf("a minority group was collapsed: %+v", collapsed)
	}
	if len(kept) != len(hits) {
		t.Errorf("kept %d hits, want %d", len(kept), len(hits))
	}
}

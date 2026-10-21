package scan

// fuzz_test.go exercises the parameter fuzzer over httptest servers: the
// request it builds, the marker it looks for, and the baseline it measures
// against when there is no marker.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/recon"
)

// TestFuzzReplacesTheExistingQueryParameter: the value goes into the query
// string, replacing the parameter of the same name and leaving the rest of
// the query alone.
func TestFuzzReplacesTheExistingQueryParameter(t *testing.T) {
	useLoopbackScope(t)
	var mu sync.Mutex
	var queries []string
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		fmt.Fprint(w, "the same page for every value")
	})

	wordlist := writeWordlist(t, "admin", "guest")
	hits, err := Fuzz(context.Background(), FuzzOptions{
		URL:      srv.URL + "/search?id=old&keep=1",
		Param:    "id",
		Wordlist: wordlist,
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("Fuzz: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), queries...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("the server saw %d requests, want 3 (baseline plus two values): %v", len(got), got)
	}
	for _, query := range got {
		if !strings.Contains(query, "keep=1") {
			t.Errorf("query %q lost the untouched parameter", query)
		}
		if strings.Contains(query, "id=old") {
			t.Errorf("query %q kept the old value", query)
		}
	}
	// The baseline is the implausible value, the two fuzzed values are the
	// wordlist.
	if !strings.Contains(got[0], "zzq7x") {
		t.Errorf("the first request was not the baseline: %q", got[0])
	}
	if len(hits) != 0 {
		t.Errorf("hits = %+v, want none: every value answered identically", hits)
	}
}

// TestFuzzBodyReplacesThePlaceholder: the body template carries the value.
func TestFuzzBodyReplacesThePlaceholder(t *testing.T) {
	useLoopbackScope(t)
	var mu sync.Mutex
	var bodies []string
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		fmt.Fprint(w, "accepted")
	})

	wordlist := writeWordlist(t, "admin")
	_, err := Fuzz(context.Background(), FuzzOptions{
		URL:      srv.URL + "/login",
		Method:   http.MethodPost,
		Param:    "user",
		Location: "body",
		Body:     "user={PARAM}&remember=1",
		Wordlist: wordlist,
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("Fuzz: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the server saw %d requests, want 2 (baseline plus one value): %v", len(got), got)
	}
	if !strings.Contains(got[1], "user=admin&remember=1") {
		t.Errorf("the placeholder was not replaced: %q", got[1])
	}
}

// TestFuzzBodyAddsTheParameterWhenTheTemplateHasNoPlaceholder: the
// parameter is added as a form field.
func TestFuzzBodyAddsTheParameterWhenTheTemplateHasNoPlaceholder(t *testing.T) {
	useLoopbackScope(t)
	var mu sync.Mutex
	var bodies []string
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		fmt.Fprint(w, "accepted")
	})

	wordlist := writeWordlist(t, "admin")
	_, err := Fuzz(context.Background(), FuzzOptions{
		URL:      srv.URL + "/login",
		Method:   http.MethodPost,
		Param:    "user",
		Location: "body",
		Body:     "remember=1",
		Wordlist: wordlist,
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("Fuzz: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("the server saw %d requests, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[1], "remember=1") || !strings.Contains(got[1], "user=admin") {
		t.Errorf("the parameter was not added as a form field: %q", got[1])
	}
}

// TestFuzzFindsTheMarker: a marker in the body is a confirmed hit.
func TestFuzzFindsTheMarker(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "admin" {
			fmt.Fprint(w, "ERROR: SQLSTATE[42S02] table missing")
			return
		}
		fmt.Fprint(w, "no problem here")
	})

	wordlist := writeWordlist(t, "admin", "guest")
	hits, err := Fuzz(context.Background(), FuzzOptions{
		URL:      srv.URL + "/item",
		Param:    "id",
		Wordlist: wordlist,
		Marker:   "sqlstate",
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("Fuzz: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly one marker hit", hits)
	}
	if hits[0].Value != "admin" || !hits[0].Marker {
		t.Errorf("hit = %+v, want the admin value with the marker", hits[0])
	}
	if hits[0].Delta {
		t.Error("a marker hit must not also be reported as a delta")
	}
}

// TestFuzzReportsDeltasAgainstTheBaseline: with no marker the run measures
// against a baseline and reports a difference as a delta, not as a
// confirmed finding.
func TestFuzzReportsDeltasAgainstTheBaseline(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "admin" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "forbidden")
			return
		}
		fmt.Fprint(w, "the same page for every value")
	})

	wordlist := writeWordlist(t, "admin", "guest")
	result, err := FuzzDetailed(context.Background(), FuzzOptions{
		URL:      srv.URL + "/item",
		Param:    "id",
		Wordlist: wordlist,
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("FuzzDetailed: %v", err)
	}
	if !result.HasBaseline {
		t.Fatal("no baseline was reported")
	}
	if result.BaselineStatus != 200 {
		t.Errorf("baseline status = %d, want 200", result.BaselineStatus)
	}
	if len(result.Hits) != 1 {
		t.Fatalf("hits = %+v, want one delta", result.Hits)
	}
	hit := result.Hits[0]
	if hit.Value != "admin" || !hit.Delta || hit.Marker {
		t.Errorf("hit = %+v, want the admin value as a delta and not as a marker hit", hit)
	}
}

// TestFuzzRefusesWhatItCannotDo: the verb set, the location and the
// parameter name are checked before anything is sent.
func TestFuzzRefusesWhatItCannotDo(t *testing.T) {
	useLoopbackScope(t)
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a refused fuzz request was dialed")
	})

	cases := []FuzzOptions{
		{URL: srv.URL, Param: "id", Method: http.MethodDelete},
		{URL: srv.URL, Param: "id", Location: "header"},
		{URL: srv.URL, Method: http.MethodGet},
		{URL: srv.URL, Param: "id id"},
		{URL: srv.URL, Param: "id", Method: http.MethodGet, Body: "id={PARAM}"},
		{URL: "notaurl", Param: "id"},
		{URL: srv.URL, Param: "id", Method: http.MethodGet, Location: "body"},
	}
	for _, opts := range cases {
		if _, err := Fuzz(context.Background(), opts); err == nil {
			t.Errorf("Fuzz(%+v) was accepted", opts)
		}
	}
	if len(seen()) != 0 {
		t.Errorf("the server saw %d requests, want 0", len(seen()))
	}
}

// TestFuzzStopsAtTheRequestCeiling: the ceiling is on requests fired.
func TestFuzzStopsAtTheRequestCeiling(t *testing.T) {
	useLoopbackScope(t)
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "the same page for every value")
	})

	wordlist := writeWordlist(t, "a", "b", "c", "d", "e")
	result, err := FuzzDetailed(context.Background(), FuzzOptions{
		URL:         srv.URL + "/item",
		Param:       "id",
		Wordlist:    wordlist,
		MaxRequests: 2,
		Probe:       plainCapture,
	})
	if err != nil {
		t.Fatalf("FuzzDetailed: %v", err)
	}
	if n := len(seen()); n != 2 {
		t.Errorf("the server saw %d requests, want 2 (the ceiling)", n)
	}
	if result.Requests != 2 {
		t.Errorf("Requests = %d, want 2", result.Requests)
	}
	if result.Stopped == "" {
		t.Error("the run did not report the ceiling")
	}
}

// TestFuzzUsesTheCaptureSeamForTheMarker: the body the marker is searched
// in is the one the capture handed back, and it is bounded.
func TestFuzzUsesTheCaptureSeamForTheMarker(t *testing.T) {
	useLoopbackScope(t)
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "a body that says MARKERHERE somewhere in it")
	})

	wordlist := writeWordlist(t, "one")
	hits, err := Fuzz(context.Background(), FuzzOptions{
		URL:      srv.URL + "/item",
		Param:    "id",
		Wordlist: wordlist,
		Marker:   "markerhere",
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("Fuzz: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the one marker hit", hits)
	}
	if hits[0].ReceiptID != "" && hits[0].SHA256 == "" {
		t.Error("the hit carries a receipt id but no digest")
	}
}

// TestFuzzRefusesToFireWhenScopeRefuses: the gate is per request.
func TestFuzzRefusesToFireWhenScopeRefuses(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	recon.Stop()
	srv, seen := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request was dialed although the scope refused it")
	})

	wordlist := writeWordlist(t, "admin")
	_, err := Fuzz(context.Background(), FuzzOptions{
		URL:      srv.URL + "/item",
		Param:    "id",
		Wordlist: wordlist,
		Marker:   "marker",
		Probe:    plainCapture,
	})
	if err != nil {
		t.Fatalf("Fuzz: %v", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("the server saw %d requests, want 0", n)
	}
}

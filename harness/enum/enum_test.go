package enum

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ceraa/harness/recon"
)

// fakeResponse is one canned answer.
type fakeResponse struct {
	status int
	body   string
	header http.Header
}

// fakeTransport answers from a table instead of the network.
//
// The keys are matched in order against the request URL, so a test can say
// "crt.sh answers 502 and Cert Spotter answers the names" without a server
// and without a name that resolves.
type fakeTransport struct {
	keys     []string
	answers  map[string]fakeResponse
	requests []string
}

// RoundTrip answers from the table.
func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req.URL.String())
	for _, key := range f.keys {
		if !strings.Contains(req.URL.String(), key) {
			continue
		}
		answer := f.answers[key]
		header := answer.header
		if header == nil {
			header = http.Header{}
		}
		status := answer.status
		if status == 0 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(answer.body)),
			Request:    req,
		}, nil
	}
	return nil, fmt.Errorf("fake transport: no answer for %s", req.URL.String())
}

// hermetic points the data directory at a temporary one.
//
// The scope gate reads the engagement record from disk, and a record left on
// the machine would refuse the fake URLs this suite asks about. The suite
// must not depend on what is on the box it runs on.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
}

// TestTheGateRefusesBeforeAnythingIsDialed: a destination outside the
// engagement is refused, and the refusal means nothing was sent. A gate that
// runs after the dial is a note, not a gate.
func TestTheGateRefusesBeforeAnythingIsDialed(t *testing.T) {
	hermetic(t)
	if err := recon.SetActive(recon.Engagement{Target: "https://example.com"}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer recon.Stop()

	transport := &fakeTransport{keys: []string{"crt.sh"}, answers: map[string]fakeResponse{
		"crt.sh": {status: http.StatusOK, body: "[]"},
	}}
	opts := Options{Transport: transport}

	_, _, err := fetch(context.Background(), opts, "https://crt.sh/?q=example.com", sourceCRTSH, 0)
	if err == nil {
		t.Fatal("the fetch was allowed for a host outside the engagement")
	}
	if len(transport.requests) != 0 {
		t.Errorf("%d requests were sent after the refusal: %v", len(transport.requests), transport.requests)
	}
}

// TestTheBudgetIsChargedPerRequest: the engagement's request cap stops the
// second request, and the refusal names the limit rather than truncating
// silently.
func TestTheBudgetIsChargedPerRequest(t *testing.T) {
	hermetic(t)
	if err := recon.SetActive(recon.Engagement{Target: "https://example.com", RequestCap: 1}); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	defer recon.Stop()

	transport := &fakeTransport{keys: []string{"example.com"}, answers: map[string]fakeResponse{
		"example.com": {status: http.StatusOK, body: "[]"},
	}}
	opts := Options{Transport: transport}

	if _, _, err := fetch(context.Background(), opts, "https://example.com/logs", sourceCRTSH, 0); err != nil {
		t.Fatalf("the first request was refused: %v", err)
	}
	_, _, err := fetch(context.Background(), opts, "https://example.com/logs", sourceCRTSH, 0)
	if err == nil {
		t.Fatal("the second request was allowed past the request cap")
	}
	if len(transport.requests) != 1 {
		t.Errorf("%d requests were sent, want 1", len(transport.requests))
	}
}

// TestANonSuccessStatusNamesTheStatusAndTheSource: a source that answers
// with an error is reported as such. A 502 that is reported as an empty
// answer is a lie about the network.
func TestANonSuccessStatusNamesTheStatusAndTheSource(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{keys: []string{"crt.sh"}, answers: map[string]fakeResponse{
		"crt.sh": {status: http.StatusBadGateway, body: "upstream error"},
	}}
	opts := Options{Transport: transport}

	_, _, err := fetch(context.Background(), opts, "https://crt.sh/?q=example.com", sourceCRTSH, 0)
	if err == nil {
		t.Fatal("a 502 was accepted as an answer")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), sourceCRTSH) {
		t.Errorf("the error does not name the status and the source: %v", err)
	}
}

// TestTheBodyCapRefusesAnOversizedAnswer: a body over the cap is refused
// rather than cut. Half a JSON document handed to a parser produces an error
// that says nothing about the real cause.
func TestTheBodyCapRefusesAnOversizedAnswer(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{keys: []string{"crt.sh"}, answers: map[string]fakeResponse{
		"crt.sh": {status: http.StatusOK, body: strings.Repeat("a", maxBodyBytes+1)},
	}}
	opts := Options{Transport: transport}

	_, _, err := fetch(context.Background(), opts, "https://crt.sh/?q=example.com", sourceCRTSH, 0)
	if err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("the error does not name the cap: %v", err)
	}
}

// TestARedirectHopIsGatedAndCharged: the hop is a request, so it is charged
// as one and the request list shows both.
func TestARedirectHopIsGatedAndCharged(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{
		keys: []string{"rdap.org", "rdap.arin.net"},
		answers: map[string]fakeResponse{
			"rdap.org": {
				status: http.StatusFound,
				header: http.Header{"Location": []string{"https://rdap.arin.net/registry/ip/8.8.8.8"}},
			},
			"rdap.arin.net": {status: http.StatusOK, body: `{"name":"GOOGLE"}`},
		},
	}
	opts := Options{Transport: transport}

	status, body, err := fetch(context.Background(), opts, "https://rdap.org/ip/8.8.8.8", sourceRDAP, maxRedirects)
	if err != nil {
		t.Fatalf("the redirect was not followed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(string(body), "GOOGLE") {
		t.Errorf("the body is not the redirect target's: %s", body)
	}
	if len(transport.requests) != 2 {
		t.Errorf("%d requests were sent, want 2: %v", len(transport.requests), transport.requests)
	}
}

// TestTheOptionsCarryTheirDefaults: the zero value is the production one.
func TestTheOptionsCarryTheirDefaults(t *testing.T) {
	var opts Options
	if opts.timeout() != defaultTimeout {
		t.Errorf("timeout = %s, want %s", opts.timeout(), defaultTimeout)
	}
	if opts.backoff() != defaultRetryBackoff {
		t.Errorf("backoff = %s, want %s", opts.backoff(), defaultRetryBackoff)
	}
	if opts.resolver() == nil {
		t.Error("the resolver is nil")
	}
	if opts.transport() == nil {
		t.Error("the transport is nil")
	}
	custom := Options{Timeout: time.Second, RetryBackoff: time.Millisecond}
	if custom.timeout() != time.Second || custom.backoff() != time.Millisecond {
		t.Errorf("the set values were not honoured: %s %s", custom.timeout(), custom.backoff())
	}
}

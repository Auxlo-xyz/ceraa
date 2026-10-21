package enum

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// crtBody is a crt.sh answer in the shape it answers: an array of
// certificates, each with a subject common name and a newline separated list
// of names.
const crtBody = `[
  {"common_name":"*.example.com","name_value":"*.example.com\nexample.com\nwww.example.com"},
  {"common_name":"mail.example.com","name_value":"mail.example.com\nshop.example.com"},
  {"common_name":"other.test","name_value":"other.test\ncdn.other.test"}
]`

// certSpotterBody is a Cert Spotter answer: the names live in dns_names.
const certSpotterBody = `[
  {"id":"1","dns_names":["example.com","www.example.com","*.api.example.com"],"not_before":"2024-01-01T00:00:00Z"}
]`

// TestTheDomainIsNormalised: the same domain is written many ways, and the
// certificate query must be the same query every time.
func TestTheDomainIsNormalised(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"example.com", "example.com"},
		{"  example.com  ", "example.com"},
		{"EXAMPLE.COM", "example.com"},
		{"example.com.", "example.com"},
		{"https://example.com", "example.com"},
		{"http://example.com/", "example.com"},
		{"https://example.com/admin/panel?x=1", "example.com"},
		{"https://user:pass@example.com/x", "example.com"},
		{"example.com:8443", "example.com"},
		{"https://example.com:8443/", "example.com"},
	}
	for _, tc := range cases {
		got, err := normaliseDomain(tc.in)
		if err != nil {
			t.Errorf("normaliseDomain(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normaliseDomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAnEmptyOrSpacedDomainIsRefused: there is no query to run without a
// domain, and a name with a space in it is a sentence, not a domain.
func TestAnEmptyOrSpacedDomainIsRefused(t *testing.T) {
	for _, in := range []string{"", "   ", "example .com", "two domains", "https://example.com/a b"} {
		if got, err := normaliseDomain(in); err == nil {
			t.Errorf("normaliseDomain(%q) = %q, want a refusal", in, got)
		}
	}
}

// TestTheCertificateNamesAreCleaned: wildcards come off, names outside the
// domain go away, the list is sorted and the apex is kept only when the logs
// carry it as a name of its own.
func TestTheCertificateNamesAreCleaned(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{keys: []string{"crt.sh"}, answers: map[string]fakeResponse{
		"crt.sh": {status: http.StatusOK, body: crtBody},
	}}
	opts := Options{Transport: transport, RetryBackoff: time.Millisecond}

	result, err := CollectSubdomains(context.Background(), opts, "https://Example.com/")
	if err != nil {
		t.Fatalf("CollectSubdomains: %v", err)
	}
	want := []string{"example.com", "mail.example.com", "shop.example.com", "www.example.com"}
	if strings.Join(result.Names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", result.Names, want)
	}
	if result.Source != sourceCRTSH {
		t.Errorf("source = %q, want %q", result.Source, sourceCRTSH)
	}
	if result.Truncated {
		t.Error("four names were reported as truncated")
	}
}

// TestTheCertSpotterShapeIsRead: the fallback answers in another shape, and
// a parser written for one shape only would answer with an empty list.
func TestTheCertSpotterShapeIsRead(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{keys: []string{"certspotter"}, answers: map[string]fakeResponse{
		"certspotter": {status: http.StatusOK, body: certSpotterBody},
	}}
	opts := Options{Transport: transport, RetryBackoff: time.Millisecond}

	result, err := CollectSubdomains(context.Background(), opts, "example.com")
	if err != nil {
		t.Fatalf("CollectSubdomains: %v", err)
	}
	want := []string{"api.example.com", "example.com", "www.example.com"}
	if strings.Join(result.Names, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", result.Names, want)
	}
	if result.Source != sourceCertSpotter {
		t.Errorf("source = %q, want %q", result.Source, sourceCertSpotter)
	}
}

// TestTheFallbackTakesOverWhenThePrimaryFails: crt.sh serves 502 often
// enough that a lookup that trusts it alone fails half the time.
func TestTheFallbackTakesOverWhenThePrimaryFails(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{
		keys: []string{"crt.sh", "certspotter"},
		answers: map[string]fakeResponse{
			"crt.sh":      {status: http.StatusBadGateway, body: "upstream error"},
			"certspotter": {status: http.StatusOK, body: certSpotterBody},
		},
	}
	opts := Options{Transport: transport, RetryBackoff: time.Millisecond}

	result, err := CollectSubdomains(context.Background(), opts, "example.com")
	if err != nil {
		t.Fatalf("CollectSubdomains: %v", err)
	}
	if result.Source != sourceCertSpotter {
		t.Errorf("source = %q, want %q", result.Source, sourceCertSpotter)
	}
	if len(result.Names) != 3 {
		t.Errorf("names = %v, want three", result.Names)
	}
	// Two attempts at crt.sh and one at the fallback.
	if len(transport.requests) != 3 {
		t.Errorf("%d requests were sent, want 3: %v", len(transport.requests), transport.requests)
	}
}

// TestBothSourcesFailingNamesBoth: a caller that sees only the last error
// cannot tell a dead source from a dead network.
func TestBothSourcesFailingNamesBoth(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{
		keys: []string{"crt.sh", "certspotter"},
		answers: map[string]fakeResponse{
			"crt.sh":      {status: http.StatusBadGateway},
			"certspotter": {status: http.StatusServiceUnavailable},
		},
	}
	opts := Options{Transport: transport, RetryBackoff: time.Millisecond}

	_, err := CollectSubdomains(context.Background(), opts, "example.com")
	if err == nil {
		t.Fatal("two dead sources were reported as an answer")
	}
	if !strings.Contains(err.Error(), sourceCRTSH) || !strings.Contains(err.Error(), sourceCertSpotter) {
		t.Errorf("the error does not name both sources: %v", err)
	}
}

// TestTheNameListIsCappedAndSaysSo: a domain with a thousand certificates is
// real, and a list of a thousand lines is not an answer.
func TestTheNameListIsCappedAndSaysSo(t *testing.T) {
	hermetic(t)
	var body strings.Builder
	body.WriteString("[")
	for i := 0; i < maxSubdomains+100; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		name := "host" + strconv.Itoa(i) + ".example.com"
		body.WriteString(`{"common_name":"` + name + `","name_value":"` + name + `"}`)
	}
	body.WriteString("]")

	transport := &fakeTransport{keys: []string{"crt.sh"}, answers: map[string]fakeResponse{
		"crt.sh": {status: http.StatusOK, body: body.String()},
	}}
	opts := Options{Transport: transport, RetryBackoff: time.Millisecond}

	result, err := CollectSubdomains(context.Background(), opts, "example.com")
	if err != nil {
		t.Fatalf("CollectSubdomains: %v", err)
	}
	if len(result.Names) != maxSubdomains {
		t.Errorf("%d names were returned, want the %d cap", len(result.Names), maxSubdomains)
	}
	if !result.Truncated {
		t.Error("the cut list was not reported as truncated")
	}
	if len(result.Notes) == 0 {
		t.Error("the cut list carried no note")
	}
}

// TestTheQueriesAreTheDocumentedOnes: the sources are addressed the way
// their APIs are documented, and a query that changes shape answers about
// something else.
func TestTheQueriesAreTheDocumentedOnes(t *testing.T) {
	crt := crtURL("example.com")
	if !strings.Contains(crt, "q=%25.example.com") || !strings.Contains(crt, "output=json") {
		t.Errorf("the crt.sh query is not the documented one: %s", crt)
	}
	spotter := certSpotterURL("example.com")
	for _, want := range []string{"domain=example.com", "include_subdomains=true", "expand=dns_names"} {
		if !strings.Contains(spotter, want) {
			t.Errorf("the Cert Spotter query is missing %q: %s", want, spotter)
		}
	}
}

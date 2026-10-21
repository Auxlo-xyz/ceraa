package enum

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// rdapBody is an RDAP registration answer in the shape the registries write:
// the block, the organisation, the country and the entity that holds the
// allocation.
const rdapBody = `{
  "handle": "NET-8-0-0-0-1",
  "startAddress": "8.8.8.0",
  "endAddress": "8.8.8.255",
  "ipVersion": "v4",
  "name": "GOOGLE",
  "type": "DIRECT ALLOCATION",
  "country": "US",
  "cidr0_cidrs": [{"v4prefix": "8.8.8.0", "length": 24}],
  "entities": [
    {
      "objectClassName": "entity",
      "handle": "GOGL",
      "roles": ["registrant"],
      "vcardArray": ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "Google LLC"]]]
    }
  ]
}`

// ripeBody is the routing registry's answer: the announced prefix and the AS
// that announces it.
const ripeBody = `{
  "status": "ok",
  "status_code": 200,
  "data": {
    "resource": "8.8.8.0/24",
    "resource_type": "prefix",
    "announced": true,
    "prefix": "8.8.8.0/24",
    "asns": ["AS15169"],
    "asn": 15169,
    "holder": "GOOGLE"
  }
}`

// TestTheRegistrationIsParsed: the block, the organisation, the country and
// the registrant all come out of the answer.
func TestTheRegistrationIsParsed(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]fakeResponse{
			"rdap.org":      {status: http.StatusOK, body: rdapBody},
			"stat.ripe.net": {status: http.StatusOK, body: ripeBody},
		},
	}
	opts := Options{Transport: transport}

	info, err := CollectHost(context.Background(), opts, "8.8.8.8")
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if info.IP != "8.8.8.8" {
		t.Errorf("ip = %q, want 8.8.8.8", info.IP)
	}
	if info.Prefix != "8.8.8.0/24" {
		t.Errorf("prefix = %q, want 8.8.8.0/24", info.Prefix)
	}
	if info.PrefixSource != sourceRDAP {
		t.Errorf("prefix source = %q, want %q", info.PrefixSource, sourceRDAP)
	}
	if info.ASName != "GOOGLE" {
		t.Errorf("as name = %q, want GOOGLE", info.ASName)
	}
	if info.ASN != "AS15169" {
		t.Errorf("asn = %q, want AS15169", info.ASN)
	}
	if info.ASNSource != sourceRIPE {
		t.Errorf("asn source = %q, want %q", info.ASNSource, sourceRIPE)
	}
	if info.Country != "US" {
		t.Errorf("country = %q, want US", info.Country)
	}
	if info.Registrant != "Google LLC" {
		t.Errorf("registrant = %q, want Google LLC", info.Registrant)
	}
	if info.Source != sourceRDAP+"+"+sourceRIPE {
		t.Errorf("source = %q, want %q", info.Source, sourceRDAP+"+"+sourceRIPE)
	}
}

// TestTheRoutingRegistryAnswersWhenTheRegistrationDoesNot: the fallback is
// the whole reason the answer can name an AS number at all.
func TestTheRoutingRegistryAnswersWhenTheRegistrationDoesNot(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]fakeResponse{
			"rdap.org":      {status: http.StatusBadGateway, body: "upstream error"},
			"stat.ripe.net": {status: http.StatusOK, body: ripeBody},
		},
	}
	opts := Options{Transport: transport}

	info, err := CollectHost(context.Background(), opts, "8.8.8.8")
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if info.Source != sourceRIPE {
		t.Errorf("source = %q, want %q", info.Source, sourceRIPE)
	}
	if info.Prefix != "8.8.8.0/24" {
		t.Errorf("prefix = %q, want 8.8.8.0/24", info.Prefix)
	}
	if info.ASN != "AS15169" {
		t.Errorf("asn = %q, want AS15169", info.ASN)
	}
	// The registration did not answer, so the fields only it carries stay
	// empty rather than being guessed at.
	if info.Country != "" {
		t.Errorf("country = %q, want it empty", info.Country)
	}
	if info.Registrant != "" {
		t.Errorf("registrant = %q, want it empty", info.Registrant)
	}
}

// TestBothSourcesFailingIsAnError: an answer nobody gave is not an answer.
func TestBothSourcesFailingIsAnError(t *testing.T) {
	hermetic(t)
	transport := &fakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]fakeResponse{
			"rdap.org":      {status: http.StatusBadGateway},
			"stat.ripe.net": {status: http.StatusBadGateway},
		},
	}
	opts := Options{Transport: transport}

	if _, err := CollectHost(context.Background(), opts, "8.8.8.8"); err == nil {
		t.Fatal("two dead sources were reported as an answer")
	}
}

// TestACountryObjectIsRead: registries write the country either as a code or
// as an object carrying the name.
func TestACountryObjectIsRead(t *testing.T) {
	hermetic(t)
	body := strings.Replace(rdapBody, `"country": "US"`, `"country": {"name": "United States of America", "iso2": "US"}`, 1)
	transport := &fakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]fakeResponse{
			"rdap.org":      {status: http.StatusOK, body: body},
			"stat.ripe.net": {status: http.StatusOK, body: ripeBody},
		},
	}
	opts := Options{Transport: transport}

	info, err := CollectHost(context.Background(), opts, "8.8.8.8")
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if info.Country != "United States of America" {
		t.Errorf("country = %q, want United States of America", info.Country)
	}
}

// TestTheRegistrantFallsBackToTheHandle: a handle is still an answer when
// the readable name is missing.
func TestTheRegistrantFallsBackToTheHandle(t *testing.T) {
	hermetic(t)
	body := strings.Replace(rdapBody,
		`"vcardArray": ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "Google LLC"]]]`,
		`"vcardArray": ["vcard", [["version", {}, "text", "4.0"]]]`, 1)
	transport := &fakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]fakeResponse{
			"rdap.org":      {status: http.StatusOK, body: body},
			"stat.ripe.net": {status: http.StatusOK, body: ripeBody},
		},
	}
	opts := Options{Transport: transport}

	info, err := CollectHost(context.Background(), opts, "8.8.8.8")
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if info.Registrant != "GOGL" {
		t.Errorf("registrant = %q, want GOGL", info.Registrant)
	}
}

// TestAHostnameIsResolvedFirst: the registration is about an address, so the
// name has to become one first.
func TestAHostnameIsResolvedFirst(t *testing.T) {
	hermetic(t)
	resolver := &fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	transport := &fakeTransport{
		keys: []string{"rdap.org", "stat.ripe.net"},
		answers: map[string]fakeResponse{
			"rdap.org":      {status: http.StatusOK, body: rdapBody},
			"stat.ripe.net": {status: http.StatusOK, body: ripeBody},
		},
	}
	opts := Options{Transport: transport, Resolver: resolver}

	info, err := CollectHost(context.Background(), opts, "example.com")
	if err != nil {
		t.Fatalf("CollectHost: %v", err)
	}
	if info.IP != "93.184.216.34" {
		t.Errorf("ip = %q, want 93.184.216.34", info.IP)
	}
	if len(resolver.lookups) != 1 {
		t.Errorf("%d lookups were made, want 1: %v", len(resolver.lookups), resolver.lookups)
	}
}

// TestAnEmptyHostIsRefused: there is nothing to look up.
func TestAnEmptyHostIsRefused(t *testing.T) {
	hermetic(t)
	opts := Options{Transport: &fakeTransport{}}
	if _, err := CollectHost(context.Background(), opts, "   "); err == nil {
		t.Fatal("an empty host was accepted")
	}
}

// TestTheBlockIsReadFromTheRangeWhenThePrefixListIsMissing: the registry
// states a range, and the block that covers it is the smallest one.
func TestTheBlockIsReadFromTheRangeWhenThePrefixListIsMissing(t *testing.T) {
	cases := []struct {
		start string
		end   string
		want  string
	}{
		{"8.8.8.0", "8.8.8.255", "8.8.8.0/24"},
		{"1.0.0.0", "1.0.0.255", "1.0.0.0/24"},
		{"192.168.0.0", "192.168.255.255", "192.168.0.0/16"},
		{"2001:db8::", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", "2001:db8::/32"},
	}
	for _, tc := range cases {
		got := prefixFromRange(tc.start, tc.end)
		if got != tc.want {
			t.Errorf("prefixFromRange(%q, %q) = %q, want %q", tc.start, tc.end, got, tc.want)
		}
	}
	if got := prefixFromRange("nonsense", "8.8.8.255"); got != "" {
		t.Errorf("prefixFromRange with a broken start = %q, want it empty", got)
	}
}

package enum

import (
	"context"
	"net"
	"strings"
	"testing"
)

// fakeResolver answers DNS queries from a table.
//
// It records every lookup, so a test can prove a CNAME chain was not chased:
// a chase is a second lookup, and the caller did not ask for one.
type fakeResolver struct {
	addresses []net.IPAddr
	cname     string
	cnameErr  error
	mx        []*net.MX
	ns        []*net.NS
	txt       []string
	err       error
	lookups   []string
}

func (f *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	f.lookups = append(f.lookups, "ip")
	if f.err != nil {
		return nil, f.err
	}
	return f.addresses, nil
}

func (f *fakeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	f.lookups = append(f.lookups, "cname")
	if f.cnameErr != nil {
		return "", f.cnameErr
	}
	return f.cname, nil
}

func (f *fakeResolver) LookupMX(ctx context.Context, host string) ([]*net.MX, error) {
	f.lookups = append(f.lookups, "mx")
	if f.err != nil {
		return nil, f.err
	}
	return f.mx, nil
}

func (f *fakeResolver) LookupNS(ctx context.Context, host string) ([]*net.NS, error) {
	f.lookups = append(f.lookups, "ns")
	if f.err != nil {
		return nil, f.err
	}
	return f.ns, nil
}

func (f *fakeResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	f.lookups = append(f.lookups, "txt")
	if f.err != nil {
		return nil, f.err
	}
	return f.txt, nil
}

// TestTheDefaultTypesAreAskedFor: a caller that does not say what it wants
// gets the useful set, and no more.
func TestTheDefaultTypesAreAskedFor(t *testing.T) {
	hermetic(t)
	resolver := &fakeResolver{
		addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}},
	}
	opts := Options{Resolver: resolver}

	result, err := CollectDNS(context.Background(), opts, "example.com", nil)
	if err != nil {
		t.Fatalf("CollectDNS: %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Type != "A" {
		t.Fatalf("records = %v, want one A record", result.Records)
	}
	if len(resolver.lookups) != len(defaultDNSTypes) {
		t.Errorf("%d lookups were made, want %d: %v", len(resolver.lookups), len(defaultDNSTypes), resolver.lookups)
	}
}

// TestTheRecordsAreSortedByTypeAndName: the answer reads the same way every
// time it is asked for.
func TestTheRecordsAreSortedByTypeAndName(t *testing.T) {
	hermetic(t)
	resolver := &fakeResolver{
		addresses: []net.IPAddr{
			{IP: net.ParseIP("2606:2800:220:1:248:1893:25c8:1946")},
			{IP: net.ParseIP("93.184.216.34")},
		},
		mx:  []*net.MX{{Host: "mail.example.com.", Pref: 10}},
		ns:  []*net.NS{{Host: "ns1.example.com."}},
		txt: []string{"v=spf1 -all"},
	}
	opts := Options{Resolver: resolver}

	result, err := CollectDNS(context.Background(), opts, "example.com", []string{"TXT", "NS", "MX", "A", "AAAA"})
	if err != nil {
		t.Fatalf("CollectDNS: %v", err)
	}
	want := []string{"A 93.184.216.34", "AAAA 2606:2800:220:1:248:1893:25c8:1946", "MX 10 mail.example.com", "NS ns1.example.com", "TXT v=spf1 -all"}
	got := make([]string, 0, len(result.Records))
	for _, record := range result.Records {
		got = append(got, record.Type+" "+record.Value)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("records = %v, want %v", got, want)
	}
}

// TestATXTValueIsCutAndSaysSo: a TXT record is a place people put things
// that are kilobytes long.
func TestATXTValueIsCutAndSaysSo(t *testing.T) {
	hermetic(t)
	long := strings.Repeat("x", maxTXTRunes+500)
	resolver := &fakeResolver{txt: []string{long}}
	opts := Options{Resolver: resolver}

	result, err := CollectDNS(context.Background(), opts, "example.com", []string{"TXT"})
	if err != nil {
		t.Fatalf("CollectDNS: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("records = %v, want one", result.Records)
	}
	if len([]rune(result.Records[0].Value)) != maxTXTRunes {
		t.Errorf("the value is %d runes, want the %d cap", len([]rune(result.Records[0].Value)), maxTXTRunes)
	}
	cut := false
	for _, note := range result.Notes {
		if strings.Contains(note, "cut") {
			cut = true
		}
	}
	if !cut {
		t.Errorf("the cut value carried no note: %v", result.Notes)
	}
}

// TestTheCNAMEIsReportedAndNotChased: the chain is a set of requests the
// caller did not ask for, and the target is named so it can ask for them.
func TestTheCNAMEIsReportedAndNotChased(t *testing.T) {
	hermetic(t)
	resolver := &fakeResolver{cname: "Edge.Example.NET."}
	opts := Options{Resolver: resolver}

	result, err := CollectDNS(context.Background(), opts, "example.com", []string{"CNAME"})
	if err != nil {
		t.Fatalf("CollectDNS: %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].Type != "CNAME" {
		t.Fatalf("records = %v, want one CNAME", result.Records)
	}
	if result.Records[0].Value != "edge.example.net" {
		t.Errorf("the CNAME value is %q, want edge.example.net", result.Records[0].Value)
	}
	mentioned := false
	for _, note := range result.Notes {
		if strings.Contains(note, "edge.example.net") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the note does not name the target: %v", result.Notes)
	}
	for _, lookup := range resolver.lookups {
		if lookup != "cname" {
			t.Errorf("the chain was chased: %v", resolver.lookups)
			break
		}
	}
}

// TestANameWithoutARecordIsNotAFailure: no CNAME is an answer.
func TestANameWithoutARecordIsNotAFailure(t *testing.T) {
	hermetic(t)
	resolver := &fakeResolver{cnameErr: &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}}
	opts := Options{Resolver: resolver}

	result, err := CollectDNS(context.Background(), opts, "example.com", []string{"CNAME"})
	if err != nil {
		t.Fatalf("CollectDNS: %v", err)
	}
	if len(result.Records) != 0 {
		t.Errorf("records = %v, want none", result.Records)
	}
}

// TestAnUnsupportedTypeIsNamedInANote: a type that cannot be looked up is
// said so, and the types that can be looked up are still answered.
func TestAnUnsupportedTypeIsNamedInANote(t *testing.T) {
	hermetic(t)
	resolver := &fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}
	opts := Options{Resolver: resolver}

	result, err := CollectDNS(context.Background(), opts, "example.com", []string{"A", "SOA", "WKS"})
	if err != nil {
		t.Fatalf("CollectDNS: %v", err)
	}
	if len(result.Records) != 1 {
		t.Errorf("records = %v, want the A record", result.Records)
	}
	joined := strings.Join(result.Notes, "; ")
	if !strings.Contains(joined, "SOA") || !strings.Contains(joined, "WKS") {
		t.Errorf("the notes do not name the unsupported types: %v", result.Notes)
	}
}

// TestAnEmptyNameIsRefused: there is nothing to resolve.
func TestAnEmptyNameIsRefused(t *testing.T) {
	hermetic(t)
	opts := Options{Resolver: &fakeResolver{}}
	if _, err := CollectDNS(context.Background(), opts, "  ", nil); err == nil {
		t.Fatal("an empty name was accepted")
	}
}

package netguard

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// The addresses that matter most are the ones this machine already answers on.
// Cloud credentials sit at the first; the Mini App, the catalog cache and the
// bridge sit at the second.
func TestCheckAddrRefusesTheAddressesThisMachineAnswersOn(t *testing.T) {
	cases := map[string]string{
		"cloud metadata (aws/ec2)": "169.254.169.254",
		"cloud metadata (gcp)":     "169.254.169.253",
		"loopback":                 "127.0.0.1",
		"loopback alias":           "127.1.2.3",
		"unspecified":              "0.0.0.0",
		"rfc1918 10/8":             "10.0.0.5",
		"rfc1918 172.16/12":        "172.16.4.9",
		"rfc1918 192.168/16":       "192.168.1.1",
		"carrier grade nat":        "100.64.0.1",
		"multicast":                "239.1.2.3",
	}
	for name, address := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckAddr(net.ParseIP(address), address)
			if err == nil {
				t.Fatalf("%s was allowed", address)
			}
			if !errors.Is(err, ErrBlocked) {
				t.Errorf("%s was refused with %v, which callers cannot detect with errors.Is", address, err)
			}
		})
	}
}

// IPv6 and the 4-in-6 forms have to behave the same, or the guard is one
// representation away from being decorative.
func TestCheckAddrRefusesTheIPv6AndMappedForms(t *testing.T) {
	for _, address := range []string{
		"::1",
		"fe80::1",
		"fc00::1",
		"::ffff:127.0.0.1",
		"::ffff:169.254.169.254",
		"::ffff:10.0.0.1",
		"ff02::1",
	} {
		ip := net.ParseIP(address)
		if ip == nil {
			t.Fatalf("test address %q does not parse", address)
		}
		if err := CheckAddr(ip, address); err == nil {
			t.Errorf("%s was allowed", address)
		}
	}
}

func TestCheckAddrAllowsPublicAddresses(t *testing.T) {
	for _, address := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := CheckAddr(net.ParseIP(address), address); err != nil {
			t.Errorf("%s was refused: %v", address, err)
		}
	}
}

// The refusal has to name the address, because it is read by a model that can
// then say something useful about it. "the request was refused" is not a message.
func TestTheRefusalNamesTheAddressAndTheReason(t *testing.T) {
	err := CheckAddr(net.ParseIP("169.254.169.254"), "example")
	if err == nil {
		t.Fatal("metadata address was allowed")
	}
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("the refusal does not name the address: %v", err)
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestCheckURLRefusesTheMetadataHostnameEvenBeforeResolving(t *testing.T) {
	for _, raw := range []string{
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://metadata.goog/computeMetadata/v1/",
		"https://metadata.google.internal/",
	} {
		err := CheckURL(context.Background(), raw)
		if err == nil {
			t.Errorf("%s was allowed", raw)
			continue
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s refused with %v", raw, err)
		}
	}
}

func TestCheckURLKeepsTheSchemeRestriction(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ftp://example.com/x",
		"data:text/html,<script>alert(1)</script>",
		"",
		"   ",
	} {
		if err := CheckURL(context.Background(), raw); err == nil {
			t.Errorf("%q was allowed", raw)
		}
	}
}

func TestCheckURLAllowsAPublicAddress(t *testing.T) {
	if err := CheckURL(context.Background(), "https://example.com/some/path?q=1"); err != nil {
		t.Fatalf("a public URL was refused: %v", err)
	}
}

// Looking at the app you are running on localhost is real work. Pretending
// otherwise gets this guard switched off rather than configured, so the escape
// hatch is explicit and the guard still refuses everything else.
func TestTheOverrideReopensOnlyWhatItNames(t *testing.T) {
	t.Setenv(AllowPrivateEnv, "127.0.0.1/32")

	if err := CheckAddr(net.ParseIP("127.0.0.1"), "127.0.0.1"); err != nil {
		t.Fatalf("an explicitly permitted range was still refused: %v", err)
	}
	if err := CheckAddr(net.ParseIP("169.254.169.254"), "169.254.169.254"); err == nil {
		t.Fatal("the override leaked to an address it did not name")
	}
	if err := CheckAddr(net.ParseIP("10.1.2.3"), "10.1.2.3"); err == nil {
		t.Fatal("the override leaked to a different private range")
	}
}

func TestOverridesReportsWhetherTheHatchIsInUse(t *testing.T) {
	t.Setenv(AllowPrivateEnv, "")
	if entries, on := Overrides(); on || len(entries) != 0 {
		t.Fatalf("Overrides() = %v, %v with the variable unset", entries, on)
	}
	t.Setenv(AllowPrivateEnv, " 10.0.0.0/8 , 192.168.0.0/16 ")
	entries, on := Overrides()
	if !on {
		t.Fatal("Overrides() reported the hatch closed while it is set")
	}
	if len(entries) != 2 || entries[0] != "10.0.0.0/8" {
		t.Fatalf("Overrides() = %v, want the two trimmed entries", entries)
	}
}

// The dial-time check is the one that holds: a name can resolve to a public
// address during validation and to 127.0.0.1 when the transport connects. This
// is DNS rebinding, and the pre-flight check cannot see it.
func TestDialContextRefusesAConnectionToALoopbackAddress(t *testing.T) {
	t.Setenv(AllowPrivateEnv, "")
	_, err := DialContext(context.Background(), "tcp", "127.0.0.1:8787")
	if err == nil {
		t.Fatal("the dialer opened a connection to loopback")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("the refusal was not detectable: %v", err)
	}
}

func TestDialContextRefusesAConnectionToMetadata(t *testing.T) {
	t.Setenv(AllowPrivateEnv, "")
	_, err := DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("the dialer reached the metadata service: %v", err)
	}
}

func TestTransportInstallsTheGuard(t *testing.T) {
	transport := Transport()
	if transport == nil || transport.DialContext == nil {
		t.Fatal("the transport has no guarded dialer")
	}
}

// A name that does not resolve is not a security event. Letting it through means
// the caller sees an honest DNS failure instead of a guard message about a host
// that never existed, and the dial-time check is what actually holds the line.
func TestAnUnresolvableHostIsLeftForTheTransportToFailOn(t *testing.T) {
	previous := ActiveResolver
	ActiveResolver = stubResolver{err: errors.New("no such host")}
	t.Cleanup(func() { ActiveResolver = previous })

	if err := CheckURL(context.Background(), "https://host.invalid.example/x"); err != nil {
		t.Fatalf("an unresolvable name was refused with a guard error: %v", err)
	}
}

// And a name that resolves into a private range is refused, which is the whole
// point of resolving rather than trusting the shape of the URL.
func TestANameResolvingIntoAPrivateRangeIsRefused(t *testing.T) {
	previous := ActiveResolver
	ActiveResolver = stubResolver{addrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	t.Cleanup(func() { ActiveResolver = previous })

	err := CheckURL(context.Background(), "https://sneaky.example/x")
	if err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("a name resolving to loopback was allowed: %v", err)
	}
}

// Every address a name resolves to is checked. One of several answers being
// public is not evidence the name is safe.
func TestEveryResolvedAddressIsChecked(t *testing.T) {
	previous := ActiveResolver
	ActiveResolver = stubResolver{addrs: []net.IPAddr{
		{IP: net.ParseIP("93.184.216.34")},
		{IP: net.ParseIP("169.254.169.254")},
	}}
	t.Cleanup(func() { ActiveResolver = previous })

	if err := CheckURL(context.Background(), "https://mixed.example/x"); err == nil {
		t.Fatal("a name with one private answer was allowed")
	}
}

type stubResolver struct {
	addrs []net.IPAddr
	err   error
}

func (s stubResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.addrs, nil
}

func (s stubResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]net.IP, 0, len(s.addrs))
	for _, addr := range s.addrs {
		out = append(out, addr.IP)
	}
	return out, nil
}

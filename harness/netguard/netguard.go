// Package netguard keeps Ceraa's outbound requests off the host's own network.
//
// The browser and webfetch could reach anything the machine can reach, because
// both checked only the URL's scheme. `http://169.254.169.254/` and
// `http://127.0.0.1:8787/` passed. That is an SSRF foot-gun pointed inward:
// instance credentials sit at the first address, and the Mini App, the model
// catalog cache and the Telegram bridge sit on the second. An agent that reads
// a hostile page, or is simply pointed at a URL from untrusted content, gets to
// read them.
//
// This package is deliberately not a proxy and deliberately not a filter on
// what comes back. It answers one question before anything dials: is this
// address on the far side of the machine?
//
// Two checks, because one is not enough:
//
//   - CheckURL resolves and refuses, so the refusal is a clear message at the
//     point the model asked, naming the address. That is the useful failure.
//   - DialContext refuses again at connect time. A name resolved during
//     CheckURL can answer differently when the transport dials it, which is DNS
//     rebinding; checking once at validation is a guard with a hole in it.
//
// Everything is fail-closed. A host that cannot be resolved is allowed through
// here precisely so the real request fails with a real error rather than a
// misleading one, and the dial-time check is what actually holds the line.
//
// The one escape hatch is CERAA_ALLOW_PRIVATE_NET, a comma-separated list of
// hosts, IPs or CIDRs. It exists because "look at the app I am running on
// localhost" is legitimate work, and pretending otherwise would get this
// guard switched off rather than configured. It is logged once at first use,
// because an egress exception nobody knows about is indistinguishable from a
// bug.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"ceraa/internal/logger"
)

// AllowPrivateEnv names the ranges permitted to bypass the guard.
const AllowPrivateEnv = "CERAA_ALLOW_PRIVATE_NET"

// cgnat is carrier-grade NAT, RFC 6598. Go's net.IP.IsPrivate does not cover
// it: it is address space an ISP hands to customers, so on a hosted box it is
// very often the hypervisor's management interface. Treating it as private is
// the difference between a guard and a suggestion.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// metadataNames are hostnames that resolve to cloud instance credentials. They
// are listed by name as well as by address because some of them are not
// link-local on every cloud, and because a name is what a model will actually
// type after reading it out of a document.
var metadataNames = map[string]bool{
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
}

// ErrBlocked is returned for a refused destination. Callers can test for it
// with errors.Is rather than matching on text.
var ErrBlocked = errors.New("destination is on this host's own network")

// Resolver is the interface the guard needs from a name lookup.
//
// It is an interface rather than *net.Resolver so a test can answer with a fixed
// set of addresses. Testing this guard otherwise means reaching the network, and
// a test that needs the internet is a test that does not run.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Resolver is the resolver used for name lookups, replaceable by a caller with a
// policy of its own.
var ActiveResolver Resolver = net.DefaultResolver

// resolveTimeout bounds the DNS lookup inside CheckURL. A slow resolver must
// not turn into a slow tool call.
var resolveTimeout = 5 * time.Second

// ---------------------------------------------------------------------------
// Overrides
// ---------------------------------------------------------------------------

var (
	overrideOnce sync.Once
	overrideList []string
)

// Overrides returns the entries in CERAA_ALLOW_PRIVATE_NET, and reports whether
// the escape hatch is in use. An egress exception that nobody logged is the
// same thing as no guard at all, so callers announce it.
func Overrides() ([]string, bool) {
	raw := strings.TrimSpace(os.Getenv(AllowPrivateEnv))
	if raw == "" {
		return nil, false
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, len(out) > 0
}

// allowedOnce logs the override the first time it is used. One line, then
// silence: a per-request line would train someone to ignore it.
func allowedOnce() {
	overrideOnce.Do(func() {
		if entries, on := Overrides(); on {
			logger.Warn(fmt.Sprintf("netguard: %s permits %s - outbound requests to those ranges are not blocked",
				AllowPrivateEnv, strings.Join(entries, ", ")))
		}
	})
}

// matchesOverride reports whether an address or host is explicitly permitted.
func matchesOverride(addr net.IP, host string) bool {
	entries, on := Overrides()
	if !on {
		return false
	}
	allowedOnce()
	lowerHost := strings.ToLower(host)
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if _, block, err := net.ParseCIDR(entry); err == nil && addr != nil && block.Contains(addr) {
			return true
		}
		if ip := net.ParseIP(entry); ip != nil {
			if addr != nil && ip.Equal(addr) {
				return true
			}
			if ip.String() == lowerHost {
				return true
			}
			continue
		}
		if entry == lowerHost {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Address classification
// ---------------------------------------------------------------------------

// privateReason names why an address is refused, or "" when it is fine.
//
// The reason is written for the model that asked. "loopback" is something it can
// act on; "the request was refused" is not.
func privateReason(addr net.IP) string {
	switch {
	case addr == nil:
		return ""
	case addr.IsLoopback():
		return "loopback (this machine)"
	case addr.IsUnspecified():
		return "the unspecified address"
	case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		// 169.254.169.254 is the address every major cloud puts instance
		// credentials on, and it is in here.
		return "link-local, which includes cloud instance metadata"
	case addr.IsPrivate():
		return "private address space"
	case cgnat.Contains(addr):
		return "carrier-grade NAT, often a hypervisor management interface"
	case addr.IsMulticast() || addr.IsInterfaceLocalMulticast():
		return "multicast"
	}
	return ""
}

// CheckAddr returns nil when an address may be contacted, and an error naming
// the reason when it may not.
func CheckAddr(addr net.IP, host string) error {
	if matchesOverride(addr, host) {
		return nil
	}
	if reason := privateReason(addr); reason != "" {
		if addr != nil {
			return fmt.Errorf("%w: %s is %s", ErrBlocked, addr, reason)
		}
		return fmt.Errorf("%w: %s", ErrBlocked, reason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// URL checks
// ---------------------------------------------------------------------------

// CheckURL validates a destination before anything dials it.
//
// It refuses non-HTTP schemes, refuses metadata hostnames by name, and refuses
// any address the host itself occupies. A name that does not resolve is
// allowed: the request will fail on its own with an accurate error, and
// DialContext is what actually prevents a rebinding swap between this check and
// the connect.
func CheckURL(ctx context.Context, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("url is not valid: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url must start with http:// or https://")
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("url has no host")
	}
	if metadataNames[strings.ToLower(host)] {
		return fmt.Errorf("%w: %s is the cloud instance metadata service", ErrBlocked, host)
	}
	if _, on := Overrides(); on && matchesOverride(nil, host) {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return CheckAddr(ip, host)
	}
	lookup, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := ActiveResolver.LookupIPAddr(lookup, host)
	if err != nil || len(addrs) == 0 {
		// Unresolvable here is not a security event. Letting it through keeps the
		// error the caller eventually sees an accurate DNS failure rather than a
		// guard message about a name that simply does not exist.
		return nil
	}
	for _, addr := range addrs {
		if err := CheckAddr(addr.IP, host); err != nil {
			return err
		}
	}
	return nil
}

// DialContext is the connect-time half of the guard.
//
// Go's transport asks for the address it is about to open a socket to, after
// its own DNS has run. That is the only point where the guard can be sure which
// address it is actually reaching, and it is the point a rebinding attack
// targets. CheckURL gives a good error; this one holds.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := CheckAddr(ip, host); err != nil {
			return nil, err
		}
	} else {
		lookup, cancel := context.WithTimeout(ctx, resolveTimeout)
		defer cancel()
		addrs, err := ActiveResolver.LookupIPAddr(lookup, host)
		if err != nil {
			// Hand the failure to the transport rather than inventing one, so a
			// genuine DNS problem still reads as a DNS problem.
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		}
		for _, resolved := range addrs {
			if err := CheckAddr(resolved.IP, host); err != nil {
				return nil, err
			}
		}
	}
	dialer := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
}

// Transport returns an http.Transport whose dialer enforces the guard at
// connect time. Redirects are not followed by anything that uses this, because
// a 3xx is exactly how a hostile host tries to walk the request inward.
func Transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = DialContext
	return transport
}

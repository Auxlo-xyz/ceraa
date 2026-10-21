// Package enum collects passive reconnaissance: the host names a domain is
// known by, the DNS records behind a name, and the network registration of an
// address.
//
// The sources are public logs and registries, and they are not cooperative.
// crt.sh answers 502 often enough that a lookup trusting one source fails
// half the time, and the authoritative RDAP server for an address is reached
// through a redirector rather than directly. So every collection here names
// the source that answered, falls back when the primary one does not, and
// leaves a field empty rather than guessing at it.
//
// Everything outbound goes through the same two checks the probe uses: the
// scope gate decides whether a destination may be contacted at all, the
// engagement budget decides whether it may be contacted now, and netguard
// refuses the host's own network at connect time.
package enum

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ceraa/harness/netguard"
	"ceraa/harness/recon"
	"ceraa/internal/build"
)

const (
	// maxBodyBytes bounds one response body. The certificate transparency
	// logs answer with a JSON array that grows with the size of the domain,
	// and a huge domain is exactly the case worth answering, so the cap is
	// generous but finite.
	maxBodyBytes = 2 * 1024 * 1024
	// defaultTimeout bounds one collection run, source fallbacks included.
	defaultTimeout = 25 * time.Second
	// maxRedirects bounds the hops a redirector may take. rdap.org is a
	// redirector by design: it answers with a 302 to the authoritative RDAP
	// server for the address, and that server holds the answer.
	maxRedirects = 3
)

// userAgent identifies these requests the way the probe identifies its own.
// Traffic that lands in someone else's logs should be findable in them.
var userAgent = "ceraa-recon/" + build.Version + " (+https://github.com/emperormk01/ceraa)"

// Resolver is the DNS seam.
//
// It is an interface rather than *net.Resolver so a test can answer with a
// fixed set of records. A test that needs the network is a test that does not
// run. *net.Resolver satisfies it as it stands.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, host string) ([]*net.MX, error)
	LookupNS(ctx context.Context, host string) ([]*net.NS, error)
	LookupTXT(ctx context.Context, host string) ([]string, error)
}

// Options carries the injectable seams.
//
// The zero value talks to the real world through the egress wall, which is
// what production uses. A test fills Transport and Resolver and never leaves
// the machine. The seams are fields on a struct rather than package globals
// because a global transport is a global the next test inherits.
type Options struct {
	// Transport carries every outbound HTTP request. nil means
	// netguard.Transport(), the wall that refuses the host's own network at
	// connect time.
	Transport http.RoundTripper
	// Resolver resolves names. nil means net.DefaultResolver.
	Resolver Resolver
	// Timeout bounds one collection run. zero means defaultTimeout.
	Timeout time.Duration
	// RetryBackoff is the wait between attempts at the primary source.
	// zero means defaultRetryBackoff, which a test shortens.
	RetryBackoff time.Duration
}

// timeout is the run budget, defaulted.
func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return defaultTimeout
}

// backoff is the wait between attempts, defaulted.
func (o Options) backoff() time.Duration {
	if o.RetryBackoff > 0 {
		return o.RetryBackoff
	}
	return defaultRetryBackoff
}

// transport is the wall, or the seam the caller supplied.
func (o Options) transport() http.RoundTripper {
	if o.Transport != nil {
		return o.Transport
	}
	return netguard.Transport()
}

// resolver is the system resolver, or the seam the caller supplied.
func (o Options) resolver() Resolver {
	if o.Resolver != nil {
		return o.Resolver
	}
	return net.DefaultResolver
}

// defaultRetryBackoff is the pause between attempts at a flaky source.
const defaultRetryBackoff = 750 * time.Millisecond

// fetch performs one bounded GET and hands back the status and the body.
//
// The order is the house order and it is load-bearing: the scope gate decides
// whether the destination may be contacted, the budget decides whether it may
// be contacted now, and only then does anything dial. A refusal therefore
// means nothing was sent, which is the whole point of gating before the
// request rather than after it.
//
// A redirect is only followed when the caller says hops is greater than zero,
// and every hop is gated and charged in its own right: a hop is a request.
func fetch(ctx context.Context, opts Options, rawurl string, source string, hops int) (int, []byte, error) {
	client := &http.Client{
		Transport: opts.transport(),
		Timeout:   opts.timeout(),
		// Redirects are handled here rather than by the transport: the hop
		// has to be gated and charged like any other request, and the
		// transport cannot do that.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	current := rawurl
	for hop := 0; ; hop++ {
		if err := recon.Gate(current); err != nil {
			return 0, nil, fmt.Errorf("%s: %w", source, err)
		}
		if err := recon.Take(1); err != nil {
			return 0, nil, fmt.Errorf("%s: %w", source, err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: build request: %w", source, err)
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: %w", source, err)
		}
		body, readErr := readBounded(resp.Body)
		status := resp.StatusCode
		location := resp.Header.Get("Location")
		resp.Body.Close()
		if readErr != nil {
			return status, nil, fmt.Errorf("%s: %w", source, readErr)
		}

		if status >= 300 && status < 400 && location != "" && hop < hops {
			next, err := resolveLocation(current, location)
			if err != nil {
				return status, body, fmt.Errorf("%s: redirect to %q: %w", source, location, err)
			}
			current = next
			continue
		}
		if status < 200 || status > 299 {
			return status, nil, fmt.Errorf("%s: answered %d", source, status)
		}
		return status, body, nil
	}
}

// resolveLocation turns a Location header into the URL to ask next.
//
// The header is resolved against the URL that was asked for, because a
// redirector may answer with a relative path and the next request has to be
// a full one.
func resolveLocation(current string, location string) (string, error) {
	next, err := url.Parse(strings.TrimSpace(location))
	if err != nil {
		return "", err
	}
	if next.IsAbs() {
		return next.String(), nil
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(next).String(), nil
}

// readBounded reads at most maxBodyBytes and refuses a body that does not
// fit. Truncating instead would hand a JSON parser half a document, and the
// parse error would say nothing about the real cause.
func readBounded(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(maxBodyBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxBodyBytes {
		return nil, fmt.Errorf("body is larger than the %dB cap", maxBodyBytes)
	}
	return data, nil
}

// sleep waits, or gives up when the run budget is spent.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

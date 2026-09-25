package freebuff

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client is the Freebuff HTTP client. It mirrors the Freebuff CLI's
// codebuff-api request behavior: bounded retries with exponential backoff and
// jitter, no retry on TLS verification failures (the user needs the "your
// network intercepts HTTPS" message, not four identical retries), and no
// logging of request bodies or the Authorization header.
type Client struct {
	HTTPClient *http.Client
	WebBaseURL string
	APIBaseURL string
	// Now and Sleep are injectable for deterministic tests.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

func NewClient() *Client {
	return &Client{
		HTTPClient: newSecureClient(),
		WebBaseURL: DefaultWebBaseURL,
		APIBaseURL: DefaultAPIBaseURL,
		Now:        time.Now,
		Sleep:      sleepCtx,
	}
}

// newSecureClient returns an HTTP client that keeps a bearer token attached
// across same-origin redirects and refuses to send it anywhere else.
//
// Two failure modes are being avoided at once. Go drops Authorization on a
// cross-host redirect, so a canonical-host redirect would silently arrive
// unauthenticated and read as a revoked token. And blindly re-attaching the
// header on every redirect would hand the token to whatever host the redirect
// names. The comparison is on the full host:port, not the hostname, because a
// different port on the same host is a different origin and may be an
// attacker-controlled local service. Since the API base is the canonical
// non-redirecting host, a legitimate redirect should never arise at all.
func newSecureClient() *http.Client {
	return &http.Client{
		Timeout: RequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			origin := via[0].URL.Host
			target := req.URL.Host
			if strings.EqualFold(origin, target) {
				// Same origin: carry the credential forward explicitly, because
				// net/http has already dropped it by this point.
				for _, header := range []string{"Authorization", "Cookie"} {
					if value := via[len(via)-1].Header.Get(header); value != "" {
						req.Header.Set(header, value)
					}
				}
				return nil
			}
			return fmt.Errorf("refusing to follow redirect for a credentialed request from %s to %s", origin, target)
		},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

func (c *Client) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: RequestTimeout}
}

func (c *Client) webBase() string {
	if strings.TrimSpace(c.WebBaseURL) != "" {
		return strings.TrimRight(c.WebBaseURL, "/")
	}
	return DefaultWebBaseURL
}

func (c *Client) apiBase() string {
	if strings.TrimSpace(c.APIBaseURL) != "" {
		return strings.TrimRight(c.APIBaseURL, "/")
	}
	return DefaultAPIBaseURL
}

// readLimited reads at most 1 MiB from a response body.
func readLimited(response *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(response.Body, 1<<20))
}

// localTimezone reports an IANA timezone name for the scheduling-preference
// header. Go's time package exposes only abbreviations like "UTC" or "IST", so
// the IANA name is read from TZ, /etc/timezone, or the /etc/localtime symlink.
// A timezone is a scheduling preference, never proof of country or access, so
// returning "" is safe and correct.
func localTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" && !strings.HasPrefix(tz, ":") {
		return tz
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		// e.g. /usr/share/zoneinfo/Europe/Berlin
		if idx := strings.Index(target, "zoneinfo/"); idx >= 0 {
			return target[idx+len("zoneinfo/"):]
		}
	}
	return ""
}

// requestOptions describes a single logical request.
type requestOptions struct {
	method  string
	path    string
	query   url.Values
	body    any
	token   string
	baseURL string
	// retryable overrides the default retryable status set. A hosted-session
	// POST may already have rotated the active instance, so it is retried only
	// on statuses proven to be pre-commit; see session.go.
	retryable map[int]bool
	// allowNonOK returns the body and status without error for non-2xx, letting
	// the caller interpret (login /status answers 200 on every failure mode).
	allowNonOK bool
}

var defaultRetryable = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

// httpResult is a completed HTTP exchange.
type httpResult struct {
	status int
	body   []byte
}

// isTLSError reports whether err is a TLS certificate verification failure,
// which the CLI treats as non-retryable.
func isTLSError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostnameErr x509.HostnameError
	if errors.As(err, &unknownAuthority) || errors.As(err, &certInvalid) || errors.As(err, &hostnameErr) {
		return true
	}
	// A *url.Error wrapping a tls error also carries a RecordHeaderError for
	// hostname mismatches in some paths.
	var recordErr tls.RecordHeaderError
	return errors.As(err, &recordErr)
}

// do performs a request with the CLI's retry policy.
func (c *Client) do(ctx context.Context, opts requestOptions) (httpResult, error) {
	base := opts.baseURL
	if base == "" {
		base = c.webBase()
	}
	endpoint := strings.TrimRight(base, "/") + opts.path
	if len(opts.query) > 0 {
		endpoint += "?" + opts.query.Encode()
	}
	var payload []byte
	if opts.body != nil {
		encoded, err := json.Marshal(opts.body)
		if err != nil {
			return httpResult{}, fmt.Errorf("encode freebuff request: %w", err)
		}
		payload = encoded
	}

	retryable := opts.retryable
	if retryable == nil {
		retryable = defaultRetryable
	}

	var lastErr error
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return httpResult{}, err
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, opts.method, endpoint, reader)
		if err != nil {
			return httpResult{}, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if opts.token != "" {
			req.Header.Set("Authorization", "Bearer "+opts.token)
		}
		resp, err := c.http().Do(req)
		if err != nil {
			// TLS failures never retry; context cancellation never retries.
			if isTLSError(err) {
				return httpResult{}, fmt.Errorf("freebuff TLS verification failed (your network may be intercepting HTTPS): %w", err)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return httpResult{}, err
			}
			lastErr = err
			if attempt == MaxAttempts-1 {
				break
			}
			if serr := c.sleep(ctx, backoff(attempt)); serr != nil {
				return httpResult{}, serr
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt == MaxAttempts-1 {
				break
			}
			if serr := c.sleep(ctx, backoff(attempt)); serr != nil {
				return httpResult{}, serr
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return httpResult{status: resp.StatusCode, body: body}, nil
		}
		if opts.allowNonOK {
			return httpResult{status: resp.StatusCode, body: body}, nil
		}
		lastErr = fmt.Errorf("freebuff %s %s returned HTTP %d", opts.method, opts.path, resp.StatusCode)
		if !retryable[resp.StatusCode] {
			return httpResult{status: resp.StatusCode, body: body}, lastErr
		}
		if attempt == MaxAttempts-1 {
			break
		}
		if serr := c.sleep(ctx, backoff(attempt)); serr != nil {
			return httpResult{}, serr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("freebuff request failed")
	}
	return httpResult{}, lastErr
}

// backoff computes delay = min(base*2^attempt, max) + jitter, matching the
// CLI's 1000 * 2^attempt + up-to-30% jitter.
func backoff(attempt int) time.Duration {
	delay := RetryBaseDelay
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= RetryMaxDelay {
			delay = RetryMaxDelay
			break
		}
	}
	if delay > RetryMaxDelay {
		delay = RetryMaxDelay
	}
	jitter := time.Duration(rand.Float64() * RetryJitter * float64(delay))
	return delay + jitter
}

// LoginCode is the response from POST /api/auth/cli/code.
//
// ExpiresAt is kept as raw JSON because the live endpoint returns it as an
// epoch-millis *number* (e.g. 1790360296254) even though the Freebuff TS types
// declare it a string. It is an HMAC input the status endpoint verifies, so it
// is echoed back byte-for-byte and never parsed, reformatted, or converted.
type LoginCode struct {
	LoginURL        string          `json:"loginUrl"`
	FingerprintHash string          `json:"fingerprintHash"`
	ExpiresAt       json.RawMessage `json:"expiresAt"`
	ExpiresInMs     int64           `json:"expiresInMs"`
}

// expiresAtParam renders the raw expiresAt value for the status query. The raw
// bytes are used verbatim so the server sees exactly what it signed.
func (c LoginCode) expiresAtParam() string {
	trimmed := strings.TrimSpace(string(c.ExpiresAt))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var decoded string
		if json.Unmarshal(c.ExpiresAt, &decoded) == nil {
			return decoded
		}
	}
	return trimmed
}

// Code requests a device login URL. The Freebuff CLI treats a non-2xx or an
// empty 2xx as a hard error rather than "trying anyway".
func (c *Client) Code(ctx context.Context, fingerprintID string) (LoginCode, error) {
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	result, err := c.do(reqCtx, requestOptions{
		method: http.MethodPost,
		path:   codePath,
		body:   map[string]string{"fingerprintId": fingerprintID},
	})
	if err != nil {
		return LoginCode{}, err
	}
	var code LoginCode
	if json.Unmarshal(result.body, &code) != nil {
		return LoginCode{}, errors.New("freebuff login code response is invalid JSON")
	}
	if strings.TrimSpace(code.LoginURL) == "" {
		return LoginCode{}, errors.New("freebuff login code response has no loginUrl")
	}
	if code.expiresAtParam() == "" {
		return LoginCode{}, errors.New("freebuff login code response has no expiresAt")
	}
	return code, nil
}

// StatusResponse is the response from GET /api/auth/cli/status.
type StatusResponse struct {
	User *User `json:"user"`
	// Some deployments may include an error string on non-2xx; keep it opaque.
	Error string `json:"error"`
}

// Status polls the login status. A user object means the browser approved the
// login. A nil user with a nil error means "keep waiting" (the CLI's polling
// loop treats every non-success as retryable and only a user object as done).
func (c *Client) Status(ctx context.Context, fingerprintID, fingerprintHash, expiresAt string) (*User, error) {
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	query := url.Values{}
	query.Set("fingerprintId", fingerprintID)
	query.Set("fingerprintHash", fingerprintHash)
	query.Set("expiresAt", expiresAt)
	result, err := c.do(reqCtx, requestOptions{
		method:     http.MethodGet,
		path:       statusPath,
		query:      query,
		allowNonOK: true, // 401 and friends are "still waiting"
	})
	if err != nil {
		// Network-level failure: the CLI logs and continues polling, so we
		// surface a nil user with nil error to keep the loop alive.
		return nil, nil
	}
	var status StatusResponse
	if json.Unmarshal(result.body, &status) != nil {
		return nil, nil // non-JSON body is still "not approved"
	}
	if status.User == nil || strings.TrimSpace(status.User.AuthToken) == "" {
		return nil, nil
	}
	return status.User, nil
}

// Me validates the token against GET /api/v1/me. 401/403/404 all mean auth
// failed (the SDK normalizes 404 to 401); 5xx is transient and must not clear
// local state.
func (c *Client) Me(ctx context.Context, token string) (*User, error) {
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	result, err := c.do(reqCtx, requestOptions{
		method:  http.MethodGet,
		path:    mePath,
		token:   token,
		baseURL: c.apiBase(),
	})
	if err != nil {
		return nil, err
	}
	var user User
	if json.Unmarshal(result.body, &user) != nil {
		return nil, errors.New("freebuff /api/v1/me returned invalid JSON")
	}
	return &user, nil
}

// MeAuthError reports whether an error from Me is an authentication failure
// (401/403/404) rather than a transient error. A 401 means the user must
// re-login; a 5xx or network error must not clear local state.
func MeAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if strings.Contains(msg, "HTTP "+strconv.Itoa(code)) {
			return true
		}
	}
	return false
}

// Logout calls POST /api/auth/cli/logout. Per the CLI, a failed logout request
// is swallowed: local state is always cleared and the operation always reports
// success, so logout is idempotent and never leaves the user stuck.
func (c *Client) Logout(ctx context.Context, token string, user User) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	_, _ = c.do(reqCtx, requestOptions{
		method:  http.MethodPost,
		path:    logoutPath,
		token:   token,
		body:    map[string]string{"userId": user.ID, "fingerprintId": user.FingerprintID, "fingerprintHash": user.FingerprintHash},
		baseURL: c.apiBase(),
	})
	return nil
}

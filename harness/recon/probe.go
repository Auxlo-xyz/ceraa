package recon

// probe.go is the multipurpose HTTP probe: the shared core of the planned
// http_request, audit_http_headers, cors_probe and recon_capture_evidence
// tools, delivered as one observation engine.
//
// The design rules are deliberate, and each exists because of a concrete
// failure mode:
//
//   - Scope (Gate) is the authorisation layer; netguard is the wall. A probe
//     to a URL the model passed is still refused at dial time if it resolves
//     onto this host, because a name can answer differently between the two
//     checks. This file never trusts validation alone.
//   - Redirects are never followed automatically. The scope Gate was applied
//     to the URL that was asked for, not to wherever a 3xx says to go, so a
//     handled redirect must come back as a RESULT and be probed explicitly
//     if it is wanted.
//   - Everything is bounded: a 512KB body cap, a 1200-rune head, a fixed
//     header subset. Receipts cite evidence; they do not warehouse it.
//   - Receipts are written but never fail the probe: the observation the
//     model asked for travels in the result, the file is bookkeeping.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ceraa/harness/netguard"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
)

const (
	defaultProbeTimeoutSecs = 15
	maxProbeTimeoutSecs     = 120
	// maxBodyBytes bounds the read. Large is not free: the receipt cites the
	// truncated read, so the cap is part of what the receipt attests to.
	maxBodyBytes = 512 * 1024
	// bodyHeadRunes caps the echoed head. Runes, not bytes, so the cut lands
	// on a character boundary and multibyte text stays decodable.
	bodyHeadRunes = 1200
	// maxRequestBodyBytes bounds what may be SENT. A probe uploads nothing
	// heavier than a small payload form; 64KB covers every honest use.
	maxRequestBodyBytes = 64 * 1024
)

// probeClient is the client every probe dial goes through. nil means the
// guarded fleet client, whose transport enforces the netguard address rules
// at connect time. Tests set it to a plain client over httptest so the suite
// exercises the probe logic without touching the wall.
var probeClient *http.Client

// receiptsBase is the root receipts live under ("recon/receipts"). Tests aim
// it at a temp directory so the suite never writes into the data dir.
var receiptsBase = paths.DataDir()

// Request describes one outbound HTTP probe.
type Request struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
	// Timeout is in seconds, default 15, maximum 120.
	Timeout int
}

// Result is one bounded observation. Every field is safe to echo to a human.
type Result struct {
	URL    string
	Method string
	Status int
	// Headers is the bounded subset worth keeping; set-cookie keeps only a
	// presence note, never its values, because cookie values are credentials.
	Headers      map[string]string
	BodyHead     string
	BodyBytes    int
	SHA256       string
	RedirectedTo string
	ElapsedMS    int64
	// ReceiptID is the file base under DataDir()/recon/receipts, or empty
	// when the receipt could not be written. Always empty means "asked and
	// not recorded", so callers can distinguish it from a read of the file.
	ReceiptID string
}

// receiptDoc is the on-disk shape of a probe receipt.
type receiptDoc struct {
	At           string            `json:"at"`
	URL          string            `json:"url"`
	Method       string            `json:"method"`
	Status       int               `json:"status"`
	SHA256       string            `json:"sha256"`
	Headers      map[string]string `json:"headers"`
	BodyHead     string            `json:"body_head"`
	RedirectedTo string            `json:"redirected_to,omitempty"`
}

var allowedMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
}

// keepHeaders is the response subset worth keeping for a probe receipt: the
// identifying set (server, fingerprint helpers), the CORS pair, and the
// click-restriction set. Anything else is noise the bounded output drops.
var keepHeaders = map[string]bool{
	"content-type":                     true,
	"content-length":                   true,
	"server":                           true,
	"date":                             true,
	"x-powered-by":                     true,
	"cf-ray":                           true,
	"location":                         true,
	"set-cookie":                       true,
	"cache-control":                    true,
	"x-frame-options":                  true,
	"content-security-policy":          true,
	"access-control-allow-origin":      true,
	"access-control-allow-credentials": true,
	"etag":                             true,
	"last-modified":                    true,
	"vary":                             true,
}

const setCookiePresence = "present (values redacted)"

// isHeaderToken reports whether a header name is an RFC token. Anything else
// (spaces, colons, control characters) is the shape of a smuggled header.
func isHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}

// filterHeaders returns the kept subset, keyed lowercase. Underscore-prefixed
// names are dropped entirely: they are fingerprint noise some platforms emit
// and no receipt reader wants.
func filterHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for canon, values := range h {
		name := strings.ToLower(canon)
		if len(name) == 0 || name[0] == '_' || !keepHeaders[name] {
			continue
		}
		if name == "set-cookie" {
			// NAMES ONLY. A cookie value is a credential; noting that cookies
			// exist is what the receipt needs.
			out[name] = setCookiePresence
			continue
		}
		out[name] = strings.Join(values, ", ")
	}
	return out
}

// Probe performs one bounded HTTP observation and writes its receipt.
//
// The body is hashed and head-cited, never returned: a probe answers "what did
// it say", not "give me the file".
func Probe(ctx context.Context, r Request) (Result, error) {
	res, _, err := execute(ctx, r, maxBodyBytes, false)
	return res, err
}

// FetchForScan runs the same pipeline as Probe and hands the body back for
// scanning. One engine means one gate, one budget, one receipt and one
// identity: a scan is a sequence of observations, not a second network path.
func FetchForScan(ctx context.Context, rawurl string, bodyCap int) ([]byte, string, Result, error) {
	if bodyCap <= 0 || bodyCap > maxPageBytes {
		bodyCap = maxPageBytes
	}
	if err := Gate(rawurl); err != nil {
		return nil, "", Result{}, err
	}
	if err := Take(1); err != nil {
		return nil, "", Result{}, err
	}
	req := Request{URL: rawurl, Method: http.MethodGet}
	res, body, err := execute(ctx, req, bodyCap, true)
	if err != nil {
		return nil, "", Result{}, err
	}
	return body, res.Headers["content-type"], res, nil
}

// execute is the single request pipeline behind the probe and the scanner:
// validation, transport, identity, bounded read, receipt.
func execute(ctx context.Context, r Request, bodyCap int, keepBody bool) (Result, []byte, error) {
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedMethods[method] {
		// The whitelist exists because arbitrary verbs turn the probe into a
		// mutation engine the scope layer never asked for.
		return Result{}, nil, fmt.Errorf("method %q is not allowed; one of GET, POST, HEAD, OPTIONS, PUT, PATCH, DELETE", method)
	}

	parsed, err := url.Parse(strings.TrimSpace(r.URL))
	if err != nil {
		return Result{}, nil, fmt.Errorf("%w: %v", ErrBadURL, err)
	}
	// Scheme checked here as well as at Gate: the probe is independently
	// callable and must stand on the same floor.
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return Result{}, nil, fmt.Errorf("%w: probe url must be absolute http:// or https://", ErrBadURL)
	}

	for name, value := range r.Headers {
		if !isHeaderToken(name) {
			return Result{}, nil, fmt.Errorf("header %q is not a valid header name", name)
		}
		// A value carrying CRLF is header smuggling, not a header.
		if strings.ContainsAny(value, "\r\n") {
			return Result{}, nil, fmt.Errorf("header %q value contains a line break", name)
		}
	}
	if len(r.Body) > maxRequestBodyBytes {
		return Result{}, nil, fmt.Errorf("body is %dB, over the %dB probe cap", len(r.Body), maxRequestBodyBytes)
	}

	timeout := r.Timeout
	if timeout > maxProbeTimeoutSecs {
		timeout = maxProbeTimeoutSecs
	}
	if timeout <= 0 {
		timeout = defaultProbeTimeoutSecs
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	client := &http.Client{}
	if probeClient != nil {
		client.Transport = probeClient.Transport
		client.Timeout = probeClient.Timeout
	} else {
		// The wall, not the authorisation layer: this holds even when a name
		// changes its answer between validation and dial.
		client.Transport = netguard.Transport()
	}
	// Redirects are never followed. Gate applied to the URL that was asked
	// for; a 3xx targets a host that may be out of scope, and following it
	// silently would move the request past the gate. ErrUseLastResponse
	// makes the transport hand back the 3xx itself.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	var bodyReader io.Reader
	if r.Body != "" {
		bodyReader = strings.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bodyReader)
	if err != nil {
		return Result{}, nil, fmt.Errorf("build request: %w", err)
	}
	for name, value := range r.Headers {
		req.Header.Set(name, value)
	}
	// The engagement's declared identifier outranks anything the caller
	// set. A probe riding an authorized engagement identifies the
	// engagement, not whatever the model found convenient, and the receipt
	// trail must name the same thing the operator declared.
	if eng, ok := Active(); ok && eng.UserAgent != "" {
		req.Header.Set("User-Agent", eng.UserAgent)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return Result{}, nil, fmt.Errorf("probe %s failed: %w", parsed.String(), err)
	}
	defer resp.Body.Close()

	// Bounded read: the receipt cites at most the cap and never warehouses
	// the warehouse.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(bodyCap)+1))
	if err != nil {
		return Result{}, nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > bodyCap {
		body = body[:bodyCap]
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	head := []rune(string(body))
	if len(head) > bodyHeadRunes {
		head = head[:bodyHeadRunes]
	}

	res := Result{
		URL:       parsed.String(),
		Method:    method,
		Status:    resp.StatusCode,
		Headers:   filterHeaders(resp.Header),
		BodyHead:  string(head),
		BodyBytes: len(body),
		SHA256:    digest,
		ElapsedMS: elapsed.Milliseconds(),
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		res.Headers["location"] = resp.Header.Get("Location")
		res.RedirectedTo = resp.Header.Get("Location")
	}
	res.ReceiptID = writeReceipt(time.Now().UTC(), res)
	if keepBody {
		return res, body, nil
	}
	return res, nil, nil
}

// writeReceipt records the observation under DataDir()/recon/receipts with
// restrictive permissions. Receipt failure is a log, never a probe failure:
// the observation the caller asked for lives in the result, the file is
// bookkeeping a half-broken disk must not take away.
func writeReceipt(at time.Time, res Result) string {
	dir := filepath.Join(receiptsBase, "recon", "receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Warn(fmt.Sprintf("recon: receipt not written: %s", err))
		return ""
	}
	name := fmt.Sprintf("%sZ-%s.json", at.UTC().Format("20060102T150405.000000000"), res.SHA256[:12])
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		// Same second and same body hash: the same observation, already cited.
		return name
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		logger.Warn(fmt.Sprintf("recon: receipt not written: %s", err))
		return ""
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(receiptDoc{
		At:           at.Format(time.RFC3339Nano),
		URL:          res.URL,
		Method:       res.Method,
		Status:       res.Status,
		SHA256:       res.SHA256,
		Headers:      res.Headers,
		BodyHead:     res.BodyHead,
		RedirectedTo: res.RedirectedTo,
	}); err != nil {
		logger.Warn(fmt.Sprintf("recon: receipt not written: %s", err))
		return ""
	}
	return name
}

// ---------------------------------------------------------------------------
// RunSpec: the tool-side executor.
// ---------------------------------------------------------------------------

type probeTarget struct {
	url     string
	label   string
	headers map[string]string
}

// clip bounds one echoed value in the summary.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// headerPreview renders up to n subset headers for the compact block.
func headerPreview(h map[string]string, n int) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+clip(h[k], 80))
	}
	return strings.Join(parts, "; ")
}

// corsVerdict states whether the response echoes the requested origin, and
// whether the reflection pairs with allow-credentials. Echoing an origin plus
// credentials is the browser-reachable misconfiguration; it says so in words
// a report can lift. It generates no payloads: this is the observation
// receipt engine, not an attacker.
func corsVerdict(requestedOrigin string, h map[string]string) string {
	acao := h["access-control-allow-origin"]
	if acao == "" || acao != requestedOrigin {
		return ""
	}
	creds := strings.EqualFold(strings.TrimSpace(h["access-control-allow-credentials"]), "true")
	if requestedOrigin == "null" {
		if creds {
			return "note: null origin reflected and allow-credentials true (actionable)"
		}
		return "note: null origin reflected"
	}
	if creds {
		return "REFLECTS with allow-credentials true (actionable)"
	}
	return "REFLECTS without allow-credentials"
}

func resultBlock(t probeTarget, res Result, verdict string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", res.Method, res.URL)
	if t.label != "" && t.label != t.url {
		fmt.Fprintf(&b, " (origin %s)", t.label)
	}
	fmt.Fprintf(&b, "\nstatus %d in %dms, body %dB, sha256:%s\n",
		res.Status, res.ElapsedMS, res.BodyBytes, res.SHA256[:12])
	h := headerPreview(res.Headers, 4)
	if h != "" {
		fmt.Fprintf(&b, "headers %s\n", h)
	}
	if res.RedirectedTo != "" {
		fmt.Fprintf(&b, "redirect %s (not followed)\n", clip(res.RedirectedTo, 160))
	}
	if res.ReceiptID != "" {
		fmt.Fprintf(&b, "receipt %s\n", res.ReceiptID)
	}
	if verdict != "" {
		fmt.Fprintf(&b, "cors: %s\n", verdict)
	}
	return strings.TrimRight(b.String(), "\n")
}

func refusalBlock(t probeTarget, err error) string {
	return fmt.Sprintf("%s\nrefused, not dialed: %v", t.url, err)
}

// maxBlocks is the summary bound. One block per request and no more.
const maxBlocks = 30

// RunSpec is the tool-side executor. Before EVERY request it applies
// Gate (scope) and Take (rate budget), so a batch of 30 paths is 30
// individually gated requests, not one call that got authorisation free on
// the first hop.
func RunSpec(ctx context.Context, args map[string]any) (string, error) {
	rawURL, _ := args["url"].(string)
	if strings.TrimSpace(rawURL) == "" {
		return "", fmt.Errorf("url is required")
	}

	mode := "single"
	switch m := strings.TrimSpace(asString(args["mode"])); m {
	case "", "single":
	case "path_list", "origin_variants":
		mode = m
	default:
		return "", fmt.Errorf("mode %q is not one of single, path_list, origin_variants", m)
	}

	headers, err := headerArgs(args)
	if err != nil {
		return "", err
	}
	timeout, err := timeoutArg(args)
	if err != nil {
		return "", err
	}
	body, _ := args["body"].(string)

	targets, err := buildTargets(mode, rawURL, args, headers)
	if err != nil {
		return "", err
	}

	var blocks []string
	rateLimited := false
	for _, t := range targets {
		if len(blocks) >= maxBlocks {
			blocks = append(blocks, fmt.Sprintf("omitting further requests: %d block cap reached", maxBlocks))
			break
		}
		// PER-REQUEST gate. The scope must apply to every URL that actually
		// goes out, including each path in a list: a batch is a batch of
		// requests, not one privileged first hop.
		if err := Gate(t.url); err != nil {
			blocks = append(blocks, refusalBlock(t, err))
			continue
		}
		// PER-REQUEST budget. On a rate-limit failure the request is not
		// fired at all: the summary ends by naming the limit rather than
		// silently truncating.
		if err := Take(1); err != nil {
			if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrExhausted) {
				rateLimited = true
				break
			}
			blocks = append(blocks, refusalBlock(t, err))
			continue
		}

		req := Request{
			URL:     t.url,
			Method:  http.MethodGet,
			Headers: t.headers,
			Body:    body,
			Timeout: timeout,
		}
		res, err := Probe(ctx, req)
		if err != nil {
			blocks = append(blocks, fmt.Sprintf("%s\nerror: %v", t.url, err))
			continue
		}
		verdict := ""
		if mode == "origin_variants" {
			verdict = corsVerdict(t.headers["Origin"], res.Headers)
		}
		blocks = append(blocks, resultBlock(t, res, verdict))
	}

	out := strings.Join(blocks, "\n\n")
	if rateLimited {
		out += "\n\nengagement rate limit reached"
	}
	return out, nil
}

// asString reads a string argument without the zero-value trap.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// headerArgs reads args["headers"] in the shapes it can arrive in.
func headerArgs(args map[string]any) (map[string]string, error) {
	switch v := args["headers"].(type) {
	case nil:
		return nil, nil
	case map[string]string:
		return v, nil
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("header %q value is not a string", k)
			}
			out[k] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("headers must be an object of string values")
	}
}

// timeoutArg reads args["timeout"] as seconds.
func timeoutArg(args map[string]any) (int, error) {
	switch v := args["timeout"].(type) {
	case nil:
		return 0, nil
	case int:
		if v < 0 {
			return 0, fmt.Errorf("timeout must not be negative")
		}
		return v, nil
	case float64:
		if v < 0 {
			return 0, fmt.Errorf("timeout must not be negative")
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("timeout must be a number of seconds")
	}
}

// stringSliceArg reads a list argument in both native and JSON-decoded shapes.
func stringSliceArg(args map[string]any, key string, max int) ([]string, error) {
	switch v := args[key].(type) {
	case nil:
		return nil, nil
	case []string:
		if len(v) > max {
			return nil, fmt.Errorf("%s exceeded: %d given, maximum %d", key, len(v), max)
		}
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) > max {
			return nil, fmt.Errorf("%s exceeded: %d given, maximum %d", key, len(out), max)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
}

// buildTargets turns the mode and its arguments into the list of requests to
// fire, in order. Bounded: <=30 paths, <=12 origins plus the one null probe.
func buildTargets(mode, rawURL string, args map[string]any, headers map[string]string) ([]probeTarget, error) {
	base := strings.TrimSuffix(strings.TrimSpace(rawURL), "/")
	switch mode {
	case "single":
		return []probeTarget{{url: strings.TrimSpace(rawURL), headers: headers}}, nil

	case "path_list":
		list, err := stringSliceArg(args, "paths", 30)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("mode path_list needs paths")
		}
		targets := make([]probeTarget, 0, len(list))
		for _, p := range list {
			p = strings.TrimSpace(p)
			// A path is relative; a scheme inside it is a second request
			// hiding in a list item and the gate only sees the one unless it
			// is caught here.
			if p == "" || strings.Contains(p, "://") || strings.ContainsAny(p, " \t\r\n") {
				return nil, fmt.Errorf("path %q is not a relative path", p)
			}
			full := base + "/" + strings.TrimLeft(p, "/")
			h := make(map[string]string, len(headers)+1)
			for k, v := range headers {
				h[k] = v
			}
			targets = append(targets, probeTarget{url: full, label: p, headers: h})
		}
		return targets, nil

	case "origin_variants":
		origins, err := stringSliceArg(args, "origins", 12)
		if err != nil {
			return nil, err
		}
		targets := make([]probeTarget, 0, len(origins)+1)
		for _, o := range origins {
			o = strings.TrimSpace(o)
			if o == "" || strings.ContainsAny(o, " \t\r\n") {
				return nil, fmt.Errorf("origin %q is not an origin", o)
			}
			h := make(map[string]string, len(headers)+1)
			for k, v := range headers {
				h[k] = v
			}
			// Ordinary GET with Origin set. The preflight OPTIONS is skipped
			// on purpose: an ordinary fetch is what a cross-origin page
			// actually sends first, and the reflection test is about that
			// response, not the negotiation.
			h["Origin"] = o
			targets = append(targets, probeTarget{url: base, label: o, headers: h})
		}
		// The null-origin probe: sandboxed frames send Origin: null, and a
		// server that reflects null specifically is a finding of its own.
		h := make(map[string]string, len(headers)+1)
		for k, v := range headers {
			h[k] = v
		}
		h["Origin"] = "null"
		targets = append(targets, probeTarget{url: base, label: "null", headers: h})
		return targets, nil
	}
	return nil, fmt.Errorf("unknown mode")
}

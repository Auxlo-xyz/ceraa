package scan

// fuzz.go is parameter fuzzing: the same request repeated with one value
// changed, so the difference between the answers is the signal.
//
// Two ways of reading a difference, and the tool has to keep them apart:
//
//   - A marker. The caller knows what a hit looks like ("stack trace",
//     "SQLSTATE", "session expired") and the body is searched for it. A
//     marker hit is evidence.
//   - No marker. Then the only signal is the baseline: one request with a
//     value nobody would send, and any value whose status or size differs
//     from it. That is a DELTA, not a confirmed hit. A size or status
//     difference is a lead worth probing by hand, and calling it a finding
//     would put a guess in the report.
//
// Every request is gated, budgeted and receipted, exactly like a sweep: the
// budget is metered per request, and a fuzzer that sends two hundred
// requests is two hundred charged ones.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"ceraa/harness/recon"
)

const (
	defaultFuzzRequests    = 200
	defaultFuzzConcurrency = 8
	defaultFuzzTimeout     = 15
	hardFuzzRequests       = 5000
	hardFuzzConcurrency    = 32
	maxFuzzValues          = 2000
	// fuzzBodyCap bounds the body the marker search reads. A marker lives
	// near the top of a response, and the read is bounded so a large page
	// cannot become a large allocation.
	fuzzBodyCap = 128 * 1024
)

// FuzzOptions bounds one fuzzing run.
type FuzzOptions struct {
	// URL is the full URL to fuzz. It may already carry a query string.
	URL string
	// Method is GET (query) or POST (body). Default GET.
	Method string
	// Param is the parameter name to fuzz.
	Param string
	// Wordlist is a path to a wordlist file. Empty uses the embedded list.
	Wordlist string
	// Location is "query", "body" or "header". Empty derives it from the
	// method.
	Location string
	// Header is the header whose value is fuzzed, when the location is
	// header.
	Header string
	// Marker is a substring that marks a hit. Empty means the run reports
	// status and size deltas against a baseline instead.
	Marker string
	// Concurrency is the number of parallel requests.
	Concurrency int
	// MaxRequests is the hard ceiling on requests fired.
	MaxRequests int
	// Timeout is the per-request timeout in seconds.
	Timeout int
	// UserAgent overrides the default identity.
	UserAgent string
	// Body is the body template for a POST run. {PARAM} is replaced by the
	// fuzz value; a template without the placeholder gets the parameter
	// added as a form field.
	Body string
	// Probe is the request seam. Nil uses the real probe.
	Probe CaptureFunc
}

// FuzzHit is one value that answered differently.
type FuzzHit struct {
	Value  string
	Status int
	Size   int
	// Marker is true when the marker was found in the body.
	Marker bool
	// Delta is true when the status or size differed from the baseline.
	// A delta is a difference, not a confirmed finding.
	Delta     bool
	SHA256    string
	ReceiptID string
}

// FuzzResult is a run plus the counters a report needs.
type FuzzResult struct {
	Hits     []FuzzHit
	Requests int
	// Values is how many values were in the list.
	Values int
	// HasBaseline is true when the baseline request was fired, which it is
	// only when no marker was given.
	HasBaseline    bool
	BaselineValue  string
	BaselineStatus int
	BaselineSize   int
	// Refused counts requests the scope gate refused before anything was
	// dialed.
	Refused int
	// Errors holds the first few request errors, bounded.
	Errors []string
	// Stopped names why the run ended early, empty when it finished.
	Stopped string
}

// Fuzz fuzzes one parameter and returns the values that answered
// differently, ordered by value.
func Fuzz(ctx context.Context, opts FuzzOptions) ([]FuzzHit, error) {
	result, err := FuzzDetailed(ctx, opts)
	if err != nil {
		return nil, err
	}
	return result.Hits, nil
}

// FuzzDetailed runs the fuzz and returns the hits with the counters.
func FuzzDetailed(ctx context.Context, opts FuzzOptions) (FuzzResult, error) {
	target, err := url.Parse(strings.TrimSpace(opts.URL))
	if err != nil {
		return FuzzResult{}, fmt.Errorf("url %q is not a valid url: %w", opts.URL, err)
	}
	scheme := strings.ToLower(target.Scheme)
	if (scheme != "http" && scheme != "https") || target.Host == "" {
		return FuzzResult{}, fmt.Errorf("url %q must be an absolute http:// or https:// url", opts.URL)
	}
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		// The verbs a fuzzer needs: a query, a body, and the update verbs
		// that a parameter test is often actually about. HEAD and OPTIONS
		// carry no body, so they fuzz the query only.
	default:
		return FuzzResult{}, fmt.Errorf("method %q is not one of GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS", method)
	}
	param := strings.TrimSpace(opts.Param)
	if param == "" {
		return FuzzResult{}, fmt.Errorf("param is required")
	}
	if strings.ContainsAny(param, " \t\r\n=&#?") {
		return FuzzResult{}, fmt.Errorf("param %q must be a plain parameter name", opts.Param)
	}
	location := strings.ToLower(strings.TrimSpace(opts.Location))
	switch location {
	case "":
		// The method decides by default: GET fuzzes the query, the verbs
		// that carry a body fuzz the body. An explicit location overrides
		// that.
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			location = "body"
		default:
			location = "query"
		}
	case "query", "body", "header":
	default:
		return FuzzResult{}, fmt.Errorf("location %q is not one of query, body, header", opts.Location)
	}
	if location == "body" && method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
		return FuzzResult{}, fmt.Errorf("body fuzzing needs POST, PUT or PATCH, not %s", method)
	}
	if location == "header" {
		// A header fuzz replaces the value of one header, which is how a
		// host header, a referer or an authorization scheme gets tested.
		if strings.TrimSpace(opts.Header) == "" {
			return FuzzResult{}, fmt.Errorf("location header needs a header name")
		}
	}
	if location == "query" && strings.TrimSpace(opts.Body) != "" {
		// The template would be silently ignored, and a fuzzer that quietly
		// does not fuzz what the caller asked for is worse than one that
		// refuses.
		return FuzzResult{}, fmt.Errorf("a body template was given but the location is query; set location to body")
	}

	values, err := LoadWordlist(opts.Wordlist)
	if err != nil {
		return FuzzResult{}, err
	}
	if len(values) > maxFuzzValues {
		values = values[:maxFuzzValues]
	}
	if len(values) == 0 {
		return FuzzResult{}, fmt.Errorf("the wordlist holds no values to fuzz")
	}

	maxRequests := opts.MaxRequests
	if maxRequests <= 0 {
		maxRequests = defaultFuzzRequests
	}
	if maxRequests > hardFuzzRequests {
		maxRequests = hardFuzzRequests
	}
	capture := opts.Probe
	if capture == nil {
		capture = defaultCapture
	}
	marker := strings.ToLower(strings.TrimSpace(opts.Marker))
	headers := map[string]string{}
	if agent := strings.TrimSpace(opts.UserAgent); agent != "" {
		headers["User-Agent"] = agent
	}

	state := &fuzzState{max: maxRequests}
	result := FuzzResult{Values: len(values)}

	if marker == "" {
		// The baseline: one request with a value nobody would send. Every
		// difference is measured against it.
		baselineValue := randomFuzzValue()
		baselineReq, err := buildQueryOrBody(target, method, location, param, opts, baselineValue, headers)
		if err != nil {
			return FuzzResult{}, err
		}
		baseline, _, fired, err := state.fire(ctx, capture, baselineReq)
		if err != nil {
			return FuzzResult{}, err
		}
		if !fired {
			return FuzzResult{}, fmt.Errorf("the baseline request was not sent: %s", state.stoppedReason())
		}
		result.HasBaseline = true
		result.BaselineValue = baselineValue
		result.BaselineStatus = baseline.Status
		result.BaselineSize = baseline.Size
	}

	runFuzzValues(ctx, state, target, method, location, param, opts, values, headers, capture, marker, result.BaselineStatus, result.BaselineSize)

	hits := state.hitsSnapshot()
	sort.Slice(hits, func(i, j int) bool { return hits[i].Value < hits[j].Value })
	result.Hits = hits
	result.Requests = state.requestCount()
	result.Refused = state.refusedCount()
	result.Errors = state.errorSnapshot()
	result.Stopped = state.stoppedReason()
	return result, nil
}

// defaultCapture is the real request seam: a bounded, receipted probe that
// hands the body back.
func defaultCapture(ctx context.Context, req recon.Request, bodyCap int) (recon.Result, []byte, error) {
	return recon.ProbeCapture(ctx, req, bodyCap)
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

// buildQueryOrBody builds one request for one value.
//
// GET puts the value in the query string, replacing an existing parameter of
// the same name. POST puts it in the body: the template's {PARAM} is
// replaced, and a template without the placeholder gets the parameter added
// as a form field.
func buildQueryOrBody(target *url.URL, method, location, param string, opts FuzzOptions, value string, headers map[string]string) (recon.Request, error) {
	if location == "header" {
		reqHeaders := make(map[string]string, len(headers)+1)
		for name, header := range headers {
			reqHeaders[name] = header
		}
		reqHeaders[opts.Header] = value
		return recon.Request{URL: target.String(), Method: method, Headers: reqHeaders, Timeout: opts.Timeout}, nil
	}
	if location == "query" {
		clone := *target
		query := clone.Query()
		// Set replaces every existing value of that name, which is what
		// "replace the existing parameter" means.
		query.Set(param, value)
		clone.RawQuery = query.Encode()
		return recon.Request{URL: clone.String(), Method: method, Headers: headers, Timeout: opts.Timeout}, nil
	}
	body, contentType, err := buildFuzzBody(opts.Body, param, value)
	if err != nil {
		return recon.Request{}, err
	}
	reqHeaders := make(map[string]string, len(headers)+1)
	for name, header := range headers {
		reqHeaders[name] = header
	}
	reqHeaders["Content-Type"] = contentType
	return recon.Request{URL: target.String(), Method: method, Headers: reqHeaders, Body: body, Timeout: opts.Timeout}, nil
}

// buildFuzzBody builds the request body for one value.
func buildFuzzBody(template, param, value string) (string, string, error) {
	trimmed := strings.TrimSpace(template)
	if strings.Contains(trimmed, "{PARAM}") {
		if looksLikeJSON(trimmed) {
			return strings.ReplaceAll(trimmed, "{PARAM}", jsonString(value)), "application/json", nil
		}
		return strings.ReplaceAll(trimmed, "{PARAM}", url.QueryEscape(value)), "application/x-www-form-urlencoded", nil
	}
	if trimmed == "" {
		return param + "=" + url.QueryEscape(value), "application/x-www-form-urlencoded", nil
	}
	if looksLikeJSON(trimmed) {
		return "", "", fmt.Errorf("body template has no {PARAM} placeholder; add one or send a form body")
	}
	return trimmed + "&" + param + "=" + url.QueryEscape(value), "application/x-www-form-urlencoded", nil
}

// looksLikeJSON reports whether a body template is JSON.
func looksLikeJSON(body string) bool {
	trimmed := strings.TrimSpace(body)
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// jsonString escapes a value for use inside a JSON string.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	return string(encoded[1 : len(encoded)-1])
}

// randomFuzzValue returns a value nobody would send, for the baseline.
func randomFuzzValue() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "zzq7x-not-a-real-value"
	}
	return "zzq7x" + hex.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// The sweep
// ---------------------------------------------------------------------------

// fuzzState is the accumulator of a fuzzing run.
type fuzzState struct {
	mu       sync.Mutex
	hits     []FuzzHit
	requests int
	refused  int
	errors   []string
	stopped  string
	max      int
}

// fuzzOutcome is what one request produced.
type fuzzOutcome struct {
	Status    int
	Size      int
	SHA256    string
	ReceiptID string
}

// fire performs one gated, budgeted, receipted request and hands back the
// result with the captured body. The bool says whether the request was
// actually fired: a refused or skipped request has no answer to compare.
func (s *fuzzState) fire(ctx context.Context, capture CaptureFunc, req recon.Request) (fuzzOutcome, []byte, bool, error) {
	if err := recon.Gate(req.URL); err != nil {
		s.mu.Lock()
		s.refused++
		s.addErrorLocked(fmt.Sprintf("%s refused by scope: %v", req.URL, err))
		s.mu.Unlock()
		return fuzzOutcome{}, nil, false, nil
	}
	if err := recon.Take(1); err != nil {
		s.setStopped(err.Error())
		return fuzzOutcome{}, nil, false, nil
	}
	s.mu.Lock()
	if s.requests >= s.max {
		s.mu.Unlock()
		s.setStopped("request ceiling reached")
		return fuzzOutcome{}, nil, false, nil
	}
	s.requests++
	s.mu.Unlock()

	res, body, err := capture(ctx, req, fuzzBodyCap)
	if err != nil {
		s.mu.Lock()
		s.addErrorLocked(fmt.Sprintf("%s: %v", req.URL, err))
		s.mu.Unlock()
		return fuzzOutcome{}, nil, false, err
	}
	return fuzzOutcome{Status: res.Status, Size: res.BodyBytes, SHA256: res.SHA256, ReceiptID: res.ReceiptID}, body, true, nil
}

// runFuzzValues fires the whole value list through a pool of workers.
func runFuzzValues(ctx context.Context, state *fuzzState, target *url.URL, method, location, param string, opts FuzzOptions, values []string, headers map[string]string, capture CaptureFunc, marker string, baselineStatus, baselineSize int) {
	workers := opts.Concurrency
	if workers <= 0 {
		workers = defaultFuzzConcurrency
	}
	if workers > hardFuzzConcurrency {
		workers = hardFuzzConcurrency
	}
	if workers > len(values) {
		workers = len(values)
	}
	if workers <= 0 {
		return
	}
	queue := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for value := range queue {
				if ctx.Err() != nil {
					return
				}
				state.probeValue(ctx, capture, target, method, location, param, opts, value, headers, marker, baselineStatus, baselineSize)
			}
		}()
	}
	for _, value := range values {
		if ctx.Err() != nil {
			state.setStopped("context cancelled")
			break
		}
		if state.isStopped() {
			break
		}
		select {
		case queue <- value:
		case <-ctx.Done():
		}
	}
	close(queue)
	wg.Wait()
}

// probeValue fires one value and records it when it answers differently.
func (s *fuzzState) probeValue(ctx context.Context, capture CaptureFunc, target *url.URL, method, location, param string, opts FuzzOptions, value string, headers map[string]string, marker string, baselineStatus, baselineSize int) {
	req, err := buildQueryOrBody(target, method, location, param, opts, value, headers)
	if err != nil {
		s.mu.Lock()
		s.addErrorLocked(err.Error())
		s.mu.Unlock()
		return
	}
	res, body, fired, err := s.fire(ctx, capture, req)
	if err != nil || !fired {
		return
	}
	hit := FuzzHit{
		Value:     value,
		Status:    res.Status,
		Size:      res.Size,
		SHA256:    res.SHA256,
		ReceiptID: res.ReceiptID,
	}
	if marker != "" {
		hit.Marker = strings.Contains(strings.ToLower(string(body)), marker)
	} else {
		hit.Delta = res.Status != baselineStatus || res.Size != baselineSize
	}
	if !hit.Marker && !hit.Delta {
		return
	}
	s.mu.Lock()
	s.hits = append(s.hits, hit)
	s.mu.Unlock()
}

// addErrorLocked records a bounded number of errors.
func (s *fuzzState) addErrorLocked(message string) {
	const maxErrors = 10
	if len(s.errors) >= maxErrors {
		return
	}
	s.errors = append(s.errors, message)
}

func (s *fuzzState) hitsSnapshot() []FuzzHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FuzzHit, len(s.hits))
	copy(out, s.hits)
	return out
}

func (s *fuzzState) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *fuzzState) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func (s *fuzzState) errorSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.errors))
	copy(out, s.errors)
	return out
}

func (s *fuzzState) setStopped(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped == "" {
		s.stopped = reason
	}
}

func (s *fuzzState) stoppedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *fuzzState) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped != ""
}

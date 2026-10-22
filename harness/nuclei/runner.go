package nuclei

// runner.go runs a set of templates against a target.
//
// Every request goes through the same three steps a probe goes through:
// recon.Gate decides whether the destination is authorized, recon.Take
// charges the engagement budget, and the probe writes the receipt. A
// template engine that skipped any of them would be a second network path
// with its own idea of what the engagement covers, which is exactly the
// thing the recon layer exists to prevent.
//
// A finding carries the matcher that fired, an excerpt of what it matched
// and the receipt of the request that produced it. A finding that only says
// "matched" is a finding the reader has to re-derive, and a finding they
// cannot re-derive is a finding they will not report.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"ceraa/harness/recon"
)

// The bounds. A caller may ask for less; nobody may ask for more.
const (
	defaultConcurrency = 4
	hardConcurrency    = 32
	defaultMaxRequests = 200
	hardMaxRequests    = 5000
	defaultMaxFindings = 50
	hardMaxFindings    = 500
	defaultTimeout     = 15
	// bodyCap bounds the body a matcher reads. A marker lives near the top
	// of a response and the read is bounded so a large page cannot become
	// a large allocation.
	bodyCap = 256 * 1024
	// maxTargets bounds a target list.
	maxTargets = 50
	// maxErrorLines bounds the errors a run reports.
	maxErrorLines = 20
	// maxSkipReasons bounds the skip reasons a run reports.
	maxSkipReasons = 20
	// maxHeaderLines bounds the headers a recorded request carries.
	maxHeaderLines = 12
)

// Options bounds one run.
type Options struct {
	// Target is one target url, or a file of targets, one url per line.
	Target string
	// Templates are the templates to run.
	Templates []Template
	// Concurrency is the number of parallel requests.
	Concurrency int
	// RateLimit is the requests per second, 0 is uncapped.
	RateLimit int
	// Timeout is the per-request timeout in seconds.
	Timeout int
	// MaxRequests is the hard ceiling on requests fired.
	MaxRequests int
	// MaxFindings is the hard ceiling on findings.
	MaxFindings int
	// Severities keeps only these severities. Empty keeps all.
	Severities []string
	// Tags keeps only the templates carrying one of these tags. Empty keeps
	// all.
	Tags []string
	// TemplateIDs keeps only these template ids. Empty keeps all.
	TemplateIDs []string
	// Probe is the request seam. Nil uses the real probe.
	Probe ProbeFunc
}

// Finding is one match.
type Finding struct {
	TemplateID string
	Name       string
	Severity   string
	Type       string
	MatchedAt  string
	// Matcher names the matcher that fired and what it looked for.
	Matcher string
	// Evidence is the bounded excerpt around the match.
	Evidence string
	// Request is the request that was sent, with the credential headers
	// redacted.
	Request string
	Status  int
	// BodyBytes is the size of the body that was read.
	BodyBytes int
	SHA256    string
	ReceiptID string
	// Source is the template file the finding came from.
	Source string
	// CatchAll marks a finding whose response body is byte for byte the
	// target root's own body. A single page application answers every
	// path with the same document, so a word matcher fires on the shell
	// and the finding is noise. The note says so, because a reader who has
	// to work that out from a hash is a reader who will not report it.
	CatchAll string
	// EmptyVars names the variables that rendered to nothing. A template
	// with a hole in it still runs, and the finding says where the hole
	// was.
	EmptyVars []string
}

// Report is a run plus the counters a report needs.
type Report struct {
	// Findings are the matches, ordered by severity and then by template
	// id, so two runs read the same.
	Findings []Finding
	// Templates is how many templates ran.
	Templates int
	// Skipped is how many templates did not run.
	Skipped int
	// SkipReasons names why, bounded.
	SkipReasons []string
	// Requests is how many requests went out.
	Requests int
	// Refused counts the requests the scope gate refused before anything
	// was dialed.
	Refused int
	// Errors holds the per-template failures, bounded.
	Errors []string
	// Stopped names why the run ended early, empty when it finished.
	Stopped string
}

// ProbeFunc is the request seam.
//
// The default is the real probe: gated, budgeted, receipted. A test injects
// a canned answer so the suite exercises the runner without touching the
// network or the operator's receipts directory.
type ProbeFunc func(ctx context.Context, req recon.Request) (recon.Result, []byte, error)

// defaultProbe is the real request seam: a bounded, receipted probe that
// hands the body back, because a matcher needs the body to match against.
func defaultProbe(ctx context.Context, req recon.Request) (recon.Result, []byte, error) {
	return recon.ProbeCapture(ctx, req, bodyCap)
}

// Run runs the templates and returns the findings.
func Run(ctx context.Context, opts Options) ([]Finding, error) {
	report, err := RunDetailed(ctx, opts)
	if err != nil {
		return nil, err
	}
	return report.Findings, nil
}

// RunDetailed runs the templates and returns the findings with the counters.
func RunDetailed(ctx context.Context, opts Options) (Report, error) {
	targets, err := resolveTargets(opts.Target)
	if err != nil {
		return Report{}, err
	}
	probe := opts.Probe
	if probe == nil {
		probe = defaultProbe
	}

	kept, skipped, reasons := selectTemplates(opts.Templates, opts.Severities, opts.Tags, opts.TemplateIDs)
	report := Report{Templates: len(kept), Skipped: skipped, SkipReasons: reasons}
	if len(kept) == 0 {
		return report, nil
	}

	maxRequests := opts.MaxRequests
	if maxRequests <= 0 {
		maxRequests = defaultMaxRequests
	}
	if maxRequests > hardMaxRequests {
		maxRequests = hardMaxRequests
	}
	maxFindings := opts.MaxFindings
	if maxFindings <= 0 {
		maxFindings = defaultMaxFindings
	}
	if maxFindings > hardMaxFindings {
		maxFindings = hardMaxFindings
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	state := &runState{
		maxRequests: maxRequests,
		maxFindings: maxFindings,
		outcomes:    map[int]*templateOutcome{},
		stopSignal:  make(chan struct{}),
	}
	limiter := newLimiter(opts.RateLimit)
	defer limiter.close()

	// The root is read once before the sweep, so a catch-all response can
	// be named rather than left for the reader to infer from a hash.
	if len(targets) > 0 {
		fetchRootHash(ctx, targets[0].String(), probe, limiter, timeout, state)
	}

	items := buildWork(kept, targets)
	queue := make(chan workItem)
	var wg sync.WaitGroup
	workers := opts.Concurrency
	if workers <= 0 {
		workers = defaultConcurrency
	}
	if workers > hardConcurrency {
		workers = hardConcurrency
	}
	if workers > len(items) {
		workers = len(items)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				if ctx.Err() != nil {
					return
				}
				if state.isStopped() {
					return
				}
				state.process(ctx, item, probe, limiter, timeout)
			}
		}()
	}
	for _, item := range items {
		select {
		case queue <- item:
		case <-ctx.Done():
			state.setStopped("the run was cancelled")
		case <-state.stopSignal:
			// The run was told to stop, so there is nobody left to feed.
		}
		if state.isStopped() {
			break
		}
	}
	close(queue)
	wg.Wait()

	report.Findings = state.findingsSnapshot()
	report.Requests = state.requestCount()
	report.Refused = state.refusedCount()
	report.Errors = state.errorSnapshot()
	report.Errors = append(report.Errors, state.failedTemplates(kept)...)
	if len(report.Errors) > maxErrorLines {
		report.Errors = report.Errors[:maxErrorLines]
	}
	report.Stopped = state.stoppedReason()
	sortFindings(report.Findings)
	return report, nil
}

// ---------------------------------------------------------------------------
// Template selection
// ---------------------------------------------------------------------------

// selectTemplates applies the filters and the support checks.
//
// The skip reasons are the point: a template that did not run has to say
// why, because a run that silently drops half its corpus looks exactly
// like a run that found nothing.
func selectTemplates(templates []Template, severities, tags, ids []string) ([]Template, int, []string) {
	var (
		kept    []Template
		skipped int
		reasons []string
	)
	severitySet := lowerSet(severities)
	tagSet := lowerSet(tags)
	idSet := lowerSet(ids)

	for _, tpl := range templates {
		if reason, ok := excluded(tpl, severitySet, tagSet, idSet); !ok {
			skipped++
			reasons = appendReason(reasons, tpl.ID+": "+reason)
			continue
		}
		if len(tpl.HTTP) == 0 {
			skipped++
			reasons = appendReason(reasons, tpl.ID+": the template has no http request")
			continue
		}
		if reason := unsupportedTemplate(tpl); reason != "" {
			skipped++
			reasons = appendReason(reasons, tpl.ID+": "+reason)
			continue
		}
		kept = append(kept, tpl)
	}
	return kept, skipped, reasons
}

// excluded reports why a template is filtered out, and whether it is.
func excluded(tpl Template, severities, tags, ids map[string]bool) (string, bool) {
	if len(severities) > 0 && !severities[strings.ToLower(strings.TrimSpace(tpl.Info.Severity))] {
		return "the severity filter does not keep it", false
	}
	if len(tags) > 0 {
		matched := false
		for _, tag := range tpl.Info.Tags {
			if tags[strings.ToLower(strings.TrimSpace(tag))] {
				matched = true
				break
			}
		}
		if !matched {
			return "the tag filter does not keep it", false
		}
	}
	if len(ids) > 0 && !ids[strings.ToLower(strings.TrimSpace(tpl.ID))] {
		return "the template id filter does not keep it", false
	}
	return "", true
}

// unsupportedTemplate names why a template cannot be executed here.
func unsupportedTemplate(tpl Template) string {
	for _, request := range tpl.HTTP {
		if len(request.Matchers) == 0 && len(request.Extractors) == 0 && request.Cluster == nil {
			return "the template has a request with no matchers"
		}
		if reason := UnsupportedReason(request.Matchers); reason != "" {
			return reason
		}
	}
	return ""
}

// lowerSet turns a list into a lookup set.
func lowerSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		out[trimmed] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// appendReason keeps the reason list bounded.
func appendReason(reasons []string, reason string) []string {
	if len(reasons) >= maxSkipReasons {
		return reasons
	}
	return append(reasons, reason)
}

// ---------------------------------------------------------------------------
// The work
// ---------------------------------------------------------------------------

// workItem is one template against one target.
//
// The requests of a template run in order, because a template that extracts
// a value from the first response and sends it in the second needs the
// first to have happened. The concurrency is across templates and targets
// instead.
type workItem struct {
	target   *url.URL
	template Template
	// index is the position of the template in the kept list, so the
	// per-template counters do not collide on a duplicate id.
	index int
}

// buildWork flattens the templates and the targets into the runs to fire.
func buildWork(templates []Template, targets []*url.URL) []workItem {
	var items []workItem
	for _, target := range targets {
		for index, tpl := range templates {
			items = append(items, workItem{target: target, template: tpl, index: index})
		}
	}
	return items
}

// templateOutcome counts what happened to one template against one target.
type templateOutcome struct {
	attempts int
	failures int
	lastErr  string
}

// runState is the accumulator of a run.
type runState struct {
	mu sync.Mutex
	// rootSHA is the hash of the target root's own body, fetched once
	// before the sweep. Empty when the root could not be read, and then
	// no finding is ever marked as a catch-all.
	rootSHA     string
	rootURL     string
	findings    []Finding
	errors      []string
	requests    int
	refused     int
	stopped     string
	maxRequests int
	maxFindings int
	outcomes    map[int]*templateOutcome
	// stopSignal is closed when the run is told to stop. The feed loop
	// waits on it: without it a run that stops early leaves the loop
	// waiting for a worker that has already gone home.
	stopSignal chan struct{}
	stopOnce   sync.Once
}

// process runs one template against one target.
//
// The requests of the template run in order: an extractor in the first
// request feeds the second, and a sweep that fired them side by side would
// have sent the second request with a variable nobody had set yet.
func (s *runState) process(ctx context.Context, item workItem, probe ProbeFunc, limiter *limiter, timeout int) {
	vars := mergedVars(item.target, item.template)
	for _, request := range item.template.HTTP {
		if request.Cluster != nil {
			s.runCluster(ctx, item, request, vars, probe, limiter, timeout)
			continue
		}
		for _, path := range request.Path {
			s.runRequest(ctx, item, request, path, false, vars, probe, limiter, timeout)
		}
		for _, block := range request.Raw {
			s.runRequest(ctx, item, request, block, true, vars, probe, limiter, timeout)
		}
	}
}

// runRequest fires one request of a template and records what it produced.
func (s *runState) runRequest(ctx context.Context, item workItem, request HTTPRequest, path string, raw bool, vars Vars, probe ProbeFunc, limiter *limiter, timeout int) {
	built, missing, err := buildRequest(item, request, path, raw, vars, timeout)
	if err != nil {
		s.addError(fmt.Sprintf("%s: %v", item.template.ID, err))
		return
	}
	result, body, ok := s.fire(ctx, item, built, probe, limiter)
	if !ok {
		return
	}
	response := Response{
		Status:         result.Status,
		Headers:        result.Headers,
		Body:           body,
		URL:            result.URL,
		Method:         built.Method,
		RequestHeaders: built.Headers,
		RequestBody:    built.Body,
	}
	if response.URL == "" {
		response.URL = built.URL
	}
	if len(request.Matchers) > 0 {
		match, err := EvaluateRequestWithVars(request, response, vars)
		if err != nil {
			s.addError(fmt.Sprintf("%s: the matchers could not be evaluated: %v", item.template.ID, err))
			return
		}
		if !match.Matched {
			return
		}
		catchAll := ""
		if s.isCatchAll(result.SHA256, response.URL) {
			catchAll = "the response body is identical to the target root, so a catch-all route answered this path"
		}
		s.addFinding(Finding{
			TemplateID: item.template.ID,
			Name:       item.template.Info.Name,
			Severity:   item.template.Info.Severity,
			Type:       item.template.Type,
			MatchedAt:  response.URL,
			Matcher:    match.Matcher,
			Evidence:   match.Evidence,
			Request:    describeRequest(built.Method, built.URL, built.Headers),
			Status:     result.Status,
			BodyBytes:  result.BodyBytes,
			SHA256:     result.SHA256,
			ReceiptID:  result.ReceiptID,
			Source:     item.template.Path,
			EmptyVars:  missing,
			CatchAll:   catchAll,
		})
	}
	// The extractors run on the responses that matched, and on every
	// response of a request that carries no matchers at all: a request
	// whose only job is to pull a value out has nothing to match.
	s.runExtractors(item, request, response, vars, built, result, missing)
}

// runExtractors runs the extractors of a request and merges what they
// found into the variables the later requests of the template render
// against.
//
// A value that is not internal is also reported: a template that pulled a
// secret out of a response has found something, and a reader who cannot
// see it cannot report it.
func (s *runState) runExtractors(item workItem, request HTTPRequest, response Response, vars Vars, built recon.Request, result recon.Result, missing []string) {
	specs := make([]Extractor, 0, len(item.template.Extractors)+len(request.Extractors))
	specs = append(specs, item.template.Extractors...)
	specs = append(specs, request.Extractors...)
	if len(specs) == 0 {
		return
	}
	values, err := ExtractWithVars(specs, response, vars)
	if err != nil {
		s.addError(fmt.Sprintf("%s: %v", item.template.ID, err))
	}
	for _, spec := range specs {
		name := spec.Name
		if name == "" {
			name = spec.Type
		}
		found := values[name]
		if len(found) == 0 {
			continue
		}
		// The first value is the one a later request sends: a later
		// request asks for one value, and the first is the one the
		// extractor found first.
		vars[name] = found[0]
		if spec.Internal {
			continue
		}
		label := "extractor " + name
		if spec.Type != "" {
			label += " (" + spec.Type + ")"
		}
		s.addFinding(Finding{
			TemplateID: item.template.ID,
			Name:       item.template.Info.Name,
			Severity:   item.template.Info.Severity,
			Type:       item.template.Type,
			MatchedAt:  response.URL,
			Matcher:    label,
			Evidence:   strings.Join(found, ", "),
			Request:    describeRequest(built.Method, built.URL, built.Headers),
			Status:     result.Status,
			BodyBytes:  result.BodyBytes,
			SHA256:     result.SHA256,
			ReceiptID:  result.ReceiptID,
			Source:     item.template.Path,
			EmptyVars:  missing,
		})
	}
}

// runCluster fires a cluster attack and records the payload that differed.
//
// A cluster attack fires one request per payload, so only the first path of
// the request is used: the paths of a cluster request are alternatives, not
// a list to sweep.
func (s *runState) runCluster(ctx context.Context, item workItem, request HTTPRequest, vars Vars, probe ProbeFunc, limiter *limiter, timeout int) {
	if len(request.Path) == 0 {
		s.addError(fmt.Sprintf("%s: the cluster attack has no path to send", item.template.ID))
		return
	}
	headers := make(map[string]string, len(request.Headers))
	for name, value := range request.Headers {
		headers[name] = value
	}
	if len(request.Cookies) > 0 {
		cookie, _ := renderCookies(request.Cookies, vars)
		setCookieHeader(headers, cookie)
	}
	fire := func(ctx context.Context, req recon.Request) (recon.Result, []byte, error) {
		result, body, ok := s.fire(ctx, item, req, probe, limiter)
		if !ok {
			return recon.Result{}, nil, fmt.Errorf("the request was not sent")
		}
		return result, body, nil
	}
	comparison, err := RunCluster(ctx, *request.Cluster, ClusterRequest{
		Method:  request.Method,
		Path:    request.Path[0],
		Headers: headers,
		Body:    request.Body,
		Timeout: timeout,
		Target:  item.target,
	}, vars, fire)
	if err != nil {
		s.addError(fmt.Sprintf("%s: the cluster attack could not run: %v", item.template.ID, err))
		return
	}
	if !comparison.Matched {
		return
	}
	for _, outlier := range comparison.Outliers {
		matchedAt := outlier.URL
		if matchedAt == "" {
			matchedAt = comparison.Baseline.URL
		}
		s.addFinding(Finding{
			TemplateID: item.template.ID,
			Name:       item.template.Info.Name,
			Severity:   item.template.Info.Severity,
			Type:       item.template.Type,
			MatchedAt:  matchedAt,
			Matcher:    "cluster " + comparison.Attribute,
			Evidence:   outlier.Difference,
			Request:    describeRequest(request.Method, matchedAt, headers),
			Status:     outlier.Status,
			SHA256:     outlier.SHA256,
			ReceiptID:  outlier.ReceiptID,
			Source:     item.template.Path,
			EmptyVars:  comparison.Missing,
		})
	}
}

// fire sends one request through the gated, budgeted path.
//
// The scope is the authorization: the request is refused before it is
// dialed, and a refusal is not a finding. The budget is metered per
// request, and on a rate limit failure the request is never dialed.
func (s *runState) fire(ctx context.Context, item workItem, request recon.Request, probe ProbeFunc, limiter *limiter) (recon.Result, []byte, bool) {
	if !s.reserveRequest() {
		s.setStopped("the request ceiling was reached")
		return recon.Result{}, nil, false
	}
	if err := recon.Gate(request.URL); err != nil {
		s.releaseRequest()
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
		s.addError(fmt.Sprintf("%s: %s refused by scope: %v", item.template.ID, request.URL, err))
		return recon.Result{}, nil, false
	}
	if err := recon.Take(1); err != nil {
		s.releaseRequest()
		s.setStopped(err.Error())
		return recon.Result{}, nil, false
	}
	if err := limiter.wait(ctx); err != nil {
		s.releaseRequest()
		s.setStopped("the run was cancelled")
		return recon.Result{}, nil, false
	}
	result, body, err := probe(ctx, request)
	if err != nil {
		s.recordFailure(item, err)
		return recon.Result{}, nil, false
	}
	s.recordSuccess(item)
	return result, body, true
}

// isCatchAll reports whether a response is the target root's own body
// served at some other path.
func (s *runState) isCatchAll(sha string, matchedAt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rootSHA == "" || sha == "" || sha != s.rootSHA {
		return false
	}
	// The root matching itself is not a catch-all: that is the root.
	return matchedAt != s.rootURL
}

// setRoot records the hash of the target root's body.
func (s *runState) setRoot(url string, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rootURL = url
	s.rootSHA = sha
}

// fetchRootHash reads the target root once, so a catch-all can be told
// apart from a page.
//
// It costs one request and it is counted against the ceiling, because a
// request is a request. When it cannot be read the hash stays empty and no
// finding is marked: an honest silence beats a wrong note.
func fetchRootHash(ctx context.Context, target string, probe ProbeFunc, limiter *limiter, timeout int, state *runState) {
	request := recon.Request{URL: target, Method: http.MethodGet, Timeout: timeout}
	if err := recon.Gate(request.URL); err != nil {
		state.addError(fmt.Sprintf("root fetch refused by scope: %v", err))
		return
	}
	if err := recon.Take(1); err != nil {
		state.addError(fmt.Sprintf("root fetch not sent: %v", err))
		return
	}
	if err := limiter.wait(ctx); err != nil {
		return
	}
	if !state.reserveRequest() {
		state.releaseRequest()
		return
	}
	result, _, err := probe(ctx, request)
	if err != nil {
		state.releaseRequest()
		state.addError(fmt.Sprintf("root fetch failed: %v", err))
		return
	}
	state.setRoot(request.URL, result.SHA256)
}

// reserveRequest takes a slot from the request ceiling.
func (s *runState) reserveRequest() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requests >= s.maxRequests {
		return false
	}
	s.requests++
	return true
}

// releaseRequest gives a slot back when the request was not sent.
func (s *runState) releaseRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requests > 0 {
		s.requests--
	}
}

// recordSuccess counts a request that was answered.
func (s *runState) recordSuccess(item workItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome := s.outcomeLocked(item.index)
	outcome.attempts++
}

// recordFailure counts a request that failed and keeps the reason.
func (s *runState) recordFailure(item workItem, err error) {
	s.mu.Lock()
	outcome := s.outcomeLocked(item.index)
	outcome.attempts++
	outcome.failures++
	outcome.lastErr = err.Error()
	s.mu.Unlock()
	s.addError(fmt.Sprintf("%s: %s: %v", item.template.ID, item.template.Info.Name, err))
}

// outcomeLocked returns the counters of one template. The caller holds the
// lock.
func (s *runState) outcomeLocked(index int) *templateOutcome {
	outcome, ok := s.outcomes[index]
	if !ok {
		outcome = &templateOutcome{}
		s.outcomes[index] = outcome
	}
	return outcome
}

// failedTemplates names the templates whose every request failed.
//
// A template that could not reach the target did not find anything, and
// saying so is the difference between a run that found nothing and a run
// that never ran.
func (s *runState) failedTemplates(templates []Template) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for index, outcome := range s.outcomes {
		if outcome.attempts == 0 || outcome.failures < outcome.attempts {
			continue
		}
		name := ""
		if index < len(templates) {
			name = templates[index].ID
		}
		out = append(out, fmt.Sprintf("%s: every request failed: %s", name, outcome.lastErr))
	}
	sort.Strings(out)
	return out
}

// addFinding records a match, and stops the run at the finding ceiling.
func (s *runState) addFinding(finding Finding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.findings) >= s.maxFindings {
		s.stopped = "the finding ceiling was reached"
		return
	}
	s.findings = append(s.findings, finding)
}

// addError records a bounded error.
func (s *runState) addError(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errors) >= maxErrorLines {
		return
	}
	s.errors = append(s.errors, message)
}

// setStopped records why the run ended early, and wakes the feed loop.
func (s *runState) setStopped(reason string) {
	s.mu.Lock()
	if s.stopped == "" {
		s.stopped = reason
	}
	s.mu.Unlock()
	s.signalStop()
}

// signalStop closes the stop channel once.
func (s *runState) signalStop() {
	s.stopOnce.Do(func() { close(s.stopSignal) })
}

// isStopped reports whether the run was told to stop.
func (s *runState) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped != ""
}

// stoppedReason returns why the run ended early.
func (s *runState) stoppedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// findingsSnapshot returns the findings so far.
func (s *runState) findingsSnapshot() []Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Finding, len(s.findings))
	copy(out, s.findings)
	return out
}

// requestCount returns how many requests went out.
func (s *runState) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// refusedCount returns how many requests the scope refused.
func (s *runState) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// errorSnapshot returns the errors so far.
func (s *runState) errorSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.errors))
	copy(out, s.errors)
	return out
}

// ---------------------------------------------------------------------------
// Building the request
// ---------------------------------------------------------------------------

// buildRequest renders one template request against one target.
//
// The second result names the variables that rendered to nothing. A
// template with a hole in it still runs, and the finding has to say where
// the hole was.
func buildRequest(item workItem, request HTTPRequest, path string, raw bool, vars Vars, timeout int) (recon.Request, []string, error) {
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	body := request.Body
	headers := map[string]string{}
	var missing []string

	if raw {
		parsed, err := parseRawRequest(path)
		if err != nil {
			return recon.Request{}, nil, err
		}
		method = parsed.method
		path = parsed.path
		body = parsed.body
		for name, value := range parsed.headers {
			rendered, gaps := Render(value, vars)
			headers[name] = rendered
			missing = append(missing, gaps...)
		}
	}
	for name, value := range request.Headers {
		rendered, gaps := Render(value, vars)
		headers[name] = rendered
		missing = append(missing, gaps...)
	}
	if len(request.Cookies) > 0 {
		cookie, gaps := renderCookies(request.Cookies, vars)
		missing = append(missing, gaps...)
		setCookieHeader(headers, cookie)
	}
	renderedPath, gaps := Render(path, vars)
	missing = append(missing, gaps...)
	renderedBody, gaps := Render(body, vars)
	missing = append(missing, gaps...)

	return recon.Request{
		URL:     joinURL(item.target, renderedPath),
		Method:  method,
		Headers: headers,
		Body:    renderedBody,
		Timeout: timeout,
	}, missing, nil
}

// setCookieHeader merges the cookie block into the Cookie header.
//
// Whichever spelling of the name the template used is the one that is
// kept: two headers that differ only in case are one header to the server,
// and which of the two values survives would otherwise be down to the order
// a map happens to iterate in.
func setCookieHeader(headers map[string]string, cookie string) {
	for name := range headers {
		if !strings.EqualFold(name, "Cookie") {
			continue
		}
		if existing := headers[name]; existing != "" {
			headers[name] = existing + "; " + cookie
		} else {
			headers[name] = cookie
		}
		return
	}
	headers["Cookie"] = cookie
}

// renderCookies renders the cookie block into a Cookie header value.
func renderCookies(cookies map[string]string, vars Vars) (string, []string) {
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	var (
		parts   []string
		missing []string
	)
	for _, name := range names {
		value, gaps := Render(cookies[name], vars)
		missing = append(missing, gaps...)
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; "), missing
}

// mergedVars is the built-in variables with the template's own variables
// layered on.
//
// The built-ins win: they describe the target, and a template that
// redefined BaseURL would be describing a different target than the one
// the operator named.
func mergedVars(target *url.URL, tpl Template) Vars {
	builtIn := TargetVars(target)
	if len(tpl.Variables) == 0 {
		return builtIn
	}
	names := make([]string, 0, len(tpl.Variables))
	for name := range tpl.Variables {
		names = append(names, name)
	}
	sort.Strings(names)

	own := make(map[string]string, len(names))
	for _, name := range names {
		scope := Vars{}
		for key, value := range builtIn {
			scope[key] = value
		}
		for key, value := range own {
			scope[key] = value
		}
		rendered, _ := Render(tpl.Variables[name], scope)
		own[name] = rendered
	}
	out := Vars{}
	for key, value := range own {
		out[key] = value
	}
	for key, value := range builtIn {
		out[key] = value
	}
	return out
}

// joinURL builds the absolute URL for one rendered path.
//
// A path that is already absolute is used as it stands. A path that starts
// with a slash is rooted at the origin, which is what a leading slash means
// in a URL. A relative path is joined onto the target's own path, so a scan
// of https://example.com/app stays inside /app.
func joinURL(target *url.URL, path string) string {
	trimmed := strings.TrimSpace(path)
	origin := originOf(target)
	if trimmed == "" {
		return origin + strings.TrimSuffix(target.Path, "/")
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return trimmed
	}
	if strings.HasPrefix(trimmed, "/") {
		return origin + trimmed
	}
	return origin + strings.TrimSuffix(target.Path, "/") + "/" + trimmed
}

// parsedRaw is a raw request block, read.
type parsedRaw struct {
	method  string
	path    string
	headers map[string]string
	body    string
}

// parseRawRequest reads a raw request block.
//
// Nuclei's raw mode puts a whole request in one string, and it is the only
// place a template sets a method, a header and a body in one breath.
func parseRawRequest(block string) (parsedRaw, error) {
	normalised := strings.ReplaceAll(unescapeRaw(block), "\r\n", "\n")
	head := normalised
	body := ""
	if cut := strings.Index(normalised, "\n\n"); cut >= 0 {
		head = normalised[:cut]
		body = normalised[cut+2:]
	}
	lines := strings.Split(head, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return parsedRaw{}, fmt.Errorf("the raw request block has no request line")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		return parsedRaw{}, fmt.Errorf("the raw request line %q is not a request", strings.TrimSpace(lines[0]))
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return parsedRaw{
		method:  strings.ToUpper(fields[0]),
		path:    fields[1],
		headers: headers,
		body:    body,
	}, nil
}

// unescapeRaw turns the escaped newlines of a one line raw block into real
// ones.
//
// A raw block written as a YAML block scalar already has real newlines,
// and its escapes are content: a JSON body carries "line1\nline2" and the
// backslash belongs in what the server receives. A block written as a
// single line spells its newlines out, so they are put back here.
func unescapeRaw(block string) string {
	if strings.ContainsAny(block, "\r\n") {
		return block
	}
	var out strings.Builder
	out.Grow(len(block))
	for i := 0; i < len(block); i++ {
		if block[i] != '\\' || i+1 >= len(block) {
			out.WriteByte(block[i])
			continue
		}
		i++
		switch block[i] {
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case '\\':
			out.WriteByte('\\')
		default:
			out.WriteByte('\\')
			out.WriteByte(block[i])
		}
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// resolveTargets reads the target list.
//
// The target is either one URL or a file of URLs, one per line. A file is
// how an operator names a list, and the operator's list is not something
// this runner guesses at.
func resolveTargets(target string) ([]*url.URL, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return nil, fmt.Errorf("target is required")
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("target %q is not a valid url", target)
		}
		return []*url.URL{parsed}, nil
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("the target list %s could not be read: %w", trimmed, err)
	}
	var targets []*url.URL
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parsed, err := url.Parse(line)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("the target %q in %s is not a url", line, trimmed)
		}
		targets = append(targets, parsed)
		if len(targets) >= maxTargets {
			break
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("the target list %s holds no urls", trimmed)
	}
	return targets, nil
}

// ---------------------------------------------------------------------------
// The recorded request
// ---------------------------------------------------------------------------

// credentialHeaders are the headers whose values are credentials.
//
// A cookie value and an authorization header are the same kind of thing:
// whoever holds them holds the session. A finding that quotes one has
// defeated the redaction the rest of the harness keeps.
var credentialHeaders = map[string]bool{
	"cookie":              true,
	"set-cookie":          true,
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-auth-token":        true,
}

// describeRequest renders the request that was sent, with the credential
// headers redacted.
func describeRequest(method, url string, headers map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", method, url)
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxHeaderLines {
		names = names[:maxHeaderLines]
	}
	for _, name := range names {
		value := headers[name]
		if credentialHeaders[strings.ToLower(name)] {
			value = "REDACTED"
		} else {
			value = clipRunes(value, 120)
		}
		fmt.Fprintf(&b, "\n%s: %s", name, value)
	}
	return b.String()
}

// clipRunes bounds a value at a character boundary.
func clipRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

// severityRank orders the severities: critical first.
var severityRank = map[string]int{
	"critical": 0,
	"high":     1,
	"medium":   2,
	"low":      3,
	"info":     4,
}

// rank returns the position of a severity, and puts an unknown one last.
func rank(severity string) int {
	if position, ok := severityRank[strings.ToLower(strings.TrimSpace(severity))]; ok {
		return position
	}
	return len(severityRank)
}

// sortFindings orders the findings so two runs read the same.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		if rank(left.Severity) != rank(right.Severity) {
			return rank(left.Severity) < rank(right.Severity)
		}
		if left.TemplateID != right.TemplateID {
			return left.TemplateID < right.TemplateID
		}
		return left.MatchedAt < right.MatchedAt
	})
}

// ---------------------------------------------------------------------------
// The rate limiter
// ---------------------------------------------------------------------------

// limiter paces the requests at a fixed rate.
//
// One token is available immediately and one more arrives every interval,
// so the first request of a run is not delayed by a tick that has not come
// yet, and the rate never exceeds the one that was asked for.
type limiter struct {
	tokens chan struct{}
	stop   chan struct{}
	once   sync.Once
}

// newLimiter starts the pacer. A rate of 0 is uncapped and returns nil,
// which every method here tolerates.
func newLimiter(rate int) *limiter {
	if rate <= 0 {
		return nil
	}
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		interval = time.Millisecond
	}
	l := &limiter{
		tokens: make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
	l.tokens <- struct{}{}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case l.tokens <- struct{}{}:
				default:
				}
			case <-l.stop:
				return
			}
		}
	}()
	return l
}

// wait blocks until a token is free.
func (l *limiter) wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	select {
	case <-l.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close stops the pacer.
func (l *limiter) close() {
	if l == nil {
		return
	}
	l.once.Do(func() { close(l.stop) })
}

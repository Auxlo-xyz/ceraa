package scan

// discover.go is content discovery: a wordlist sweep over a site, with the
// requests going through the same gate, budget and receipt trail as a single
// probe.
//
// The design follows the rules the recon layer already set:
//
//   - Every request is gated, budgeted and receipted individually. A sweep of
//     two hundred paths is two hundred authorized requests, not one
//     privileged first hop with a hundred followers.
//   - Nothing is unbounded. The request ceiling, the result ceiling, the
//     candidate list, the recursion depth and the printed lines all have
//     hard caps, because a wordlist is caller-supplied input.
//   - The soft-404 rule is the difference between a report and a pile of
//     noise. A server that answers every miss with the same 404 page makes
//     every path look like a hit, so a size that repeats across paths is
//     treated as the not-found size and dropped, and the summary says so.
//
// The package holds no mutable state. The request seam is an option field so
// a test can inject a plain probe: harness/scan cannot reach the unexported
// client knob harness/recon keeps for its own suite, and a test that reaches
// the public network is a test that does not run.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"ceraa/harness/recon"
)

// ProbeFunc is the request seam the scanners use.
//
// The default is a real probe: gated, budgeted, receipted. A test injects a
// plain one so the suite exercises the sweep logic without touching the
// network or the operator's receipts directory.
type ProbeFunc func(ctx context.Context, req recon.Request) (recon.Result, error)

// CaptureFunc is ProbeFunc with the bounded body handed back, for the marker
// search a fuzzer needs.
type CaptureFunc func(ctx context.Context, req recon.Request, bodyCap int) (recon.Result, []byte, error)

// The bounds. A caller may ask for less; nobody may ask for more.
const (
	defaultDiscoverRequests    = 200
	defaultDiscoverResults     = 100
	defaultDiscoverConcurrency = 8
	defaultDiscoverTimeout     = 15
	hardDiscoverRequests       = 5000
	hardDiscoverResults        = 1000
	hardDiscoverConcurrency    = 32
	maxDiscoverExtensions      = 16
	maxDiscoverDepth           = 3
	maxCandidatePaths          = 20000
	// maxDirectoryChildren is how many filenames are probed inside a
	// directory that looked like one. The goal is one level of recursion,
	// not a crawler.
	maxDirectoryChildren = 12
	// maxDirectoriesPerLevel bounds how many directories one level expands.
	maxDirectoriesPerLevel = 20
	// soft404Threshold is how many distinct paths must share a size before
	// that size is treated as the not-found size.
	soft404Threshold = 3
)

// directoryProbeNamesText is the fixed set of filenames probed inside a
// directory that looked like a directory.
const directoryProbeNamesText = `
index.php
index.html
index.htm
default.php
config.php
config.json
.env
.htaccess
web.config
composer.json
package.json
README.md
`

// DiscoverOptions bounds one discovery sweep.
type DiscoverOptions struct {
	// BaseURL is the absolute http(s) URL the sweep starts from.
	BaseURL string
	// Paths are explicit relative paths, tried as given: no extension
	// variants are derived from them.
	Paths []string
	// Wordlist is a path to a wordlist file. Empty uses the embedded list.
	Wordlist string
	// Extensions are suffixes appended to each wordlist word.
	Extensions []string
	// Depth is the directory recursion depth. 0 disables recursion.
	Depth int
	// Concurrency is the number of parallel requests.
	Concurrency int
	// MaxRequests is the hard ceiling on requests fired.
	MaxRequests int
	// MaxResults is the hard ceiling on hits returned.
	MaxResults int
	// MatchStatus keeps only these status codes. Empty keeps all.
	MatchStatus []int
	// ExcludeStatus drops these status codes.
	ExcludeStatus []int
	// ExcludeSize drops responses of exactly this size, which is the
	// explicit form of the soft-404 rule.
	ExcludeSize int
	// Timeout is the per-request timeout in seconds.
	Timeout int
	// UserAgent overrides the default identity.
	UserAgent string
	// DetectSoft404 enables the repeated-size rule. It is a pointer
	// because the default has to be ON: a sweep that silently does not
	// detect soft 404s reports a page of false hits, and a plain bool
	// cannot say "unset means on".
	DetectSoft404 *bool
	// Probe is the request seam. Nil uses the real probe.
	Probe ProbeFunc
}

// Hit is one path that answered.
type Hit struct {
	Path        string
	Status      int
	Size        int
	ContentType string
	SHA256      string
	ReceiptID   string
	ElapsedMS   int64
}

// DiscoverResult is a sweep plus the counters a report needs. A sweep that
// only returned hits could not say how much was fired or what was dropped,
// and a report that cannot say that is a report nobody trusts.
type DiscoverResult struct {
	Hits     []Hit
	Requests int
	// Refused counts requests the scope gate refused before anything was
	// dialed.
	Refused int
	// Errors holds the first few request errors, bounded.
	Errors []string
	// ExcludedStatus counts hits dropped by the status filters.
	ExcludedStatus int
	// ExcludedSize counts hits dropped by the explicit size filter.
	ExcludedSize int
	// ExcludedSoft404 counts hits dropped because their size matched the
	// detected not-found size.
	ExcludedSoft404 int
	// Soft404Sizes lists the sizes treated as not-found, empty when none.
	Soft404Sizes []int
	// ExcludedCap counts hits dropped because the result ceiling was
	// reached.
	ExcludedCap int
	// Truncated is true when the sweep stopped early, whatever stopped it.
	Truncated bool
	// Stopped names why the sweep ended early, empty when it finished.
	Stopped string
}

// Discover runs a content-discovery sweep and returns the hits, ordered by
// path so a report reads the same twice.
func Discover(ctx context.Context, opts DiscoverOptions) ([]Hit, error) {
	result, err := DiscoverDetailed(ctx, opts)
	if err != nil {
		return nil, err
	}
	return result.Hits, nil
}

// DiscoverDetailed runs the sweep and returns the hits with the counters.
func DiscoverDetailed(ctx context.Context, opts DiscoverOptions) (DiscoverResult, error) {
	base, err := normalizeBaseURL(opts.BaseURL)
	if err != nil {
		return DiscoverResult{}, err
	}
	words, err := LoadWordlist(opts.Wordlist)
	if err != nil {
		return DiscoverResult{}, err
	}
	candidates, err := buildCandidates(base, opts, words)
	if err != nil {
		return DiscoverResult{}, err
	}
	if opts.Depth < 0 {
		opts.Depth = 0
	}
	if opts.Depth > maxDiscoverDepth {
		opts.Depth = maxDiscoverDepth
	}
	maxRequests := opts.MaxRequests
	if maxRequests <= 0 {
		maxRequests = defaultDiscoverRequests
	}
	if maxRequests > hardDiscoverRequests {
		maxRequests = hardDiscoverRequests
	}
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = defaultDiscoverResults
	}
	if maxResults > hardDiscoverResults {
		maxResults = hardDiscoverResults
	}
	probe := opts.Probe
	if probe == nil {
		probe = defaultProbe
	}

	state := &sweepState{max: maxRequests}
	level := 0
	wave := candidates
	for len(wave) > 0 {
		runWave(ctx, state, wave, opts, probe)
		if ctx.Err() != nil {
			state.setStopped("context cancelled")
			break
		}
		if state.isStopped() {
			break
		}
		if opts.Depth <= level {
			break
		}
		wave = childCandidates(base, state.takeDirs())
		level++
	}

	kept, excluded := applyFilters(state.hitsSnapshot(), opts)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Path < kept[j].Path })

	result := DiscoverResult{
		Hits:            kept,
		Requests:        state.requestCount(),
		Refused:         state.refusedCount(),
		Errors:          state.errorSnapshot(),
		ExcludedStatus:  excluded.byStatus,
		ExcludedSize:    excluded.bySize,
		ExcludedSoft404: excluded.bySoft404,
		Soft404Sizes:    excluded.softSizes,
		Stopped:         state.stoppedReason(),
	}
	result.Truncated = result.Stopped != ""
	if len(kept) > maxResults {
		result.ExcludedCap = len(kept) - maxResults
		result.Hits = kept[:maxResults]
	}
	return result, nil
}

// defaultProbe is the real request seam: a bounded, receipted probe.
func defaultProbe(ctx context.Context, req recon.Request) (recon.Result, error) {
	return recon.Probe(ctx, req)
}

// ---------------------------------------------------------------------------
// Candidates
// ---------------------------------------------------------------------------

// candidate is one path to try: the relative path as it will be reported and
// the absolute url that will be dialled.
type candidate struct {
	path string
	url  string
}

// normalizeBaseURL validates the base url and strips the parts a sweep
// cannot use: a base that carries a query or a fragment would produce urls
// whose path is not the one the caller asked for.
func normalizeBaseURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("base url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("base url %q is not a valid url: %w", trimmed, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("base url %q must be an absolute http:// or https:// url", trimmed)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

// buildCandidates turns the paths, the wordlist and the extensions into the
// list of requests to fire, in a deterministic order.
func buildCandidates(base *url.URL, opts DiscoverOptions, words []string) ([]candidate, error) {
	candidates := make([]candidate, 0, len(words))
	seen := make(map[string]bool, len(words))
	add := func(raw string) error {
		if len(candidates) >= maxCandidatePaths {
			return nil
		}
		rel, err := relativePath(raw)
		if err != nil {
			return err
		}
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		candidates = append(candidates, candidate{path: rel, url: joinPath(base, rel)})
		return nil
	}
	for _, path := range opts.Paths {
		if err := add(path); err != nil {
			return nil, err
		}
	}
	extensions := sanitizeExtensions(opts.Extensions)
	for _, word := range words {
		if len(candidates) >= maxCandidatePaths {
			break
		}
		if err := add(word); err != nil {
			return nil, err
		}
		for _, ext := range extensions {
			if err := add(word + ext); err != nil {
				return nil, err
			}
		}
	}
	return candidates, nil
}

// sanitizeExtensions keeps the suffixes usable as path fragments: no
// whitespace, no separators, and no more than a handful of them.
func sanitizeExtensions(extensions []string) []string {
	out := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		trimmed := strings.TrimSpace(ext)
		if trimmed == "" {
			continue
		}
		if strings.ContainsAny(trimmed, " \t\r\n/?#") {
			continue
		}
		out = append(out, trimmed)
		if len(out) >= maxDiscoverExtensions {
			break
		}
	}
	return out
}

// relativePath validates a path and returns it in the canonical relative
// form: a leading slash, and a trailing slash kept when the caller asked for
// a directory.
//
// A path is relative. A scheme inside it is a second request hiding in a
// list item, whitespace is a request line break, and a traversal sequence
// walks out of the base the gate authorized.
func relativePath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("path %q is empty", raw)
	}
	if strings.ContainsAny(path, " \t\r\n") {
		return "", fmt.Errorf("path %q contains whitespace", raw)
	}
	if strings.ContainsAny(path, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f") {
		return "", fmt.Errorf("path %q contains a control character", raw)
	}
	if strings.Contains(path, "://") || hasSchemePrefix(path) {
		return "", fmt.Errorf("path %q is not a relative path", raw)
	}
	if strings.HasPrefix(path, "//") {
		// A protocol-relative reference is a second target wearing a
		// path's clothes.
		return "", fmt.Errorf("path %q is not a relative path", raw)
	}
	if strings.Contains(path, "\\") {
		return "", fmt.Errorf("path %q contains a backslash", raw)
	}
	if hasTraversal(path) {
		return "", fmt.Errorf("path %q contains a path traversal sequence", raw)
	}
	return "/" + strings.TrimLeft(path, "/"), nil
}

// hasSchemePrefix reports whether a path starts with something shaped like
// a URL scheme.
func hasSchemePrefix(path string) bool {
	for i, c := range path {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return false
			}
		case c == ':':
			return i > 0
		default:
			return false
		}
	}
	return false
}

// hasTraversal reports whether a path walks out of its base, after decoding:
// a percent-encoded %2e%2e is the same sequence with more steps.
func hasTraversal(path string) bool {
	decoded := path
	if unescaped, err := url.PathUnescape(path); err == nil {
		decoded = unescaped
	}
	for _, segment := range strings.Split(strings.ReplaceAll(decoded, "\\", "/"), "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// joinPath builds the absolute url for a relative path.
func joinPath(base *url.URL, rel string) string {
	clone := *base
	clone.Path = base.Path + rel
	clone.RawPath = ""
	return clone.String()
}

// childCandidates builds the next wave: a small set of filenames inside each
// directory that looked like a directory.
func childCandidates(base *url.URL, dirs []string) []candidate {
	if len(dirs) == 0 {
		return nil
	}
	names := normalizeWordlist(strings.Split(directoryProbeNamesText, "\n"))
	if len(names) > maxDirectoryChildren {
		names = names[:maxDirectoryChildren]
	}
	out := make([]candidate, 0, len(dirs)*len(names))
	for _, dir := range dirs {
		if !strings.HasSuffix(dir, "/") {
			dir += "/"
		}
		for _, name := range names {
			rel := dir + name
			out = append(out, candidate{path: rel, url: joinPath(base, rel)})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The sweep
// ---------------------------------------------------------------------------

// sweepState is the accumulator of a sweep. Several workers write to it, so
// every field sits behind the mutex.
type sweepState struct {
	mu       sync.Mutex
	hits     []Hit
	dirs     []string
	requests int
	refused  int
	errors   []string
	stopped  string
	// max is the ceiling on requests fired for the whole run.
	max int
}

// runWave feeds one wave of candidates to a pool of workers.
func runWave(ctx context.Context, state *sweepState, cands []candidate, opts DiscoverOptions, probe ProbeFunc) {
	if len(cands) == 0 {
		return
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = defaultDiscoverConcurrency
	}
	if workers > hardDiscoverConcurrency {
		workers = hardDiscoverConcurrency
	}
	if workers > len(cands) {
		workers = len(cands)
	}
	headers := map[string]string{}
	if agent := strings.TrimSpace(opts.UserAgent); agent != "" {
		headers["User-Agent"] = agent
	}
	queue := make(chan candidate)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range queue {
				if ctx.Err() != nil {
					return
				}
				state.probeOne(ctx, c, headers, opts, probe)
			}
		}()
	}
	for _, c := range cands {
		if ctx.Err() != nil {
			state.setStopped("context cancelled")
			break
		}
		if state.isStopped() {
			break
		}
		select {
		case queue <- c:
		case <-ctx.Done():
		}
	}
	close(queue)
	wg.Wait()
}

// probeOne performs one gated, budgeted, receipted request and records what
// came back.
func (s *sweepState) probeOne(ctx context.Context, c candidate, headers map[string]string, opts DiscoverOptions, probe ProbeFunc) {
	if err := recon.Gate(c.url); err != nil {
		s.mu.Lock()
		s.refused++
		s.addErrorLocked(fmt.Sprintf("%s refused by scope: %v", c.path, err))
		s.mu.Unlock()
		return
	}
	if err := recon.Take(1); err != nil {
		s.setStopped(err.Error())
		return
	}
	// The ceiling is reserved under the lock, so two workers cannot both
	// see room for the last request and fire it twice.
	s.mu.Lock()
	if s.requests >= s.max {
		s.mu.Unlock()
		s.setStopped("request ceiling reached")
		return
	}
	s.requests++
	s.mu.Unlock()

	req := recon.Request{URL: c.url, Method: http.MethodGet, Headers: headers, Timeout: opts.Timeout}
	res, err := probe(ctx, req)
	if err != nil {
		s.mu.Lock()
		s.addErrorLocked(fmt.Sprintf("%s: %v", c.path, err))
		s.mu.Unlock()
		return
	}
	hit := Hit{
		Path:        c.path,
		Status:      res.Status,
		Size:        res.BodyBytes,
		ContentType: res.Headers["content-type"],
		SHA256:      res.SHA256,
		ReceiptID:   res.ReceiptID,
		ElapsedMS:   res.ElapsedMS,
	}
	s.mu.Lock()
	s.hits = append(s.hits, hit)
	if dir, ok := directoryOf(c.path, res, c.url); ok {
		s.dirs = append(s.dirs, dir)
	}
	s.mu.Unlock()
}

// addErrorLocked records a bounded number of errors. A sweep that fails on
// every path would otherwise fill the report with its own failures.
func (s *sweepState) addErrorLocked(message string) {
	const maxErrors = 10
	if len(s.errors) >= maxErrors {
		return
	}
	s.errors = append(s.errors, message)
}

func (s *sweepState) hitsSnapshot() []Hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Hit, len(s.hits))
	copy(out, s.hits)
	return out
}

func (s *sweepState) takeDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs := s.dirs
	s.dirs = nil
	if len(dirs) > maxDirectoriesPerLevel {
		dirs = dirs[:maxDirectoriesPerLevel]
	}
	return dirs
}

func (s *sweepState) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *sweepState) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func (s *sweepState) errorSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.errors))
	copy(out, s.errors)
	return out
}

func (s *sweepState) setStopped(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped == "" {
		s.stopped = reason
	}
}

func (s *sweepState) stoppedReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *sweepState) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped != ""
}

// directoryOf reports whether a response looks like a directory and returns
// the directory to probe inside.
//
// Two shapes count: a path asked for with a trailing slash, and a 3xx that
// points at the same path with a trailing slash added. The second is the
// common one: "/admin" answers 301 to "/admin/".
func directoryOf(path string, res recon.Result, rawurl string) (string, bool) {
	if strings.HasSuffix(path, "/") {
		return path, true
	}
	if res.Status < 300 || res.Status > 399 || res.RedirectedTo == "" {
		return "", false
	}
	base, err := url.Parse(rawurl)
	if err != nil {
		return "", false
	}
	ref, err := url.Parse(res.RedirectedTo)
	if err != nil {
		return "", false
	}
	resolved := base.ResolveReference(ref)
	if resolved.Path == path+"/" {
		return resolved.Path, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

// exclusionCounts is what a filter pass dropped and why.
type exclusionCounts struct {
	byStatus  int
	bySize    int
	bySoft404 int
	softSizes []int
}

// applyFilters keeps the hits that match the status filters, drops the
// explicit size, and then applies the soft-404 rule.
func applyFilters(hits []Hit, opts DiscoverOptions) ([]Hit, exclusionCounts) {
	var counts exclusionCounts
	match := statusSet(opts.MatchStatus)
	exclude := statusSet(opts.ExcludeStatus)

	kept := make([]Hit, 0, len(hits))
	for _, hit := range hits {
		if len(match) > 0 && !match[hit.Status] {
			counts.byStatus++
			continue
		}
		if exclude[hit.Status] {
			counts.byStatus++
			continue
		}
		if opts.ExcludeSize > 0 && hit.Size == opts.ExcludeSize {
			counts.bySize++
			continue
		}
		kept = append(kept, hit)
	}

	if opts.ExcludeSize > 0 || !soft404Enabled(opts) {
		return kept, counts
	}
	soft := soft404Sizes(kept)
	if len(soft) == 0 {
		return kept, counts
	}
	counts.softSizes = sortedSizes(soft)
	final := make([]Hit, 0, len(kept))
	for _, hit := range kept {
		if soft[hit.Size] {
			counts.bySoft404++
			continue
		}
		final = append(final, hit)
	}
	return final, counts
}

// sortedSizes turns a set of sizes into an ordered list, so the summary
// reads the same twice.
func sortedSizes(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for size := range set {
		out = append(out, size)
	}
	sort.Ints(out)
	return out
}

// soft404Enabled reports whether the repeated-size rule runs. The default is
// on: a sweep that cannot tell a soft 404 from a real page reports noise.
func soft404Enabled(opts DiscoverOptions) bool {
	if opts.DetectSoft404 == nil {
		return true
	}
	return *opts.DetectSoft404
}

// soft404Sizes finds the sizes that repeat across distinct paths. A size
// shared by three or more paths is the not-found size: the server answers
// every miss with the same page.
//
// Only failing responses establish the size. Three successful pages of the
// same length are three pages, not a not-found page, and dropping them
// would hide real findings.
func soft404Sizes(hits []Hit) map[int]bool {
	paths := make(map[int]map[string]bool)
	for _, hit := range hits {
		if hit.Status < 400 {
			continue
		}
		set := paths[hit.Size]
		if set == nil {
			set = make(map[string]bool)
			paths[hit.Size] = set
		}
		set[hit.Path] = true
	}
	soft := make(map[int]bool)
	for size, set := range paths {
		if len(set) >= soft404Threshold {
			soft[size] = true
		}
	}
	return soft
}

// statusSet turns a list of status codes into a lookup set.
func statusSet(codes []int) map[int]bool {
	if len(codes) == 0 {
		return nil
	}
	out := make(map[int]bool, len(codes))
	for _, code := range codes {
		out[code] = true
	}
	return out
}

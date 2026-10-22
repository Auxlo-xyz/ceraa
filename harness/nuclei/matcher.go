package nuclei

// matcher.go evaluates the matchers of a Nuclei template against a
// response.
//
// The result says WHICH matcher fired and what it matched. A finding that
// only says "matched" is a finding the reader has to re-derive from scratch,
// and a finding they cannot re-derive is a finding they will not report.
//
// The grouping follows Nuclei: a matcher entry holds one or more words (or
// regexes) and its `condition` says whether all of them or any of them have
// to be there. The list itself is grouped the same way: entries that ask
// for "or" are satisfied by one of them, and the rest all have to match.
// The request-level `matchers-condition` overrides that grouping when the
// template sets it.
//
// The dsl matcher is implemented: the expression is parsed and evaluated
// against the response. A dsl matcher that could not be evaluated is an
// error rather than a miss, because a check that never ran and a check
// that found nothing have to look different in the report.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Matcher is one matcher entry.
type Matcher struct {
	// Type is "word", "regex", "status", "size", "binary", "dsl", "and" or
	// "or".
	Type string
	// Words are the substrings a word matcher looks for.
	Words []string
	// Negative inverts the result of this matcher.
	Negative bool
	// Condition is "and" or "or", and decides how the words of this entry
	// and the entries of the list are grouped.
	Condition string
	// Part is "body", "header", "all" or "url".
	Part string
	// CaseInsensitive makes the match ignore case.
	CaseInsensitive bool
	// Regex are the patterns a regex matcher looks for.
	Regex []string
	// Status are the status codes a status matcher accepts.
	Status []int
	// Size are the body lengths a size matcher accepts.
	Size []int
	// DSL are the expressions of a dsl matcher. They are not evaluated.
	DSL []string
	// Binary are the base64 encoded blobs a binary matcher looks for.
	Binary []string
	// Matchers are the children of an "and" or "or" matcher.
	Matchers []Matcher
}

// Response is what a matcher is evaluated against.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	URL     string
	// Method is the method of the request that produced this response.
	// The dsl matcher reads it, and so does the evidence of a request
	// that has to name what it sent.
	Method string
	// RequestHeaders is the header block of the request that produced
	// this response.
	RequestHeaders map[string]string
	// RequestBody is the body that was sent.
	RequestBody string
}

// evalContext is what a matcher is evaluated against: the response and the
// names the template supplied.
type evalContext struct {
	response Response
	vars     map[string]string
}

// MatchResult says whether the matchers matched, which matcher fired and
// what it matched.
type MatchResult struct {
	// Matched is the verdict.
	Matched bool
	// Matcher names the matcher that fired, with the thing it looked for.
	Matcher string
	// Evidence is the bounded excerpt around the match. It is never the
	// whole body: a body is not evidence, a window around the hit is.
	Evidence string
}

// The bounds. A response is not a haystack to search forever in.
const (
	// maxRegexMatches is how many matches of one pattern are collected.
	// The count is a cap, not a target: the first hit is the evidence and
	// the rest are noise.
	maxRegexMatches = 10
	// evidenceRunes is the width of the excerpt kept around a match.
	evidenceRunes = 200
	// maxCachedRegexes bounds the compiled pattern cache. A corpus of
	// thousands of templates must not grow the cache without limit.
	maxCachedRegexes = 512
)

// Evaluate runs a matcher list against a response.
func Evaluate(matchers []Matcher, resp Response) (MatchResult, error) {
	return EvaluateWithVars(matchers, resp, nil)
}

// EvaluateWithVars runs a matcher list against a response with the names
// the template supplied.
//
// The names are the template's own variables and the values an extractor
// pulled out of an earlier response. A dsl matcher may name them, and a
// dsl matcher that names one nobody set is an error rather than a miss.
func EvaluateWithVars(matchers []Matcher, resp Response, vars map[string]string) (MatchResult, error) {
	return evaluateList(matchers, evalContext{response: resp, vars: vars}, "")
}

// EvaluateRequest runs the matchers of one request, honouring the request
// level condition and the request level case-insensitive default.
func EvaluateRequest(req HTTPRequest, resp Response) (MatchResult, error) {
	return EvaluateRequestWithVars(req, resp, nil)
}

// EvaluateRequestWithVars runs the matchers of one request with the names
// the template supplied.
func EvaluateRequestWithVars(req HTTPRequest, resp Response, vars map[string]string) (MatchResult, error) {
	matchers := req.Matchers
	if req.CaseInsensitive {
		// A copy: the request belongs to the template, and the template is
		// run again for the next target.
		matchers = make([]Matcher, len(req.Matchers))
		copy(matchers, req.Matchers)
		for i := range matchers {
			if !matchers[i].CaseInsensitive {
				matchers[i].CaseInsensitive = true
			}
		}
	}
	return evaluateList(matchers, evalContext{response: resp, vars: vars}, req.MatchersCondition)
}

// evaluateList evaluates the matcher list and combines the entries.
func evaluateList(matchers []Matcher, ctx evalContext, condition string) (MatchResult, error) {
	if len(matchers) == 0 {
		return MatchResult{}, fmt.Errorf("the request has no matchers")
	}
	entries := make([]entryResult, 0, len(matchers))
	for _, m := range matchers {
		result, err := evaluateOne(m, ctx)
		if err != nil {
			return MatchResult{}, err
		}
		entries = append(entries, entryResult{matcher: m, result: result})
	}

	// The request level condition overrides the grouping: "or" means one
	// entry is enough, whatever the entries say.
	if strings.EqualFold(strings.TrimSpace(condition), "or") {
		for _, entry := range entries {
			if entry.result.Matched {
				return entry.result, nil
			}
		}
		return MatchResult{}, nil
	}

	var (
		andAll  = true
		hasOr   bool
		orFired bool
		firing  MatchResult
		found   bool
	)
	for _, entry := range entries {
		if entry.result.Matched && !found {
			firing = entry.result
			found = true
		}
		if strings.EqualFold(entry.matcher.Condition, "or") {
			hasOr = true
			if entry.result.Matched {
				orFired = true
			}
			continue
		}
		if !entry.result.Matched {
			andAll = false
		}
	}
	matched := andAll && (!hasOr || orFired)
	if !matched {
		return MatchResult{}, nil
	}
	if !found {
		// Every entry was negative and fired, and none of them reported
		// itself. The verdict still stands; the reason is thin.
		firing = MatchResult{Matcher: "matchers", Evidence: "the negative matchers held"}
	}
	return firing, nil
}

// entryResult is one evaluated matcher entry.
type entryResult struct {
	matcher Matcher
	result  MatchResult
}

// evaluateOne evaluates a single matcher entry.
func evaluateOne(m Matcher, ctx evalContext) (MatchResult, error) {
	result, err := evaluateRaw(m, ctx)
	if err != nil {
		return MatchResult{}, err
	}
	if m.Negative {
		result.Matched = !result.Matched
		result.Matcher = "negative " + result.Matcher
		if result.Matched {
			result.Evidence = "absent: " + result.Evidence
		}
	}
	return result, nil
}

// evaluateRaw evaluates a matcher entry without the negative flag.
func evaluateRaw(m Matcher, ctx evalContext) (MatchResult, error) {
	resp := ctx.response
	switch m.Type {
	case "word":
		return matchWords(m, resp)
	case "regex":
		return matchRegex(m, resp)
	case "status":
		return matchStatus(m, resp)
	case "size":
		return matchSize(m, resp)
	case "binary":
		return matchBinary(m, resp)
	case "and", "or":
		return matchGroup(m, ctx)
	case "dsl":
		return matchDSL(m, ctx)
	case "":
		return MatchResult{}, fmt.Errorf("a matcher has no type")
	default:
		return MatchResult{}, fmt.Errorf("the %q matcher type is not implemented", m.Type)
	}
}

// matchDSL evaluates the expressions of a dsl matcher.
//
// The expressions are grouped the way the other matchers group their
// words: an "or" condition is satisfied by one of them and anything else
// needs all of them. An expression that cannot be evaluated is an error:
// a dsl matcher that quietly did not match would be a false negative
// dressed as a clean run.
func matchDSL(m Matcher, ctx evalContext) (MatchResult, error) {
	if len(m.DSL) == 0 {
		return MatchResult{}, fmt.Errorf("the dsl matcher has no expressions")
	}
	var (
		hits  int
		first MatchResult
	)
	for _, expression := range m.DSL {
		result, err := EvaluateDSL(expression, DSLContext{Response: ctx.response, Vars: ctx.vars})
		if err != nil {
			return MatchResult{}, err
		}
		if !result.Bool {
			if isOr(m.Condition) {
				continue
			}
			return MatchResult{Matcher: describeDSL(expression), Evidence: dslEvidence(expression, result)}, nil
		}
		hits++
		if hits == 1 {
			first = MatchResult{
				Matched:  true,
				Matcher:  describeDSL(expression),
				Evidence: dslEvidence(expression, result),
			}
		}
	}
	if hits == 0 {
		return MatchResult{Matcher: describeDSL(strings.Join(m.DSL, " ")), Evidence: dslEvidence(m.DSL[0], DSLResult{})}, nil
	}
	first.Matched = true
	if hits < len(m.DSL) {
		first.Matcher = describeDSL(m.DSL[0]) + " (some expressions matched)"
	}
	return first, nil
}

// describeDSL names the expression a dsl matcher looked at.
func describeDSL(expression string) string {
	return "dsl " + strconv.Quote(expression)
}

// dslEvidence names the expression and the value that decided it.
func dslEvidence(expression string, result DSLResult) string {
	evidence := describeDSL(expression)
	if result.Bool {
		evidence += " is true"
	} else {
		evidence += " is false"
	}
	if result.Witness != "" {
		evidence += ": " + result.Witness
	}
	return evidence
}

// matchWords looks for the substrings in the chosen part.
func matchWords(m Matcher, resp Response) (MatchResult, error) {
	part := effectivePart(m.Part)
	text := partText(part, resp)
	haystack := text
	if m.CaseInsensitive {
		haystack = strings.ToLower(text)
	}
	var (
		hits   int
		first  = MatchResult{}
		needle string
	)
	for _, word := range m.Words {
		probe := word
		if m.CaseInsensitive {
			probe = strings.ToLower(word)
		}
		index := strings.Index(haystack, probe)
		if index < 0 {
			if isOr(m.Condition) {
				continue
			}
			return MatchResult{Matcher: describe("word", word, part)}, nil
		}
		hits++
		if hits == 1 {
			needle = word
			first = MatchResult{
				Matcher:  describe("word", word, part),
				Evidence: excerpt(text, index, evidenceRunes),
			}
		}
	}
	if hits == 0 {
		return MatchResult{Matcher: describe("word", strings.Join(m.Words, " "), part)}, nil
	}
	if isOr(m.Condition) {
		// Any word is enough, and the one that was there is the reason.
		first.Matched = true
		return first, nil
	}
	first.Matched = true
	if hits < len(m.Words) {
		first.Matcher = describe("word", needle, part) + " (some words matched)"
	}
	return first, nil
}

// matchRegex looks for the patterns in the chosen part.
func matchRegex(m Matcher, resp Response) (MatchResult, error) {
	part := effectivePart(m.Part)
	text := partText(part, resp)
	var (
		hits         int
		first        MatchResult
		firstPattern string
	)
	for _, pattern := range m.Regex {
		compiled, err := compileRegex(pattern, m.CaseInsensitive)
		if err != nil {
			return MatchResult{}, err
		}
		matches := compiled.FindAllStringIndex(text, maxRegexMatches)
		if len(matches) == 0 {
			if isOr(m.Condition) {
				continue
			}
			return MatchResult{Matcher: describe("regex", pattern, part)}, nil
		}
		hits++
		if hits == 1 {
			firstPattern = pattern
			first = MatchResult{
				Matcher:  describe("regex", pattern, part),
				Evidence: excerpt(text, matches[0][0], evidenceRunes),
			}
		}
	}
	if hits == 0 {
		return MatchResult{Matcher: describe("regex", strings.Join(m.Regex, " "), part)}, nil
	}
	first.Matched = true
	if hits < len(m.Regex) {
		first.Matcher = describe("regex", firstPattern, part) + " (some patterns matched)"
	}
	return first, nil
}

// matchStatus checks the status code against the list.
func matchStatus(m Matcher, resp Response) (MatchResult, error) {
	for _, code := range m.Status {
		if code == resp.Status {
			return MatchResult{
				Matched:  true,
				Matcher:  "status " + strconv.Itoa(code),
				Evidence: "status " + strconv.Itoa(resp.Status),
			}, nil
		}
	}
	return MatchResult{Matcher: "status " + joinInts(m.Status)}, nil
}

// matchSize checks the body length against the list.
func matchSize(m Matcher, resp Response) (MatchResult, error) {
	size := len(resp.Body)
	for _, want := range m.Size {
		if want == size {
			return MatchResult{
				Matched:  true,
				Matcher:  "size " + strconv.Itoa(want),
				Evidence: "body " + strconv.Itoa(size) + " bytes",
			}, nil
		}
	}
	return MatchResult{Matcher: "size " + joinInts(m.Size)}, nil
}

// matchBinary decodes the base64 blobs and looks for them in the response.
func matchBinary(m Matcher, resp Response) (MatchResult, error) {
	part := effectivePart(m.Part)
	haystack := partBytes(part, resp)
	var (
		hits  int
		first MatchResult
	)
	for _, encoded := range m.Binary {
		decoded, err := decodeBase64(encoded)
		if err != nil {
			return MatchResult{}, err
		}
		index := bytes.Index(haystack, decoded)
		if index < 0 {
			if isOr(m.Condition) {
				continue
			}
			return MatchResult{Matcher: describe("binary", encoded, part)}, nil
		}
		hits++
		if hits == 1 {
			first = MatchResult{
				Matcher:  describe("binary", encoded, part),
				Evidence: excerpt(string(haystack), index, evidenceRunes),
			}
		}
	}
	if hits == 0 {
		return MatchResult{Matcher: describe("binary", strings.Join(m.Binary, " "), part)}, nil
	}
	first.Matched = true
	return first, nil
}

// matchGroup combines the child matchers of an "and" or "or" matcher.
func matchGroup(m Matcher, ctx evalContext) (MatchResult, error) {
	if len(m.Matchers) == 0 {
		return MatchResult{}, fmt.Errorf("the %s matcher has no child matchers", m.Type)
	}
	var (
		firing MatchResult
		found  bool
	)
	for _, child := range m.Matchers {
		result, err := evaluateOne(child, ctx)
		if err != nil {
			return MatchResult{}, err
		}
		if isOr(m.Type) {
			if result.Matched {
				return MatchResult{
					Matched:  true,
					Matcher:  "or: " + result.Matcher,
					Evidence: result.Evidence,
				}, nil
			}
			continue
		}
		if !result.Matched {
			return MatchResult{Matcher: "and: " + result.Matcher}, nil
		}
		if !found {
			firing = result
			found = true
		}
	}
	if isOr(m.Type) {
		return MatchResult{Matcher: "or: no child matched"}, nil
	}
	if !found {
		return MatchResult{Matcher: "and"}, nil
	}
	return MatchResult{Matched: true, Matcher: "and: " + firing.Matcher, Evidence: firing.Evidence}, nil
}

// UnsupportedReason names why a matcher list cannot be evaluated here, or
// returns an empty string when it can.
//
// The caller skips the template and says why. A template that is skipped
// loudly is a gap in the run; a template that is skipped quietly is a check
// that never happened.
func UnsupportedReason(matchers []Matcher) string {
	for _, m := range matchers {
		switch m.Type {
		case "word", "regex", "status", "size", "binary", "and", "or":
		case "dsl":
			if reason := dslReason(m); reason != "" {
				return reason
			}
		case "":
			return "the template has a matcher with no type"
		default:
			return "the template uses a " + strconv.Quote(m.Type) + " matcher, which this runner does not implement"
		}
		if reason := unobservableHeaderReason(m); reason != "" {
			return reason
		}
		if reason := UnsupportedReason(m.Matchers); reason != "" {
			return reason
		}
	}
	return ""
}

// dslReason names why a dsl matcher cannot be evaluated here.
//
// The expressions are parsed here, where there is no response to evaluate
// them against: a template whose dsl expression does not compile is a
// template that would fail on every response, and saying so once is
// better than saying it once per response.
func dslReason(m Matcher) string {
	if len(m.DSL) == 0 {
		return "the template has a dsl matcher with no expressions"
	}
	for _, expression := range m.DSL {
		if _, err := parseDSLCached(expression); err != nil {
			return "the template's dsl expression does not compile: " + err.Error()
		}
	}
	return ""
}

// unobservableHeaders are the response headers the harness's probe does not
// keep.
//
// The probe writes a bounded subset of the response headers into the result
// and the receipt. A check on a header outside that subset would report
// every site as missing it, which is a false positive dressed as a finding.
// A template that checks one of these is skipped with a reason instead.
//
// The list is deliberately short: it names the security headers an operator
// asks about, and it errs towards skipping a check that could have run.
// A header added to the probe's kept subset later leaves this list stale
// in the safe direction, and a header removed from it would need this list
// updated.
var unobservableHeaders = map[string]bool{
	"strict-transport-security":    true,
	"permissions-policy":           true,
	"referrer-policy":              true,
	"x-content-type-options":       true,
	"cross-origin-opener-policy":   true,
	"cross-origin-resource-policy": true,
	"x-xss-protection":             true,
	"public-key-pins":              true,
}

// unobservableHeaderReason names the header a matcher cannot see.
func unobservableHeaderReason(m Matcher) string {
	if m.Type != "word" {
		return ""
	}
	if part := strings.ToLower(strings.TrimSpace(m.Part)); part != "header" && part != "all" {
		return ""
	}
	for _, word := range m.Words {
		name := strings.ToLower(strings.TrimSpace(word))
		if unobservableHeaders[name] {
			return "the template checks the " + name + " header, which the harness's probe does not keep"
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Parts
// ---------------------------------------------------------------------------

// effectivePart returns the part a matcher reads, defaulting to the body.
func effectivePart(part string) string {
	switch strings.ToLower(strings.TrimSpace(part)) {
	case "", "body":
		return "body"
	case "header":
		return "header"
	case "all":
		return "all"
	case "url":
		return "url"
	default:
		return "body"
	}
}

// partText returns the text of the chosen part.
func partText(part string, resp Response) string {
	switch part {
	case "header":
		return headerText(resp.Headers)
	case "all":
		return headerText(resp.Headers) + "\n" + string(resp.Body)
	case "url":
		return resp.URL
	default:
		return string(resp.Body)
	}
}

// partBytes returns the bytes of the chosen part.
func partBytes(part string, resp Response) []byte {
	switch part {
	case "header":
		return []byte(headerText(resp.Headers))
	case "all":
		return []byte(headerText(resp.Headers) + "\n" + string(resp.Body))
	case "url":
		return []byte(resp.URL)
	default:
		return resp.Body
	}
}

// headerText renders the headers as one sorted block.
func headerText(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sortStrings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(headers[name])
		b.WriteString("\n")
	}
	return b.String()
}

// sortStrings sorts in place. A tiny helper keeps the import list of this
// file to what it actually uses.
func sortStrings(values []string) {
	sort.Strings(values)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// describe names the matcher and what it looked for.
func describe(kind, needle, part string) string {
	if needle == "" {
		return kind + " in " + part
	}
	return kind + " " + strconv.Quote(needle) + " in " + part
}

// isOr reports whether a condition asks for any of the group.
func isOr(condition string) bool {
	return strings.EqualFold(strings.TrimSpace(condition), "or")
}

// joinInts renders a list of ints for a matcher description.
func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}

// decodeBase64 decodes a binary matcher blob, tolerating the unpadded form.
func decodeBase64(encoded string) ([]byte, error) {
	trimmed := strings.TrimSpace(encoded)
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err == nil {
		return decoded, nil
	}
	raw, rawErr := base64.RawStdEncoding.DecodeString(trimmed)
	if rawErr != nil {
		return nil, fmt.Errorf("binary matcher %q is not base64: %w", encoded, err)
	}
	return raw, nil
}

// excerpt returns a bounded window of text around a byte index.
//
// The window is in runes, so the cut lands on a character boundary and
// multibyte text stays readable, and it is never the whole body.
func excerpt(text string, byteIndex int, window int) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	position := len([]rune(text[:clampIndex(byteIndex, len(text))]))
	start := position - window/2
	if start < 0 {
		start = 0
	}
	end := start + window
	if end > len(runes) {
		end = len(runes)
		start = end - window
		if start < 0 {
			start = 0
		}
	}
	out := sanitize(string(runes[start:end]))
	if start > 0 {
		out = "..." + out
	}
	if end < len(runes) {
		out = out + "..."
	}
	return out
}

// clampIndex keeps a byte index inside the text.
func clampIndex(index, length int) int {
	if index < 0 {
		return 0
	}
	if index > length {
		return length
	}
	return index
}

// sanitize replaces the control characters with spaces, so an excerpt can go
// into a line of output without carrying a terminal escape.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// ---------------------------------------------------------------------------
// The compiled pattern cache
// ---------------------------------------------------------------------------

var (
	regexMu    sync.Mutex
	regexCache = map[string]*regexp.Regexp{}
)

// compileRegex compiles a pattern and keeps it.
//
// The cache exists because a corpus of templates runs the same patterns
// again and again, and compiling a regexp per request per template is the
// kind of cost that turns a fast scan into a slow one.
func compileRegex(pattern string, caseInsensitive bool) (*regexp.Regexp, error) {
	key := pattern
	if caseInsensitive {
		key = "(?i)" + pattern
	}
	regexMu.Lock()
	defer regexMu.Unlock()
	if compiled, ok := regexCache[key]; ok {
		return compiled, nil
	}
	compiled, err := regexp.Compile(key)
	if err != nil {
		return nil, fmt.Errorf("the regex %q does not compile: %w", pattern, err)
	}
	if len(regexCache) >= maxCachedRegexes {
		// The whole cache goes rather than one entry: the corpus is finite
		// and a full cache means a pathological one.
		regexCache = map[string]*regexp.Regexp{}
	}
	regexCache[key] = compiled
	return compiled, nil
}

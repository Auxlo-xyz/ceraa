package nuclei

// template.go reads Nuclei templates.
//
// The Nuclei dialect is not Go's template dialect and pretending otherwise
// buys nothing. A Nuclei expression is a variable, a function call or a
// quoted literal, wrapped in {{ }}, so the renderer here is explicit and
// small. An expression it cannot resolve renders as an empty string and the
// name of what was missing comes back with the result: a template with a
// hole in it is a template with a hole in it, and the run says which hole
// rather than dying on it.
//
// Extractors are implemented: the `extractors:` block, the regex, dsl, kval
// and json extractors, and the values a previous request produced feeding
// the next one. A template whose matchers depend on an extracted value runs
// against the value the extractor actually found.
//
// Cluster attacks are implemented too: the request is fired once per
// payload plus once with no payload, and the payload whose response differs
// from the baseline is the finding.
//
// Only the HTTP template types are supported. The network, dns, headless,
// ssl, whois, javascript and code types are read, refused and named: a
// silently ignored template is a check that never ran wearing the costume
// of a check that did.

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Template is one Nuclei template, reduced to what the runner needs.
type Template struct {
	ID   string
	Info Info
	HTTP []HTTPRequest
	// Type is the Nuclei template type. It is always "http" here: the other
	// types never reach the parser.
	Type string
	// Variables is the template's own `variables:` block. Other requests in
	// the same template may reference them.
	Variables map[string]string
	// Extractors are the template level `extractors:` block. They run
	// against every response the template produces.
	Extractors []Extractor
	// Path is the file the template was read from, so a finding can name
	// the file it came from.
	Path string
	// Raw is the decoded YAML, kept for the fields this runner does not
	// model yet.
	Raw map[string]any
}

// Info is the `info:` block.
type Info struct {
	Name        string
	Severity    string
	Description string
	Tags        []string
	Author      []string
	Reference   []string
}

// HTTPRequest is one request block of the `http:` list.
type HTTPRequest struct {
	Method  string
	Path    []string
	Headers map[string]string
	Body    string
	Cookies map[string]string
	// Raw holds the raw request blocks, for the raw request mode. A raw
	// block is a whole HTTP request in one string.
	Raw []string
	// Matchers are the matchers of this request.
	Matchers []Matcher
	// Extractors are the request's `extractors:` block. The values they
	// pull out of a response feed the later requests of the template.
	Extractors []Extractor
	// AttackType is "single" or "cluster". A cluster attack fires the
	// request once per payload and compares the responses.
	AttackType string
	// Cluster is the cluster attack this request declares. It is nil for
	// every request that does not declare one.
	Cluster *Cluster
	// Payload is the request's payload block. It is parsed so a template
	// with one loads, and it feeds a cluster attack.
	Payload map[string]any
	// Payloads is the request's `payloads:` list, the values a cluster
	// attack sweeps.
	Payloads []string
	// MatchersCondition is the request-level `matchers-condition`, which
	// decides how the matcher list is grouped.
	MatchersCondition string
	// CaseInsensitive is the request-level `matchers-case-insensitive`, the
	// default for the matchers that do not set their own.
	CaseInsensitive bool
}

// unsupportedTemplateTypes are the Nuclei template types this runner reads
// and refuses. The name is in the error, because a corpus of thousands has
// one of these in it and the operator has to know which ones were skipped.
var unsupportedTemplateTypes = []string{
	"network", "dns", "headless", "ssl", "whois", "javascript", "code", "file",
}

// Parse reads one Nuclei template from YAML bytes.
//
// path is the file the template came from. It only ever appears in errors,
// and that is the point: a template that cannot be loaded has to be
// nameable by whoever has to fix it.
func Parse(data []byte, path string) (Template, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Template{}, fmt.Errorf("template %s is not valid YAML: %w", displayPath(path), err)
	}
	if root == nil {
		return Template{}, fmt.Errorf("template %s is empty", displayPath(path))
	}

	id := strings.TrimSpace(stringOf(root["id"]))
	if id == "" {
		return Template{}, fmt.Errorf("template %s has no id", displayPath(path))
	}

	block, ok := root["http"]
	if !ok {
		block, ok = root["requests"]
	}
	if !ok {
		for _, kind := range unsupportedTemplateTypes {
			if _, present := root[kind]; present {
				return Template{}, fmt.Errorf("template %s is a %s template; only http templates are supported", displayPath(path), kind)
			}
		}
		return Template{}, fmt.Errorf("template %s has no http block", displayPath(path))
	}

	items, ok := block.([]any)
	if !ok {
		return Template{}, fmt.Errorf("template %s: the http block is not a list of requests", displayPath(path))
	}
	if len(items) == 0 {
		return Template{}, fmt.Errorf("template %s has an empty http block", displayPath(path))
	}

	tpl := Template{
		ID:        id,
		Info:      parseInfo(root["info"]),
		Type:      "http",
		Variables: parseVariables(root["variables"]),
		Path:      path,
		Raw:       root,
	}
	extractors, err := parseExtractors(root["extractors"])
	if err != nil {
		return Template{}, fmt.Errorf("template %s: %w", displayPath(path), err)
	}
	tpl.Extractors = extractors
	for i, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			return Template{}, fmt.Errorf("template %s: http entry %d is not a mapping", displayPath(path), i+1)
		}
		request, err := parseHTTPRequest(item)
		if err != nil {
			return Template{}, fmt.Errorf("template %s: http entry %d: %w", displayPath(path), i+1, err)
		}
		tpl.HTTP = append(tpl.HTTP, request)
	}
	return tpl, nil
}

// parseInfo reads the `info:` block. A missing block is not an error: the
// name and the severity are what a finding carries, and a template without
// them still runs, it just says less.
func parseInfo(v any) Info {
	m, _ := v.(map[string]any)
	return Info{
		Name:        stringOf(m["name"]),
		Severity:    strings.ToLower(strings.TrimSpace(stringOf(m["severity"]))),
		Description: stringOf(m["description"]),
		// The corpus writes tags and authors as one comma separated
		// string, and as a list, and both are in the wild.
		Tags:      commaList(m["tags"]),
		Author:    commaList(m["author"]),
		Reference: stringList(m["reference"]),
	}
}

// parseVariables reads the template level `variables:` block.
func parseVariables(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for name, value := range m {
		out[name] = stringOf(value)
	}
	return out
}

// parseHTTPRequest reads one entry of the `http:` list.
func parseHTTPRequest(item map[string]any) (HTTPRequest, error) {
	req := HTTPRequest{
		Method:            strings.ToUpper(strings.TrimSpace(stringOf(item["method"]))),
		Path:              stringList(item["path"]),
		Headers:           stringMap(item["headers"]),
		Body:              stringOf(item["body"]),
		Cookies:           stringMap(item["cookies"]),
		Raw:               stringList(item["raw"]),
		AttackType:        strings.ToLower(strings.TrimSpace(stringOf(item["attack"]))),
		Payload:           mapOf(item["payload"]),
		Payloads:          stringList(item["payloads"]),
		MatchersCondition: strings.ToLower(strings.TrimSpace(stringOf(item["matchers-condition"]))),
		CaseInsensitive:   boolOf(item["matchers-case-insensitive"]),
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	matchers, err := parseMatchers(item["matchers"])
	if err != nil {
		return HTTPRequest{}, err
	}
	req.Matchers = matchers
	extractors, err := parseExtractors(item["extractors"])
	if err != nil {
		return HTTPRequest{}, err
	}
	req.Extractors = extractors
	if strings.EqualFold(req.AttackType, "cluster") {
		cluster, err := parseCluster(req)
		if err != nil {
			return HTTPRequest{}, err
		}
		req.Cluster = cluster
	}
	return req, nil
}

// parseExtractors reads an `extractors:` list.
//
// The list is a list of mappings and nothing else is accepted: an
// extractor that is a bare string is a template this parser would have to
// guess about, and a guess in an extractor is a value nobody asked for.
func parseExtractors(v any) ([]Extractor, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]Extractor, 0, len(items))
	for i, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("extractor %d is not a mapping", i+1)
		}
		spec := Extractor{
			Type:            strings.ToLower(strings.TrimSpace(stringOf(m["type"]))),
			Regex:           stringList(m["regex"]),
			DSL:             stringList(m["dsl"]),
			Kval:            stringList(m["kval"]),
			JSON:            stringList(m["json"]),
			Name:            strings.TrimSpace(stringOf(m["name"])),
			Part:            strings.ToLower(strings.TrimSpace(stringOf(m["part"]))),
			CaseInsensitive: boolOf(m["case-insensitive"]),
			Internal:        boolOf(m["internal"]),
		}
		if group, ok := toInt(m["group"]); ok {
			spec.Group = group
		}
		out = append(out, spec)
	}
	return out, nil
}

// parseCluster reads the cluster attack of a request.
//
// The attribute the responses are compared on comes from the matchers:
// a word matcher compares the words, a status matcher the status code and
// a size matcher the body size. A cluster attack with no such matcher has
// nothing to compare and is refused here rather than at request time.
func parseCluster(req HTTPRequest) (*Cluster, error) {
	attribute, words, ok := clusterAttribute(req.Matchers)
	if !ok {
		return nil, fmt.Errorf("the cluster attack has no word, status or size matcher to compare the responses by")
	}
	return &Cluster{
		Method:    req.Method,
		Payloads:  clusterPayloads(req),
		Attribute: attribute,
		Words:     words,
		Condition: req.MatchersCondition,
	}, nil
}

// clusterAttribute reads the attribute a cluster compares on.
func clusterAttribute(matchers []Matcher) (string, []string, bool) {
	for _, m := range matchers {
		switch m.Type {
		case "word":
			if len(m.Words) == 0 {
				continue
			}
			return "word", m.Words, true
		case "status":
			return "status", nil, true
		case "size":
			return "size", nil, true
		}
	}
	return "", nil, false
}

// clusterPayloads reads the payloads a cluster attack sweeps.
//
// The `payloads:` list contributes one entry per value, named by the value
// because that is what a reader wants to see in the finding. The `payload:`
// block contributes one entry per key, and a key whose value is a list
// contributes one entry per item, named key-1, key-2 and so on.
func clusterPayloads(req HTTPRequest) map[string]string {
	out := map[string]string{}
	for _, value := range req.Payloads {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		name := uniquePayloadName(out, value)
		out[name] = value
	}
	names := make([]string, 0, len(req.Payload))
	for name := range req.Payload {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for index, value := range stringList(req.Payload[name]) {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := name
			if index > 0 {
				key = fmt.Sprintf("%s-%d", name, index+1)
			}
			key = uniquePayloadName(out, key)
			out[key] = value
		}
	}
	return out
}

// uniquePayloadName returns a name that is not taken yet.
func uniquePayloadName(taken map[string]string, preferred string) string {
	if _, exists := taken[preferred]; !exists {
		return preferred
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", preferred, i)
		if _, exists := taken[candidate]; !exists {
			return candidate
		}
	}
}

// parseMatchers reads the `matchers:` list.
//
// The list is a list of mappings and nothing else is accepted: a matcher
// that is a bare string is a template this parser would have to guess about,
// and a guess in a matcher is a false finding.
func parseMatchers(v any) ([]Matcher, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]Matcher, 0, len(items))
	for i, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("matcher %d is not a mapping", i+1)
		}
		matcher := Matcher{
			Type:            strings.ToLower(strings.TrimSpace(stringOf(m["type"]))),
			Words:           stringList(m["words"]),
			Regex:           stringList(m["regex"]),
			DSL:             stringList(m["dsl"]),
			Binary:          stringList(m["binary"]),
			Status:          intList(m["status"]),
			Size:            intList(m["size"]),
			Negative:        boolOf(m["negative"]),
			Condition:       strings.ToLower(strings.TrimSpace(stringOf(m["condition"]))),
			Part:            strings.ToLower(strings.TrimSpace(stringOf(m["part"]))),
			CaseInsensitive: boolOf(m["case-insensitive"]),
		}
		children, err := parseMatchers(m["matchers"])
		if err != nil {
			return nil, fmt.Errorf("matcher %d: %w", i+1, err)
		}
		matcher.Matchers = children
		out = append(out, matcher)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The variable renderer
// ---------------------------------------------------------------------------

// Vars is the variable set one request is rendered against.
type Vars map[string]string

// TargetVars derives the built-in variables from a target URL.
//
// The names are Nuclei's own: a template written against the upstream corpus
// uses them, and renaming them would make every such template render an
// empty path.
func TargetVars(target *url.URL) Vars {
	scheme := strings.ToLower(target.Scheme)
	host := target.Hostname()
	port := target.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	path := target.Path
	if path == "" {
		path = "/"
	}
	// BaseURL keeps the target's own path, so a scan of
	// https://example.com/app stays inside /app when a template builds on
	// {{BaseURL}}.
	base := originOf(target) + strings.TrimSuffix(target.Path, "/")
	return Vars{
		"BaseURL":  base,
		"RootURL":  base,
		"Host":     target.Host,
		"Hostname": host,
		"Port":     port,
		"Scheme":   scheme,
		"Path":     path,
	}
}

// originOf returns scheme://host[:port] for a target, without its path.
func originOf(target *url.URL) string {
	return strings.ToLower(target.Scheme) + "://" + target.Host
}

// Render expands the Nuclei {{ }} expressions in s.
//
// The second result names the variables that resolved to nothing. An unknown
// variable renders as an empty string instead of failing the run, because a
// template that references a variable nobody set is still a template whose
// other checks are worth running, and the run has to say which hole it hit.
func Render(s string, vars Vars) (string, []string) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var (
		out     strings.Builder
		missing []string
	)
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			// An unterminated expression: the rest is literal text.
			out.WriteString(rest)
			break
		}
		end += start
		out.WriteString(rest[:start])
		value, gap := evalExpr(rest[start+2:end], vars)
		if gap != "" {
			missing = append(missing, gap)
		}
		out.WriteString(value)
		rest = rest[end+2:]
	}
	return out.String(), missing
}

// evalExpr evaluates one Nuclei expression.
//
// The second result names what was missing: a variable nobody set, or a
// function this runner does not implement. It is empty when the expression
// resolved.
func evalExpr(expr string, vars Vars) (string, string) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", ""
	}
	if literal, ok := unquote(expr); ok {
		return literal, ""
	}
	if name, args, ok := splitCall(expr); ok {
		return evalFunc(name, args, vars)
	}
	return evalVariable(expr, vars)
}

// evalVariable resolves a bare variable name.
func evalVariable(name string, vars Vars) (string, string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", ""
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "randstr":
		return randomString(defaultRandStrLength), ""
	case "uuid":
		return randomUUID(), ""
	}
	if value, ok := vars[trimmed]; ok {
		return value, ""
	}
	// {{RandStr:12}} asks for a length.
	if strings.HasPrefix(lower, "randstr:") {
		if n, err := strconv.Atoi(strings.TrimSpace(trimmed[len("randstr:"):])); err == nil && n > 0 && n <= 64 {
			return randomString(n), ""
		}
	}
	return "", trimmed
}

// evalFunc evaluates a function call.
//
// The helpers are the ones real templates reach for. A helper this runner
// does not know renders empty and is named in the missing list, so a
// template written for a newer Nuclei says what it wanted instead of
// quietly matching nothing.
func evalFunc(name string, args []string, vars Vars) (string, string) {
	switch strings.ToLower(name) {
	case "to_lower":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		return strings.ToLower(value), ""
	case "to_upper":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		return strings.ToUpper(value), ""
	case "trim_space":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		return strings.TrimSpace(value), ""
	case "trim_prefix":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		prefix, gap := resolveArg(arg(args, 1), vars)
		if gap != "" {
			return "", gap
		}
		return strings.TrimPrefix(value, prefix), ""
	case "trim_suffix":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		suffix, gap := resolveArg(arg(args, 1), vars)
		if gap != "" {
			return "", gap
		}
		return strings.TrimSuffix(value, suffix), ""
	case "replace":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		old, gap := resolveArg(arg(args, 1), vars)
		if gap != "" {
			return "", gap
		}
		replacement, gap := resolveArg(arg(args, 2), vars)
		if gap != "" {
			return "", gap
		}
		return strings.ReplaceAll(value, old, replacement), ""
	case "concat":
		var b strings.Builder
		for _, a := range args {
			value, gap := resolveArg(a, vars)
			if gap != "" {
				return "", gap
			}
			b.WriteString(value)
		}
		return b.String(), ""
	case "len":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		return strconv.Itoa(len([]rune(value))), ""
	case "contains":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		needle, gap := resolveArg(arg(args, 1), vars)
		if gap != "" {
			return "", gap
		}
		return strconv.FormatBool(strings.Contains(value, needle)), ""
	case "has_prefix":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		prefix, gap := resolveArg(arg(args, 1), vars)
		if gap != "" {
			return "", gap
		}
		return strconv.FormatBool(strings.HasPrefix(value, prefix)), ""
	case "has_suffix":
		value, gap := resolveArg(arg(args, 0), vars)
		if gap != "" {
			return "", gap
		}
		suffix, gap := resolveArg(arg(args, 1), vars)
		if gap != "" {
			return "", gap
		}
		return strconv.FormatBool(strings.HasSuffix(value, suffix)), ""
	default:
		return "", name
	}
}

// resolveArg resolves one function argument: a quoted literal is itself, and
// anything else is an expression.
func resolveArg(text string, vars Vars) (string, string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", ""
	}
	if literal, ok := unquote(trimmed); ok {
		return literal, ""
	}
	return evalExpr(trimmed, vars)
}

// arg returns the nth argument or an empty string.
func arg(args []string, n int) string {
	if n < 0 || n >= len(args) {
		return ""
	}
	return args[n]
}

// unquote reads a quoted literal.
func unquote(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1], true
	}
	return "", false
}

// splitCall splits a function call into its name and its arguments.
func splitCall(expr string) (string, []string, bool) {
	open := strings.Index(expr, "(")
	if open <= 0 || !strings.HasSuffix(expr, ")") {
		return "", nil, false
	}
	name := strings.TrimSpace(expr[:open])
	if name == "" {
		return "", nil, false
	}
	return name, splitArgs(expr[open+1 : len(expr)-1]), true
}

// splitArgs splits an argument list on the commas that are not inside a
// quoted literal or a nested call.
func splitArgs(inner string) []string {
	var (
		args  []string
		depth int
		quote byte
		start int
	)
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			args = append(args, strings.TrimSpace(inner[start:i]))
			start = i + 1
		}
	}
	if tail := strings.TrimSpace(inner[start:]); tail != "" || len(args) > 0 {
		args = append(args, tail)
	}
	return args
}

// defaultRandStrLength is the length of {{RandStr}} when the template does
// not ask for one.
const defaultRandStrLength = 6

// randomString returns a random alphanumeric string.
//
// crypto/rand, because a predictable value in a request path is a value an
// attacker can guess. The modulo is biased by a few percent, which is
// irrelevant for a nonce and irrelevant to nobody else.
func randomString(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if n <= 0 {
		n = defaultRandStrLength
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "zzq7x" + strconv.Itoa(n)
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf)
}

// randomUUID returns a random uuid v4.
func randomUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// ---------------------------------------------------------------------------
// Small YAML helpers
// ---------------------------------------------------------------------------

// stringOf reads a scalar as a string. Numbers and booleans are stringified
// because a YAML template writes a port or a count as a number and the
// renderer wants text.
func stringOf(v any) string {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

// stringList reads a scalar or a list of scalars as a list of strings.
func stringList(v any) []string {
	switch typed := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			out = append(out, stringOf(entry))
		}
		return out
	case []string:
		return typed
	default:
		return []string{stringOf(typed)}
	}
}

// commaList reads a scalar or a list of scalars as a list of strings, and
// splits a comma separated scalar into its parts.
//
// The corpus writes `tags: exposure,git,misconfig` as often as it writes a
// list, and both have to end up as a list of tags.
func commaList(v any) []string {
	values := stringList(v)
	if len(values) <= 1 {
		if len(values) == 0 {
			return nil
		}
		if !strings.Contains(values[0], ",") {
			return values
		}
	}
	var out []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

// stringMap reads a mapping of scalars as a mapping of strings.
func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for name, value := range m {
		out[name] = stringOf(value)
	}
	return out
}

// mapOf reads a mapping as it stands.
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// intList reads a number or a list of numbers as a list of ints.
func intList(v any) []int {
	switch typed := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]int, 0, len(typed))
		for _, entry := range typed {
			if n, ok := toInt(entry); ok {
				out = append(out, n)
			}
		}
		return out
	case []int:
		return typed
	default:
		if n, ok := toInt(typed); ok {
			return []int{n}
		}
		return nil
	}
}

// toInt reads one number, whatever shape YAML gave it.
func toInt(v any) (int, bool) {
	switch typed := v.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// boolOf reads a boolean, tolerating the string form.
func boolOf(v any) bool {
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return err == nil && parsed
	default:
		return false
	}
}

// displayPath names a file in an error. An empty path is the embedded set,
// which has no file name.
func displayPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "embedded template"
	}
	return path
}

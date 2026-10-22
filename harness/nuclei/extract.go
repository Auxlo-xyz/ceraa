package nuclei

// extract.go pulls values out of a response.
//
// A Nuclei extractor is how a template carries a value from one request to
// the next: the first request finds a token and the second sends it. The
// values are stored under the extractor's name, and the later requests of
// the same template render {{name}}.
//
// Four kinds are implemented, the four the corpus uses:
//
//   - regex: a pattern and a capture group.
//   - dsl: a DSL expression whose value is a string.
//   - kval: a key=value pair out of a line oriented body.
//   - json: a dotted path into a JSON document.
//
// The value count is bounded. An extractor that matches a thousand times
// has one useful answer, and a template that stored a thousand values would
// put a thousand variables into the renderer.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The bounds.
const (
	// maxExtractedValues is how many values one extractor name may hold.
	// The count is a cap, not a target: the first value is the one a later
	// request uses and the rest are noise.
	maxExtractedValues = 10
	// maxExtractedJSONSize bounds the rendering of a json value that is
	// not a scalar.
	maxExtractedJSONSize = 4096
)

// Extractor is one extractor of a request.
type Extractor struct {
	// Type is "regex", "dsl", "kval" or "json".
	Type string
	// Regex are the patterns a regex extractor runs.
	Regex []string
	// DSL are the expressions a dsl extractor runs.
	DSL []string
	// Kval are the keys a kval extractor looks for. An empty list takes
	// every key=value pair in the body.
	Kval []string
	// JSON are the dotted paths a json extractor walks.
	JSON []string
	// Name is the variable the values are stored under.
	Name string
	// Group is the capture group a regex extractor takes. The default is
	// the first group, and the whole match when the pattern has no group.
	Group int
	// Part is "body", "header" or "all": what the extractor reads.
	Part string
	// CaseInsensitive makes the extraction ignore case.
	CaseInsensitive bool
	// Internal marks a variable that is only for the template's own later
	// requests. An internal value is not reported as a finding.
	Internal bool
}

// Extract runs a set of extractors against a response.
//
// The values are returned beside the error, not instead of it: one
// extractor that does not run must not cost the values the others found.
func Extract(specs []Extractor, response Response) (map[string][]string, error) {
	return ExtractWithVars(specs, response, nil)
}

// ExtractWithVars runs the extractors with the template's own variables
// available to a dsl extractor.
//
// The variables are the template's own variables block and whatever an
// earlier response produced, so a dsl extractor can compare against a
// value the template set up.
func ExtractWithVars(specs []Extractor, response Response, vars map[string]string) (map[string][]string, error) {
	out := map[string][]string{}
	var problems []string
	for _, spec := range specs {
		values, err := extractOne(spec, response, vars)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		name := spec.Name
		if name == "" {
			name = spec.Type
		}
		if len(out[name]) >= maxExtractedValues {
			continue
		}
		room := maxExtractedValues - len(out[name])
		if len(values) > room {
			values = values[:room]
		}
		out[name] = append(out[name], values...)
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("the extractors did not all run: %s", strings.Join(problems, "; "))
	}
	return out, nil
}

// extractOne runs one extractor.
func extractOne(spec Extractor, response Response, vars map[string]string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(spec.Type)) {
	case "regex":
		return extractRegex(spec, response)
	case "dsl":
		return extractDSL(spec, response, vars)
	case "kval":
		return extractKval(spec, response)
	case "json":
		return extractJSON(spec, response)
	case "":
		return nil, fmt.Errorf("the extractor has no type")
	default:
		return nil, fmt.Errorf("the %q extractor type is not implemented", spec.Type)
	}
}

// extractRegex runs the patterns of a regex extractor.
func extractRegex(spec Extractor, response Response) ([]string, error) {
	text := partText(effectivePart(spec.Part), response)
	var out []string
	for _, pattern := range spec.Regex {
		compiled, err := compileRegex(pattern, spec.CaseInsensitive)
		if err != nil {
			return nil, err
		}
		group := spec.Group
		if group <= 0 {
			// The default is the first group, and the whole match when the
			// pattern has no group to take.
			if compiled.NumSubexp() == 0 {
				group = 0
			} else {
				group = 1
			}
		}
		if group > compiled.NumSubexp() {
			return nil, fmt.Errorf("the extractor regex %q has %d capture groups, group %d was asked for", pattern, compiled.NumSubexp(), group)
		}
		matches := compiled.FindAllStringSubmatch(text, maxExtractedValues+1)
		for _, match := range matches {
			if len(out) >= maxExtractedValues {
				break
			}
			if group >= len(match) {
				continue
			}
			value := match[group]
			if value == "" {
				continue
			}
			out = append(out, value)
		}
	}
	return out, nil
}

// extractDSL runs the expressions of a dsl extractor.
//
// A dsl expression that answers with true or false has not extracted
// anything: the value of the expression is the answer, and a boolean is
// not a value a later request can send.
func extractDSL(spec Extractor, response Response, vars map[string]string) ([]string, error) {
	var out []string
	for _, expression := range spec.DSL {
		result, err := EvaluateDSL(expression, DSLContext{Response: response, Vars: vars})
		if err != nil {
			return nil, err
		}
		value := strings.TrimSpace(result.Value)
		if value == "" {
			continue
		}
		if strings.EqualFold(value, "true") || strings.EqualFold(value, "false") {
			continue
		}
		out = append(out, value)
	}
	return out, nil
}

// extractKval reads the key=value pairs of a body.
func extractKval(spec Extractor, response Response) ([]string, error) {
	text := partText(effectivePart(spec.Part), response)
	var out []string
	if len(spec.Kval) == 0 {
		for _, pair := range kvalPairs(text) {
			if len(out) >= maxExtractedValues {
				break
			}
			out = append(out, pair.value)
		}
		return out, nil
	}
	for _, key := range spec.Kval {
		if len(out) >= maxExtractedValues {
			break
		}
		pair, ok := findKval(text, key, spec.CaseInsensitive)
		if !ok {
			continue
		}
		out = append(out, pair.value)
	}
	return out, nil
}

// extractJSON walks the dotted paths of a json extractor.
func extractJSON(spec Extractor, response Response) ([]string, error) {
	body := string(response.Body)
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	var document any
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		return nil, fmt.Errorf("the body is not JSON: %w", err)
	}
	var out []string
	for _, path := range spec.JSON {
		if len(out) >= maxExtractedValues {
			break
		}
		value, ok := jsonPath(document, path)
		if !ok {
			continue
		}
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out, nil
}

// kvalPair is one key=value pair of a body.
type kvalPair struct {
	name  string
	value string
}

// kvalPairs reads every key=value pair of a text.
//
// The pairs are read off the whitespace separated words of each line,
// which is the shape a body that carries configuration keys has.
func kvalPairs(text string) []kvalPair {
	var out []kvalPair
	for _, line := range strings.Split(text, "\n") {
		for _, field := range strings.Fields(line) {
			name, value, found := strings.Cut(field, "=")
			if !found || name == "" {
				continue
			}
			out = append(out, kvalPair{name: name, value: strings.Trim(value, `"'`)})
		}
	}
	return out
}

// findKval reads the value of one key.
func findKval(text, key string, caseInsensitive bool) (kvalPair, bool) {
	for _, pair := range kvalPairs(text) {
		if pair.name == key || caseInsensitive && strings.EqualFold(pair.name, key) {
			return pair, true
		}
	}
	return kvalPair{}, false
}

// ---------------------------------------------------------------------------
// The json paths
// ---------------------------------------------------------------------------

// jsonSegment is one step of a json path.
type jsonSegment struct {
	name    string
	index   int
	isIndex bool
}

// parseJSONPath reads a dotted path such as "$.items[0].id".
func parseJSONPath(path string) ([]jsonSegment, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("the json path is empty")
	}
	if !strings.HasPrefix(trimmed, "$") {
		return nil, fmt.Errorf("the json path %q does not start with $", path)
	}
	var segments []jsonSegment
	rest := trimmed[1:]
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			if name == "" {
				return nil, fmt.Errorf("the json path %q has an empty name", path)
			}
			segments = append(segments, jsonSegment{name: name})
			rest = rest[end:]
		case '[':
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, fmt.Errorf("the json path %q has an unclosed [", path)
			}
			inner := strings.TrimSpace(rest[1:end])
			rest = rest[end+1:]
			if strings.HasPrefix(inner, "'") || strings.HasPrefix(inner, `"`) {
				segments = append(segments, jsonSegment{name: strings.Trim(inner, `'"`)})
				continue
			}
			index, err := strconv.Atoi(inner)
			if err != nil {
				return nil, fmt.Errorf("the json path %q has the index %q, which is not a number", path, inner)
			}
			if index < 0 {
				return nil, fmt.Errorf("the json path %q has the negative index %d", path, index)
			}
			segments = append(segments, jsonSegment{index: index, isIndex: true})
		default:
			return nil, fmt.Errorf("the json path %q has an unexpected %q", path, string(rest[0]))
		}
	}
	return segments, nil
}

// jsonPath walks a parsed document and renders the value it finds.
func jsonPath(document any, path string) (string, bool) {
	segments, err := parseJSONPath(path)
	if err != nil {
		return "", false
	}
	current := document
	for _, segment := range segments {
		if segment.isIndex {
			list, ok := current.([]any)
			if !ok || segment.index >= len(list) {
				return "", false
			}
			current = list[segment.index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		value, ok := object[segment.name]
		if !ok {
			return "", false
		}
		current = value
	}
	return jsonScalar(current), true
}

// jsonScalar renders a json value as text.
func jsonScalar(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		if len(encoded) > maxExtractedJSONSize {
			return string(encoded[:maxExtractedJSONSize]) + "..."
		}
		return string(encoded)
	}
}

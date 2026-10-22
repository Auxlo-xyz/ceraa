package nuclei

import (
	"encoding/base64"
	"strings"
	"testing"
)

// sampleResponse is the response the matchers are evaluated against.
func sampleResponse() Response {
	return Response{
		Status: 200,
		Headers: map[string]string{
			"content-type":    "text/html",
			"x-frame-options": "DENY",
		},
		Body: []byte("<html><body>Welcome to the admin panel</body></html>"),
		URL:  "https://example.com/login",
	}
}

// TestEvaluateWordMatchesTheBody: the default part is the body.
func TestEvaluateWordMatchesTheBody(t *testing.T) {
	result, err := Evaluate([]Matcher{{Type: "word", Words: []string{"admin panel"}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Fatal("the word matcher did not match")
	}
	if !strings.Contains(result.Matcher, "word") || !strings.Contains(result.Matcher, "admin panel") {
		t.Errorf("the matcher does not name itself: %q", result.Matcher)
	}
	if !strings.Contains(result.Evidence, "admin panel") {
		t.Errorf("the evidence does not carry the match: %q", result.Evidence)
	}
}

// TestEvaluateWordFailsWhenTheWordIsAbsent.
func TestEvaluateWordFailsWhenTheWordIsAbsent(t *testing.T) {
	result, err := Evaluate([]Matcher{{Type: "word", Words: []string{"nothing here"}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("a word that is not there matched: %+v", result)
	}
}

// TestEvaluateWordIgnoresCaseWhenAsked.
func TestEvaluateWordIgnoresCaseWhenAsked(t *testing.T) {
	matchers := []Matcher{{Type: "word", Words: []string{"ADMIN PANEL"}, CaseInsensitive: true}}
	result, err := Evaluate(matchers, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Errorf("the case-insensitive matcher did not match: %+v", result)
	}
	strict := []Matcher{{Type: "word", Words: []string{"ADMIN PANEL"}}}
	result, err = Evaluate(strict, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("the case-sensitive matcher matched the wrong case: %+v", result)
	}
}

// TestEvaluateRegex: Go regexp, and the matched text is the evidence.
func TestEvaluateRegex(t *testing.T) {
	matchers := []Matcher{{Type: "regex", Regex: []string{`admin [a-z]+`}}}
	result, err := Evaluate(matchers, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Fatal("the regex matcher did not match")
	}
	if !strings.Contains(result.Evidence, "admin panel") {
		t.Errorf("the evidence does not carry the match: %q", result.Evidence)
	}
	if _, err := Evaluate([]Matcher{{Type: "regex", Regex: []string{"([unclosed"}}}, sampleResponse()); err == nil {
		t.Error("a regex that does not compile was accepted")
	}
}

// TestEvaluateStatus.
func TestEvaluateStatus(t *testing.T) {
	result, err := Evaluate([]Matcher{{Type: "status", Status: []int{200}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched || !strings.Contains(result.Matcher, "200") {
		t.Errorf("the status matcher = %+v", result)
	}
	result, err = Evaluate([]Matcher{{Type: "status", Status: []int{404}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("the status matcher matched the wrong code: %+v", result)
	}
}

// TestEvaluateSize: the body length, and not the header length.
func TestEvaluateSize(t *testing.T) {
	response := sampleResponse()
	matchers := []Matcher{{Type: "size", Size: []int{len(response.Body)}}}
	result, err := Evaluate(matchers, response)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Errorf("the size matcher did not match the body length %d: %+v", len(response.Body), result)
	}
	result, err = Evaluate([]Matcher{{Type: "size", Size: []int{1}}}, response)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("the size matcher matched the wrong length: %+v", result)
	}
}

// TestEvaluateBinary: the blob is decoded first and then looked for.
func TestEvaluateBinary(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("admin panel"))
	result, err := Evaluate([]Matcher{{Type: "binary", Binary: []string{encoded}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Errorf("the binary matcher did not match: %+v", result)
	}
	if _, err := Evaluate([]Matcher{{Type: "binary", Binary: []string{"not base64!!"}}}, sampleResponse()); err == nil {
		t.Error("a blob that is not base64 was accepted")
	}
}

// TestEvaluateNegativeInvertsTheResult: a negative matcher fires when the
// thing is absent.
func TestEvaluateNegativeInvertsTheResult(t *testing.T) {
	matchers := []Matcher{{Type: "word", Words: []string{"nothing here"}, Negative: true}}
	result, err := Evaluate(matchers, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Fatalf("the negative matcher did not fire: %+v", result)
	}
	if !strings.HasPrefix(result.Matcher, "negative ") {
		t.Errorf("the matcher does not say it is negative: %q", result.Matcher)
	}
	if !strings.Contains(result.Evidence, "absent") {
		t.Errorf("the evidence does not say the thing is absent: %q", result.Evidence)
	}
	matchers = []Matcher{{Type: "word", Words: []string{"admin panel"}, Negative: true}}
	result, err = Evaluate(matchers, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("the negative matcher fired on a word that is there: %+v", result)
	}
}

// TestEvaluateReadsTheChosenPart: the part decides what the matcher sees.
func TestEvaluateReadsTheChosenPart(t *testing.T) {
	response := sampleResponse()
	cases := []struct {
		part    string
		word    string
		matched bool
	}{
		{part: "header", word: "x-frame-options", matched: true},
		{part: "body", word: "x-frame-options", matched: false},
		{part: "url", word: "example.com", matched: true},
		{part: "url", word: "admin panel", matched: false},
		{part: "all", word: "content-type", matched: true},
		{part: "all", word: "admin panel", matched: true},
		{part: "", word: "admin panel", matched: true},
	}
	for _, c := range cases {
		matchers := []Matcher{{Type: "word", Part: c.part, Words: []string{c.word}}}
		result, err := Evaluate(matchers, response)
		if err != nil {
			t.Fatalf("Evaluate(%q): %v", c.part, err)
		}
		if result.Matched != c.matched {
			t.Errorf("part %q with the word %q matched = %v, want %v", c.part, c.word, result.Matched, c.matched)
		}
	}
}

// TestEvaluateGroupsTheEntriesByCondition: entries that ask for "or" are
// satisfied by one of them, and the rest all have to match.
func TestEvaluateGroupsTheEntriesByCondition(t *testing.T) {
	response := sampleResponse()
	matchers := []Matcher{
		{Type: "word", Words: []string{"admin panel"}, Condition: "or"},
		{Type: "word", Words: []string{"content-type"}, Part: "header"},
	}
	result, err := Evaluate(matchers, response)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Errorf("the or group did not satisfy the list: %+v", result)
	}
	matchers = []Matcher{
		{Type: "word", Words: []string{"admin panel"}, Condition: "or"},
		{Type: "word", Words: []string{"nothing here"}},
	}
	result, err = Evaluate(matchers, response)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("the list matched with an and entry that failed: %+v", result)
	}
}

// TestEvaluateRequestHonoursTheRequestCondition: the request level
// condition overrides the grouping of the list.
func TestEvaluateRequestHonoursTheRequestCondition(t *testing.T) {
	request := HTTPRequest{
		MatchersCondition: "or",
		Matchers: []Matcher{
			{Type: "word", Words: []string{"nothing here"}},
			{Type: "word", Words: []string{"admin panel"}},
		},
	}
	result, err := EvaluateRequest(request, sampleResponse())
	if err != nil {
		t.Fatalf("EvaluateRequest: %v", err)
	}
	if !result.Matched {
		t.Errorf("the request level or did not match: %+v", result)
	}
}

// TestEvaluateTheAndAndOrGroupTypes: the grouping entries with children.
func TestEvaluateTheAndAndOrGroupTypes(t *testing.T) {
	response := sampleResponse()
	and := Matcher{Type: "and", Matchers: []Matcher{
		{Type: "word", Words: []string{"admin panel"}},
		{Type: "status", Status: []int{200}},
	}}
	result, err := Evaluate([]Matcher{and}, response)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Errorf("the and group did not match: %+v", result)
	}
	or := Matcher{Type: "or", Matchers: []Matcher{
		{Type: "word", Words: []string{"nothing here"}},
		{Type: "word", Words: []string{"admin panel"}},
	}}
	result, err = Evaluate([]Matcher{or}, response)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Errorf("the or group did not match: %+v", result)
	}
}

// TestEvaluateADSLMatcher: the expression is parsed and evaluated, and the
// evidence names the expression and the value that decided it.
func TestEvaluateADSLMatcher(t *testing.T) {
	result, err := Evaluate([]Matcher{{Type: "dsl", DSL: []string{"status_code == 200"}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Fatalf("the dsl matcher did not match: %+v", result)
	}
	if !strings.Contains(result.Matcher, "status_code == 200") {
		t.Errorf("the matcher does not name the expression: %q", result.Matcher)
	}
	if !strings.Contains(result.Evidence, "status_code") {
		t.Errorf("the evidence does not carry the deciding value: %q", result.Evidence)
	}
	result, err = Evaluate([]Matcher{{Type: "dsl", DSL: []string{"status_code == 404"}}}, sampleResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Matched {
		t.Errorf("the dsl matcher matched a status that is not there: %+v", result)
	}
}

// TestEvaluateRefusesADSLMatcherItCannotEvaluate: a template that cannot be
// evaluated must not look like a clean miss.
func TestEvaluateRefusesADSLMatcherItCannotEvaluate(t *testing.T) {
	_, err := Evaluate([]Matcher{{Type: "dsl", DSL: []string{"nosuchfunction(body)"}}}, sampleResponse())
	if err == nil {
		t.Fatal("a dsl matcher with an unknown function was evaluated")
	}
	if !strings.Contains(err.Error(), "nosuchfunction") {
		t.Errorf("the error does not name the cause: %v", err)
	}
	_, err = Evaluate([]Matcher{{Type: "dsl", DSL: []string{"nosuchvariable == 1"}}}, sampleResponse())
	if err == nil {
		t.Fatal("a dsl matcher with an unknown variable was evaluated")
	}
	if !strings.Contains(err.Error(), "nosuchvariable") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestUnsupportedReasonNamesTheUnrunnableTemplates: the caller skips the
// template and says why.
func TestUnsupportedReasonNamesTheUnrunnableTemplates(t *testing.T) {
	if reason := UnsupportedReason([]Matcher{{Type: "dsl", DSL: []string{"status_code == 200"}}}); reason != "" {
		t.Errorf("a dsl template was named as unrunnable: %s", reason)
	}
	if reason := UnsupportedReason([]Matcher{{Type: "fuzzy", Words: []string{"x"}}}); reason == "" {
		t.Error("a matcher type this runner does not implement was not named")
	}
	if reason := UnsupportedReason([]Matcher{{Type: "word", Words: []string{"x"}}}); reason != "" {
		t.Errorf("a word template was named as unrunnable: %s", reason)
	}
	reason := UnsupportedReason([]Matcher{{Type: "word", Part: "header", Words: []string{"strict-transport-security"}}})
	if reason == "" {
		t.Error("a header the probe does not keep was not named as unrunnable")
	}
	if !strings.Contains(reason, "strict-transport-security") {
		t.Errorf("the reason does not name the header: %s", reason)
	}
}

// TestUnsupportedReasonNamesADSLExpressionThatDoesNotCompile: a template
// whose expression cannot be parsed is named once, before any request.
func TestUnsupportedReasonNamesADSLExpressionThatDoesNotCompile(t *testing.T) {
	reason := UnsupportedReason([]Matcher{{Type: "dsl", DSL: []string{"status_code =="}}})
	if reason == "" {
		t.Fatal("a dsl expression that does not compile was not named")
	}
	if !strings.Contains(reason, "status_code ==") {
		t.Errorf("the reason does not name the expression: %s", reason)
	}
}

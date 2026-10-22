package nuclei

import (
	"strconv"
	"strings"
	"testing"
)

// dslResponse is the response the dsl expressions are evaluated against.
func dslResponse() Response {
	return Response{
		Status: 200,
		Headers: map[string]string{
			"content-type": "text/html",
			"server":       "nginx",
			"set-cookie":   "session=abc123; Path=/",
		},
		Body:   []byte("<html><head><title>Admin Panel</title></head><body>Welcome to the admin panel</body></html>"),
		URL:    "https://example.com:8443/admin?debug=1",
		Method: "GET",
	}
}

// evalDSL evaluates an expression and fails the test when it cannot.
func evalDSL(t *testing.T, expr string, ctx DSLContext) DSLResult {
	t.Helper()
	result, err := EvaluateDSL(expr, ctx)
	if err != nil {
		t.Fatalf("EvaluateDSL(%q): %v", expr, err)
	}
	return result
}

// TestTheTokenizerReadsTheTokens: the shape of an expression is read before
// anything is decided about it.
func TestTheTokenizerReadsTheTokens(t *testing.T) {
	tokens, err := dslTokenize(`status_code == 200 && contains(body, "admin")`)
	if err != nil {
		t.Fatalf("dslTokenize: %v", err)
	}
	want := []struct {
		kind dslTokenKind
		text string
	}{
		{dslTokenIdent, "status_code"},
		{dslTokenOp, "=="},
		{dslTokenNumber, "200"},
		{dslTokenOp, "&&"},
		{dslTokenIdent, "contains"},
		{dslTokenLParen, "("},
		{dslTokenIdent, "body"},
		{dslTokenComma, ","},
		{dslTokenString, "admin"},
		{dslTokenRParen, ")"},
		{dslTokenEOF, ""},
	}
	if len(tokens) != len(want) {
		t.Fatalf("the tokenizer read %d tokens, want %d: %+v", len(tokens), len(want), tokens)
	}
	for i, expected := range want {
		if tokens[i].kind != expected.kind || tokens[i].text != expected.text {
			t.Errorf("token %d = %v %q, want %v %q", i, tokens[i].kind, tokens[i].text, expected.kind, expected.text)
		}
	}
}

// TestTheTokenizerTellsTheNegatedOperatorFromTheNegatedCall: `!contains`
// is an operator and `!contains(...)` is a negated call, and the corpus
// writes both.
func TestTheTokenizerTellsTheNegatedOperatorFromTheNegatedCall(t *testing.T) {
	tokens, err := dslTokenize(`body !contains "x"`)
	if err != nil {
		t.Fatalf("dslTokenize: %v", err)
	}
	if tokens[1].kind != dslTokenOp || tokens[1].text != "!contains" {
		t.Errorf("the negated operator was read as %v %q", tokens[1].kind, tokens[1].text)
	}
	tokens, err = dslTokenize(`!contains(body, "x")`)
	if err != nil {
		t.Fatalf("dslTokenize: %v", err)
	}
	if tokens[0].kind != dslTokenOp || tokens[0].text != "!" {
		t.Errorf("the negated call was read as %v %q", tokens[0].kind, tokens[0].text)
	}
	if tokens[1].kind != dslTokenIdent || tokens[1].text != "contains" {
		t.Errorf("the call was read as %v %q", tokens[1].kind, tokens[1].text)
	}
}

// TestTheParserHoldsThePrecedence: || binds looser than &&, and the
// comparison binds tighter than both.
func TestTheParserHoldsThePrecedence(t *testing.T) {
	ctx := DSLContext{Response: dslResponse()}
	cases := []struct {
		expr string
		want bool
	}{
		// || is looser than &&, so this is true || (false && false).
		{"status_code == 200 || status_code == 404 && body contains \"nothing\"", true},
		// The other reading would be false.
		{"(status_code == 200 || status_code == 404) && body contains \"nothing\"", false},
		// The comparison binds tighter than &&.
		{"status_code == 200 && body contains \"admin panel\"", true},
		{"status_code == 404 && body contains \"admin panel\"", false},
	}
	for _, c := range cases {
		result := evalDSL(t, c.expr, ctx)
		if result.Bool != c.want {
			t.Errorf("%q evaluated to %v, want %v", c.expr, result.Bool, c.want)
		}
	}
}

// TestTheParserReadsTheParenthesesAndTheUnaryNot: the grouping is the
// template's own.
func TestTheParserReadsTheParenthesesAndTheUnaryNot(t *testing.T) {
	ctx := DSLContext{Response: dslResponse()}
	cases := []struct {
		expr string
		want bool
	}{
		{"!(status_code == 404)", true},
		{"!(status_code == 200)", false},
		{"!false", true},
		{"!true", false},
		{"(status_code == 200)", true},
		{"((status_code == 200))", true},
	}
	for _, c := range cases {
		result := evalDSL(t, c.expr, ctx)
		if result.Bool != c.want {
			t.Errorf("%q evaluated to %v, want %v", c.expr, result.Bool, c.want)
		}
	}
}

// TestTheEvaluatorReadsTheResponse: the names of the language describe the
// response that came back.
func TestTheEvaluatorReadsTheResponse(t *testing.T) {
	response := dslResponse()
	ctx := DSLContext{Response: response}
	length := len(response.Body)
	cases := []struct {
		expr string
		want bool
	}{
		{"status_code == 200", true},
		{"content_length == " + strconv.Itoa(length), true},
		{"title == \"Admin Panel\"", true},
		{"title contains \"Admin\"", true},
		{"all_headers contains \"nginx\"", true},
		{"body contains \"admin panel\"", true},
		{"method == \"GET\"", true},
		{"host == \"example.com\"", true},
		{"path == \"/admin\"", true},
		{"query == \"debug=1\"", true},
		{"request contains \"GET /admin?debug=1\"", true},
		{"raw contains \"GET /admin?debug=1\"", true},
		{"response contains \"HTTP/1.1 200\"", true},
		{"response contains \"admin panel\"", true},
		{"content_type contains \"text/html\"", true},
	}
	for _, c := range cases {
		result := evalDSL(t, c.expr, ctx)
		if result.Bool != c.want {
			t.Errorf("%q evaluated to %v, want %v", c.expr, result.Bool, c.want)
		}
	}
}

// TestTheEvaluatorReadsTheSuppliedVariables: the template's own variables
// and the values an extractor found are names the expression may use.
func TestTheEvaluatorReadsTheSuppliedVariables(t *testing.T) {
	ctx := DSLContext{
		Response: dslResponse(),
		Vars:     map[string]string{"token": "admin panel"},
	}
	result := evalDSL(t, "token == \"admin panel\"", ctx)
	if !result.Bool {
		t.Errorf("the supplied variable was not read: %+v", result)
	}
	result = evalDSL(t, "contains(body, token)", ctx)
	if !result.Bool {
		t.Errorf("the supplied variable was not read inside a call: %+v", result)
	}
}

// TestEveryOperator: the operators the corpus uses, one by one.
func TestEveryOperator(t *testing.T) {
	ctx := DSLContext{Response: dslResponse()}
	cases := []struct {
		expr string
		want bool
	}{
		{"status_code == 200", true},
		{"status_code == 404", false},
		{"status_code != 200", false},
		{"status_code != 404", true},
		{`body contains "admin panel"`, true},
		{`body contains "nothing here"`, false},
		{`body !contains "admin panel"`, false},
		{`body !contains "nothing here"`, true},
		{`body matches "admin [a-z]+"`, true},
		{`body matches "nothing [a-z]+"`, false},
		{`body !matches "nothing [a-z]+"`, true},
		{"status_code > 199", true},
		{"status_code > 200", false},
		{"status_code < 201", true},
		{"status_code < 200", false},
		{"status_code >= 200", true},
		{"status_code >= 201", false},
		{"status_code <= 200", true},
		{"status_code <= 199", false},
		{`status_code == 200 && body contains "admin"`, true},
		{`status_code == 404 || body contains "admin"`, true},
		{`status_code == 404 || body contains "nothing"`, false},
	}
	for _, c := range cases {
		result := evalDSL(t, c.expr, ctx)
		if result.Bool != c.want {
			t.Errorf("%q evaluated to %v, want %v", c.expr, result.Bool, c.want)
		}
	}
}

// TestEveryFunction: the helpers the corpus reaches for.
func TestEveryFunction(t *testing.T) {
	ctx := DSLContext{Response: dslResponse()}
	cases := []struct {
		expr string
		want string
	}{
		{`contains(body, "admin panel")`, "true"},
		{`regex(body, "admin [a-z]+")`, "true"},
		{`regex(body, "nothing [a-z]+")`, "false"},
		{`len("")`, "0"},
		{`len("abc")`, "3"},
		{`md5("")`, "d41d8cd98f00b204e9800998ecf8427e"},
		{`sha1("")`, "da39a3ee5e6b4b0d3255bfef95601890afd80709"},
		{`sha256("")`, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{`base64("hello")`, "aGVsbG8="},
		{`base64_decode("aGVsbG8=")`, "hello"},
		{`to_lower("ABC")`, "abc"},
		{`to_upper("abc")`, "ABC"},
		{`lower("ABC")`, "abc"},
		{`upper("abc")`, "ABC"},
		{`concat("ab", "cd")`, "abcd"},
		{`starts_with("abcdef", "abc")`, "true"},
		{`starts_with("abcdef", "xyz")`, "false"},
		{`ends_with("abcdef", "def")`, "true"},
		{`ends_with("abcdef", "xyz")`, "false"},
		{`header_name("server")`, "nginx"},
		{`cookie("session")`, "abc123"},
	}
	for _, c := range cases {
		result := evalDSL(t, c.expr, ctx)
		if result.Value != c.want {
			t.Errorf("%q evaluated to %q, want %q", c.expr, result.Value, c.want)
		}
	}
}

// TestAParseErrorNamesThePosition: a template that does not parse has to
// say which part of it is wrong.
func TestAParseErrorNamesThePosition(t *testing.T) {
	_, err := ParseDSL("status_code ==")
	if err == nil {
		t.Fatal("an expression that does not parse was accepted")
	}
	if !strings.Contains(err.Error(), "status_code ==") {
		t.Errorf("the error does not name the expression: %v", err)
	}
	if !strings.Contains(err.Error(), "position") {
		t.Errorf("the error does not name the position: %v", err)
	}
	_, err = ParseDSL("status_code == 200 &&")
	if err == nil {
		t.Fatal("an expression that ends in an operator was accepted")
	}
	if !strings.Contains(err.Error(), "position") {
		t.Errorf("the error does not name the position: %v", err)
	}
}

// TestAnUnknownFunctionIsAnError: a function this engine does not know is
// a parse error, not a silent false.
func TestAnUnknownFunctionIsAnError(t *testing.T) {
	_, err := ParseDSL("nosuchfunction(body)")
	if err == nil {
		t.Fatal("an unknown function was accepted")
	}
	if !strings.Contains(err.Error(), "nosuchfunction") {
		t.Errorf("the error does not name the function: %v", err)
	}
}

// TestAnUnknownVariableIsAnError: a name nobody set is an error, not a
// silent false. A template that cannot be evaluated must not look like a
// clean miss.
func TestAnUnknownVariableIsAnError(t *testing.T) {
	_, err := EvaluateDSL("nosuchvariable == 1", DSLContext{Response: dslResponse()})
	if err == nil {
		t.Fatal("an unknown variable was accepted")
	}
	if !strings.Contains(err.Error(), "nosuchvariable") {
		t.Errorf("the error does not name the variable: %v", err)
	}
}

// TestABareHelperNameIsAnError: len and md5 take an argument, and using
// them as a value says so.
func TestABareHelperNameIsAnError(t *testing.T) {
	_, err := EvaluateDSL("md5", DSLContext{Response: dslResponse()})
	if err == nil {
		t.Fatal("a bare helper name was accepted")
	}
	if !strings.Contains(err.Error(), "md5") {
		t.Errorf("the error does not name the helper: %v", err)
	}
}

// TestTheWorkIsBounded: a hostile template must not be able to hang the
// run.
func TestTheWorkIsBounded(t *testing.T) {
	long := strings.Repeat("a", maxDSLExpressionRunes+1)
	_, err := ParseDSL(long)
	if err == nil {
		t.Fatal("an expression over the length ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("the error does not name the ceiling: %v", err)
	}

	// The node ceiling is reached before the length one: the expression
	// below is short and still holds more nodes than the ceiling allows.
	many := strings.Repeat("a||", 130) + "a"
	_, err = ParseDSL(many)
	if err == nil {
		t.Fatal("an expression over the node ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "nodes") {
		t.Errorf("the error does not name the node ceiling: %v", err)
	}

	deep := strings.Repeat("(", maxDSLDepth+10) + "true" + strings.Repeat(")", maxDSLDepth+10)
	_, err = ParseDSL(deep)
	if err == nil {
		t.Fatal("an expression over the depth ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "deeper") {
		t.Errorf("the error does not name the depth ceiling: %v", err)
	}
}

// TestTheEvidenceNamesTheExpressionAndTheValue: a finding has to say why.
func TestTheEvidenceNamesTheExpressionAndTheValue(t *testing.T) {
	result, err := Evaluate([]Matcher{{Type: "dsl", DSL: []string{"status_code == 200"}}}, dslResponse())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !result.Matched {
		t.Fatalf("the dsl matcher did not match: %+v", result)
	}
	if !strings.Contains(result.Matcher, "status_code == 200") {
		t.Errorf("the matcher does not name the expression: %q", result.Matcher)
	}
	if !strings.Contains(result.Evidence, "status_code = 200") {
		t.Errorf("the evidence does not carry the deciding value: %q", result.Evidence)
	}
}

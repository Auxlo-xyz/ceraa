package nuclei

import (
	"net/url"
	"strings"
	"testing"
)

// realTemplate is a Nuclei template in the shape the upstream corpus uses:
// the info block, a variables block, and an http entry with headers, a
// body, cookies, matchers and a payload.
const realTemplate = `id: exposed-token-endpoint

info:
  name: Exposed Token Endpoint
  author: ceraa
  severity: high
  description: A token endpoint is reachable without a session.
  tags: exposure,token
  reference:
    - https://cwe.mitre.org/data/definitions/306.html

variables:
  greeting: hello

http:
  - method: POST
    path:
      - "{{BaseURL}}/oauth/token"
    headers:
      Content-Type: application/json
      X-Trace: "{{greeting}}"
    body: '{"grant_type":"client_credentials"}'
    cookies:
      session: abc
    matchers:
      - type: word
        words:
          - "access_token"
        part: body
        case-insensitive: true
      - type: status
        status:
          - 200
        condition: and
    attack: single
    payload:
      user:
        - admin
`

// TestParseARealNucleiTemplate: the shape the upstream corpus uses has to
// survive the parser, field for field.
func TestParseARealNucleiTemplate(t *testing.T) {
	tpl, err := Parse([]byte(realTemplate), "templates/token.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tpl.ID != "exposed-token-endpoint" {
		t.Errorf("ID = %q", tpl.ID)
	}
	if tpl.Type != "http" {
		t.Errorf("Type = %q, want http", tpl.Type)
	}
	if tpl.Path != "templates/token.yaml" {
		t.Errorf("Path = %q", tpl.Path)
	}
	if tpl.Info.Name != "Exposed Token Endpoint" {
		t.Errorf("Info.Name = %q", tpl.Info.Name)
	}
	if tpl.Info.Severity != "high" {
		t.Errorf("Info.Severity = %q, want high", tpl.Info.Severity)
	}
	if tpl.Info.Description == "" {
		t.Error("Info.Description is empty")
	}
	if len(tpl.Info.Tags) != 2 || tpl.Info.Tags[0] != "exposure" || tpl.Info.Tags[1] != "token" {
		t.Errorf("Info.Tags = %v", tpl.Info.Tags)
	}
	if len(tpl.Info.Author) != 1 || tpl.Info.Author[0] != "ceraa" {
		t.Errorf("Info.Author = %v", tpl.Info.Author)
	}
	if len(tpl.Info.Reference) != 1 {
		t.Errorf("Info.Reference = %v", tpl.Info.Reference)
	}
	if tpl.Variables["greeting"] != "hello" {
		t.Errorf("Variables = %v", tpl.Variables)
	}
	if len(tpl.HTTP) != 1 {
		t.Fatalf("HTTP has %d entries, want 1", len(tpl.HTTP))
	}
	request := tpl.HTTP[0]
	if request.Method != "POST" {
		t.Errorf("Method = %q, want POST", request.Method)
	}
	if len(request.Path) != 1 || request.Path[0] != "{{BaseURL}}/oauth/token" {
		t.Errorf("Path = %v", request.Path)
	}
	if request.Headers["Content-Type"] != "application/json" {
		t.Errorf("Headers = %v", request.Headers)
	}
	if request.Body != `{"grant_type":"client_credentials"}` {
		t.Errorf("Body = %q", request.Body)
	}
	if request.Cookies["session"] != "abc" {
		t.Errorf("Cookies = %v", request.Cookies)
	}
	if request.AttackType != "single" {
		t.Errorf("AttackType = %q, want single", request.AttackType)
	}
	if request.Payload["user"] == nil {
		t.Errorf("Payload = %v", request.Payload)
	}
	if len(request.Matchers) != 2 {
		t.Fatalf("Matchers = %v", request.Matchers)
	}
	word := request.Matchers[0]
	if word.Type != "word" || len(word.Words) != 1 || word.Words[0] != "access_token" {
		t.Errorf("the word matcher = %+v", word)
	}
	if word.Part != "body" || !word.CaseInsensitive {
		t.Errorf("the word matcher = %+v", word)
	}
	status := request.Matchers[1]
	if status.Type != "status" || len(status.Status) != 1 || status.Status[0] != 200 {
		t.Errorf("the status matcher = %+v", status)
	}
	if status.Condition != "and" {
		t.Errorf("the status matcher condition = %q, want and", status.Condition)
	}
}

// TestParseRefusesATemplateWithoutAnID: a template with no id cannot be
// named in a report, so it does not load.
func TestParseRefusesATemplateWithoutAnID(t *testing.T) {
	_, err := Parse([]byte("info:\n  name: Nameless\nhttp:\n  - path: [\"{{BaseURL}}\"]\n"), "templates/nameless.yaml")
	if err == nil {
		t.Fatal("a template with no id loaded")
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestParseRefusesANonHTTPTemplate: the type is in the error, because a
// corpus holds one of every type and the operator has to know which ones
// were skipped.
func TestParseRefusesANonHTTPTemplate(t *testing.T) {
	network := `id: tcp-listener
info:
  name: TCP listener
  severity: info
network:
  - host: "{{BaseURL}}"
    ports:
      - 22
`
	_, err := Parse([]byte(network), "templates/tcp.yaml")
	if err == nil {
		t.Fatal("a network template loaded")
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("the error does not name the type: %v", err)
	}
}

// TestParseRefusesBrokenYAML: the file has to be nameable in the error.
func TestParseRefusesBrokenYAML(t *testing.T) {
	_, err := Parse([]byte("id: broken\nhttp:\n  - path: [\n"), "templates/broken.yaml")
	if err == nil {
		t.Fatal("broken YAML loaded")
	}
	if !strings.Contains(err.Error(), "templates/broken.yaml") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

// TestRenderExpandsTheBuiltInVariables: the names are Nuclei's own, and a
// template written against the upstream corpus uses them.
func TestRenderExpandsTheBuiltInVariables(t *testing.T) {
	target, err := url.Parse("https://example.com:8443/app")
	if err != nil {
		t.Fatal(err)
	}
	vars := TargetVars(target)
	for name, want := range map[string]string{
		"BaseURL":  "https://example.com:8443/app",
		"Host":     "example.com:8443",
		"Hostname": "example.com",
		"Port":     "8443",
		"Scheme":   "https",
		"Path":     "/app",
	} {
		if vars[name] != want {
			t.Errorf("vars[%s] = %q, want %q", name, vars[name], want)
		}
	}
	rendered, missing := Render("{{BaseURL}}/{{Path}}", vars)
	if rendered != "https://example.com:8443/app//app" {
		t.Errorf("Render = %q", rendered)
	}
	if len(missing) != 0 {
		t.Errorf("Render reported %v as missing", missing)
	}
}

// TestRenderExpandsTheHelpers: the helpers real templates reach for.
func TestRenderExpandsTheHelpers(t *testing.T) {
	target, err := url.Parse("https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	vars := TargetVars(target)
	cases := map[string]string{
		"{{to_lower(Hostname)}}":                         "example.com",
		"{{to_upper(Hostname)}}":                         "EXAMPLE.COM",
		"{{trim_space(\"  padded  \")}}":                 "padded",
		"{{replace(Hostname, \"example\", \"target\")}}": "target.com",
		"{{concat(Scheme, \"://\", Hostname)}}":          "https://example.com",
		"{{len(Hostname)}}":                              "11",
	}
	for input, want := range cases {
		rendered, missing := Render(input, vars)
		if rendered != want {
			t.Errorf("Render(%q) = %q, want %q", input, rendered, want)
		}
		if len(missing) != 0 {
			t.Errorf("Render(%q) reported %v as missing", input, missing)
		}
	}
}

// TestRenderAnUnknownVariableIsEmptyAndNamed: a template with a hole in it
// still runs, and the run says where the hole was.
func TestRenderAnUnknownVariableIsEmptyAndNamed(t *testing.T) {
	vars := TargetVars(mustURL(t, "https://example.com"))
	rendered, missing := Render("/{{NoSuchVariable}}/x", vars)
	if rendered != "//x" {
		t.Errorf("Render = %q, want the unknown variable to render empty", rendered)
	}
	if len(missing) != 1 || missing[0] != "NoSuchVariable" {
		t.Errorf("missing = %v, want the unknown variable named", missing)
	}
	if _, missing := Render("{{to_lower(NoSuchVariable)}}", vars); len(missing) != 1 || missing[0] != "NoSuchVariable" {
		t.Errorf("missing = %v, want the unknown variable named through a helper", missing)
	}
}

// TestRenderTheRandomValuesChangeEveryTime: a nonce that repeats is not a
// nonce.
func TestRenderTheRandomValuesChangeEveryTime(t *testing.T) {
	vars := TargetVars(mustURL(t, "https://example.com"))
	first, _ := Render("{{RandStr}}", vars)
	second, _ := Render("{{RandStr}}", vars)
	if first == second {
		t.Errorf("RandStr repeated: %q", first)
	}
	if len([]rune(first)) != defaultRandStrLength {
		t.Errorf("RandStr = %q, want %d characters", first, defaultRandStrLength)
	}
	long, _ := Render("{{RandStr:12}}", vars)
	if len([]rune(long)) != 12 {
		t.Errorf("RandStr:12 = %q, want 12 characters", long)
	}
	uuid, _ := Render("{{uuid}}", vars)
	if !looksLikeUUID(uuid) {
		t.Errorf("uuid = %q", uuid)
	}
	other, _ := Render("{{uuid}}", vars)
	if uuid == other {
		t.Errorf("uuid repeated: %q", uuid)
	}
}

// TestRenderUsesTheTemplateVariables: the variables block is what other
// requests in the same template may reference.
func TestRenderUsesTheTemplateVariables(t *testing.T) {
	tpl, err := Parse([]byte(`id: with-variables
info:
  name: With variables
  severity: info
variables:
  base: /api
  call: "{{base}}/v1"
http:
  - method: GET
    path:
      - "{{BaseURL}}{{call}}"
`), "templates/vars.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	vars := mergedVars(mustURL(t, "https://example.com"), tpl)
	rendered, missing := Render(tpl.HTTP[0].Path[0], vars)
	if rendered != "https://example.com/api/v1" {
		t.Errorf("Render = %q", rendered)
	}
	if len(missing) != 0 {
		t.Errorf("Render reported %v as missing", missing)
	}
}

// mustURL parses a url or fails the test.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url %q: %v", raw, err)
	}
	return parsed
}

// looksLikeUUID reports whether a string is shaped like a uuid v4.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for _, index := range []int{8, 13, 18, 23} {
		if s[index] != '-' {
			return false
		}
	}
	return s[14] == '4'
}

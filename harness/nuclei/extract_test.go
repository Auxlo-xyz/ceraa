package nuclei

import (
	"strings"
	"testing"
)

// extractResponse is the response the extractors are run against.
func extractResponse(body string) Response {
	return Response{
		Status:  200,
		Headers: map[string]string{"content-type": "application/json"},
		Body:    []byte(body),
		URL:     "https://example.com/login",
		Method:  "POST",
	}
}

// TestARegexExtractorTakesTheCaptureGroup: the group is what the template
// asked for.
func TestARegexExtractorTakesTheCaptureGroup(t *testing.T) {
	specs := []Extractor{{
		Type:  "regex",
		Regex: []string{`token=([a-z0-9]+)`},
		Name:  "token",
		Group: 1,
	}}
	values, err := Extract(specs, extractResponse("login ok token=abc123&next=/home"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := values["token"]
	if len(found) != 1 || found[0] != "abc123" {
		t.Errorf("the extractor found %v, want the capture group", found)
	}
}

// TestARegexExtractorWithoutAGroupTakesTheWholeMatch: a pattern with no
// group has nothing else to give.
func TestARegexExtractorWithoutAGroupTakesTheWholeMatch(t *testing.T) {
	specs := []Extractor{{
		Type:  "regex",
		Regex: []string{`token=[a-z0-9]+`},
		Name:  "token",
	}}
	values, err := Extract(specs, extractResponse("login ok token=abc123&next=/home"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := values["token"]
	if len(found) != 1 || found[0] != "token=abc123" {
		t.Errorf("the extractor found %v, want the whole match", found)
	}
}

// TestARegexExtractorRefusesAGroupThatIsNotThere: a template that asks for
// a group the pattern does not have is a template that cannot run.
func TestARegexExtractorRefusesAGroupThatIsNotThere(t *testing.T) {
	specs := []Extractor{{
		Type:  "regex",
		Regex: []string{`token=([a-z0-9]+)`},
		Name:  "token",
		Group: 4,
	}}
	_, err := Extract(specs, extractResponse("token=abc123"))
	if err == nil {
		t.Fatal("a group the pattern does not have was accepted")
	}
	if !strings.Contains(err.Error(), "group") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestAKvalExtractorReadsThePair: the key=value pair of a body.
func TestAKvalExtractorReadsThePair(t *testing.T) {
	specs := []Extractor{{
		Type: "kval",
		Kval: []string{"APP_KEY"},
		Name: "app_key",
	}}
	values, err := Extract(specs, extractResponse("APP_KEY=base64:something\nOTHER=thing\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := values["app_key"]
	if len(found) != 1 || found[0] != "base64:something" {
		t.Errorf("the extractor found %v, want the value of the key", found)
	}
}

// TestAKvalExtractorWithoutAKeyTakesEveryPair: a body of configuration keys
// carries all of them.
func TestAKvalExtractorWithoutAKeyTakesEveryPair(t *testing.T) {
	specs := []Extractor{{Type: "kval", Name: "pairs"}}
	values, err := Extract(specs, extractResponse("APP_KEY=one\nOTHER=two\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := values["pairs"]
	if len(found) != 2 || found[0] != "one" || found[1] != "two" {
		t.Errorf("the extractor found %v, want both values", found)
	}
}

// TestAJSONExtractorWalksThePath: the dotted path of a json body.
func TestAJSONExtractorWalksThePath(t *testing.T) {
	specs := []Extractor{{
		Type: "json",
		JSON: []string{"$.items[0].id"},
		Name: "item_id",
	}}
	values, err := Extract(specs, extractResponse(`{"items":[{"id":"42","name":"first"}]}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := values["item_id"]
	if len(found) != 1 || found[0] != "42" {
		t.Errorf("the extractor found %v, want the value at the path", found)
	}
}

// TestAJSONExtractorNamesABodyThatIsNotJSON: a body that cannot be parsed
// is an error, not an empty result.
func TestAJSONExtractorNamesABodyThatIsNotJSON(t *testing.T) {
	specs := []Extractor{{Type: "json", JSON: []string{"$.id"}, Name: "id"}}
	_, err := Extract(specs, extractResponse("this is not json"))
	if err == nil {
		t.Fatal("a body that is not json was accepted")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestADslExtractorTakesTheValue: the expression's value is the value, and
// a boolean is not a value a later request can send.
func TestADslExtractorTakesTheValue(t *testing.T) {
	specs := []Extractor{{
		Type: "dsl",
		DSL:  []string{"status_code"},
		Name: "code",
	}}
	values, err := Extract(specs, extractResponse("body"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := values["code"]
	if len(found) != 1 || found[0] != "200" {
		t.Errorf("the extractor found %v, want the status code", found)
	}
	specs = []Extractor{{Type: "dsl", DSL: []string{"status_code == 200"}, Name: "matched"}}
	values, err = Extract(specs, extractResponse("body"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(values["matched"]) != 0 {
		t.Errorf("a boolean was stored as a value: %v", values["matched"])
	}
}

// TestTheValueCountIsBounded: an extractor that matches a thousand times
// has one useful answer.
func TestTheValueCountIsBounded(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString("id=abc\n")
	}
	specs := []Extractor{{Type: "regex", Regex: []string{`id=([a-z]+)`}, Name: "ids"}}
	values, err := Extract(specs, extractResponse(body.String()))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(values["ids"]) != maxExtractedValues {
		t.Errorf("the extractor stored %d values, want the ceiling of %d", len(values["ids"]), maxExtractedValues)
	}
}

// TestTheExtractedValueFeedsTheRenderer: the point of an extractor is that
// a later request can send what it found.
func TestTheExtractedValueFeedsTheRenderer(t *testing.T) {
	specs := []Extractor{{
		Type:  "regex",
		Regex: []string{`token=([a-z0-9]+)`},
		Name:  "token",
		Group: 1,
	}}
	values, err := Extract(specs, extractResponse("token=abc123"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	vars := Vars{}
	for name, list := range values {
		if len(list) > 0 {
			vars[name] = list[0]
		}
	}
	rendered, missing := Render("/session?token={{token}}", vars)
	if rendered != "/session?token=abc123" {
		t.Errorf("Render = %q, want the extracted value", rendered)
	}
	if len(missing) != 0 {
		t.Errorf("Render reported %v as missing", missing)
	}
}

// TestAnExtractorThatCannotRunDoesNotCostTheOthers: one broken extractor
// must not cost the values the others found.
func TestAnExtractorThatCannotRunDoesNotCostTheOthers(t *testing.T) {
	specs := []Extractor{
		{Type: "regex", Regex: []string{`token=([a-z0-9]+)`}, Name: "token", Group: 1},
		{Type: "regex", Regex: []string{`([unclosed`}, Name: "broken"},
	}
	values, err := Extract(specs, extractResponse("token=abc123"))
	if err == nil {
		t.Fatal("a regex that does not compile was accepted")
	}
	if len(values["token"]) != 1 || values["token"][0] != "abc123" {
		t.Errorf("the values that were found were lost: %v", values)
	}
}

package nuclei

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/recon"
)

// hermeticScope points the scope record at a temp directory, so the gate
// runs observation-grade and the test never reads the operator's record.
func hermeticScope(t *testing.T) {
	t.Helper()
	t.Setenv("CERAA_HOME", t.TempDir())
}

// fakeProbe answers every request with the same canned response.
func fakeProbe(status int, headers map[string]string, body string) ProbeFunc {
	return func(_ context.Context, req recon.Request) (recon.Result, []byte, error) {
		return recon.Result{
			URL:       req.URL,
			Method:    req.Method,
			Status:    status,
			Headers:   headers,
			BodyBytes: len(body),
			SHA256:    "0123456789abcdef",
			ReceiptID: "receipt-1.json",
		}, []byte(body), nil
	}
}

// countingProbe counts the requests it answered.
func countingProbe(inner ProbeFunc) (ProbeFunc, func() int) {
	var (
		mu    sync.Mutex
		calls int
	)
	wrapped := func(ctx context.Context, req recon.Request) (recon.Result, []byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return inner(ctx, req)
	}
	return wrapped, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// wordTemplate is a template that matches one word in the body.
func wordTemplate(id string) Template {
	return Template{
		ID:   id,
		Info: Info{Name: "Word template", Severity: "high"},
		Type: "http",
		Path: "templates/" + id + ".yaml",
		HTTP: []HTTPRequest{{
			Method:   "GET",
			Path:     []string{"{{BaseURL}}/probe"},
			Matchers: []Matcher{{Type: "word", Words: []string{"admin panel"}}},
		}},
	}
}

// TestRunProducesAFindingWithTheReason: a finding names the matcher that
// fired, carries the evidence and cites the receipt.
func TestRunProducesAFindingWithTheReason(t *testing.T) {
	hermeticScope(t)
	findings, err := Run(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{wordTemplate("word-one")},
		Probe:     fakeProbe(200, map[string]string{"content-type": "text/html"}, "<b>admin panel</b>"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	finding := findings[0]
	if finding.TemplateID != "word-one" {
		t.Errorf("TemplateID = %q", finding.TemplateID)
	}
	if finding.Severity != "high" {
		t.Errorf("Severity = %q", finding.Severity)
	}
	if finding.Status != 200 {
		t.Errorf("Status = %d", finding.Status)
	}
	if finding.ReceiptID != "receipt-1.json" {
		t.Errorf("ReceiptID = %q", finding.ReceiptID)
	}
	if finding.Source != "templates/word-one.yaml" {
		t.Errorf("Source = %q", finding.Source)
	}
	if !strings.Contains(finding.Matcher, "admin panel") {
		t.Errorf("the matcher does not say what it matched: %q", finding.Matcher)
	}
	if !strings.Contains(finding.Evidence, "admin panel") {
		t.Errorf("the evidence does not carry the match: %q", finding.Evidence)
	}
	if !strings.Contains(finding.Request, "https://example.com/probe") {
		t.Errorf("the recorded request does not name the url: %q", finding.Request)
	}
}

// TestRunRedactsTheCredentialHeaders: a finding that quotes a cookie value
// has defeated the redaction the rest of the harness keeps.
func TestRunRedactsTheCredentialHeaders(t *testing.T) {
	hermeticScope(t)
	tpl := wordTemplate("word-one")
	tpl.HTTP[0].Headers = map[string]string{
		"Cookie":        "session=s3cr3t-value",
		"Authorization": "Bearer s3cr3t-token",
		"X-Trace":       "keep-me",
	}
	findings, err := Run(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{tpl},
		Probe:     fakeProbe(200, nil, "admin panel"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	request := findings[0].Request
	if strings.Contains(request, "s3cr3t-value") || strings.Contains(request, "s3cr3t-token") {
		t.Errorf("the recorded request carries a credential: %q", request)
	}
	if !strings.Contains(request, "REDACTED") {
		t.Errorf("the recorded request does not say the value was redacted: %q", request)
	}
	if !strings.Contains(request, "keep-me") {
		t.Errorf("the recorded request dropped a harmless header: %q", request)
	}
}

// TestRunHonoursTheRequestCeiling: the ceiling is on the requests that went
// out, not on the templates.
func TestRunHonoursTheRequestCeiling(t *testing.T) {
	hermeticScope(t)
	probe, calls := countingProbe(fakeProbe(200, nil, "admin panel"))
	templates := []Template{
		wordTemplate("one"), wordTemplate("two"), wordTemplate("three"), wordTemplate("four"),
	}
	report, err := RunDetailed(context.Background(), Options{
		Target:      "https://example.com",
		Templates:   templates,
		MaxRequests: 2,
		// One worker, so the feed loop is left waiting for a worker that
		// has gone home once the ceiling is reached.
		Concurrency: 1,
		Probe:       probe,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if report.Requests != 2 {
		t.Errorf("the run fired %d requests, want 2", report.Requests)
	}
	if calls() != 2 {
		t.Errorf("the probe answered %d requests, want 2", calls())
	}
	if report.Stopped == "" {
		t.Error("the run did not say that it stopped at the ceiling")
	}
}

// TestRunHonoursTheFindingCeiling: the run stops at the ceiling instead of
// collecting a pile nobody reads.
func TestRunHonoursTheFindingCeiling(t *testing.T) {
	hermeticScope(t)
	report, err := RunDetailed(context.Background(), Options{
		Target:      "https://example.com",
		Templates:   []Template{wordTemplate("one"), wordTemplate("two")},
		MaxFindings: 1,
		Probe:       fakeProbe(200, nil, "admin panel"),
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Errorf("the run collected %d findings, want 1", len(report.Findings))
	}
}

// TestRunPacesTheRequestsAtTheRate: the limiter is what keeps a template
// corpus from looking like a flood.
func TestRunPacesTheRequestsAtTheRate(t *testing.T) {
	hermeticScope(t)
	start := time.Now()
	_, err := Run(context.Background(), Options{
		Target:      "https://example.com",
		Templates:   []Template{wordTemplate("one"), wordTemplate("two"), wordTemplate("three")},
		RateLimit:   10,
		Concurrency: 3,
		Probe:       fakeProbe(200, nil, "admin panel"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	// Ten a second is a token every hundred milliseconds, so three requests
	// cannot finish before the second token arrives.
	if elapsed < 150*time.Millisecond {
		t.Errorf("three requests at ten a second took %s, want the pacing to apply", elapsed)
	}
}

// TestRunReportsATemplateWhoseRequestsAllFailed: a template that could not
// reach the target did not find anything, and saying so is the difference
// between a run that found nothing and a run that never ran.
func TestRunReportsATemplateWhoseRequestsAllFailed(t *testing.T) {
	hermeticScope(t)
	failing := func(_ context.Context, _ recon.Request) (recon.Result, []byte, error) {
		return recon.Result{}, nil, fmt.Errorf("connection refused")
	}
	report, err := RunDetailed(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{wordTemplate("one")},
		Probe:     failing,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("a failed request produced %d findings", len(report.Findings))
	}
	joined := strings.Join(report.Errors, "\n")
	if !strings.Contains(joined, "every request failed") {
		t.Errorf("the errors do not say the template never ran: %v", report.Errors)
	}
	if !strings.Contains(joined, "connection refused") {
		t.Errorf("the errors do not carry the cause: %v", report.Errors)
	}
}

// TestRunSkipsATemplateItCannotRun: an unrunnable template is named, not
// quietly dropped.
func TestRunSkipsATemplateItCannotRun(t *testing.T) {
	hermeticScope(t)
	dsl := wordTemplate("dsl-one")
	dsl.HTTP[0].Matchers = []Matcher{{Type: "dsl", DSL: []string{"status_code == 200"}}}
	probe, calls := countingProbe(fakeProbe(200, nil, "admin panel"))
	report, err := RunDetailed(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{dsl},
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if calls() != 0 {
		t.Errorf("the probe answered %d requests for a template that cannot run", calls())
	}
	if report.Skipped != 1 {
		t.Errorf("the run skipped %d templates, want 1", report.Skipped)
	}
	if !strings.Contains(strings.Join(report.SkipReasons, "\n"), "dsl") {
		t.Errorf("the skip reason does not name the cause: %v", report.SkipReasons)
	}
}

// TestRunFiltersBySeverity: a filter is a filter, and what it drops is
// named.
func TestRunFiltersBySeverity(t *testing.T) {
	hermeticScope(t)
	probe, calls := countingProbe(fakeProbe(200, nil, "admin panel"))
	report, err := RunDetailed(context.Background(), Options{
		Target:     "https://example.com",
		Templates:  []Template{wordTemplate("high-one")},
		Severities: []string{"low"},
		Probe:      probe,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if calls() != 0 {
		t.Errorf("the probe answered %d requests for a filtered out template", calls())
	}
	if report.Skipped != 1 {
		t.Errorf("the run skipped %d templates, want 1", report.Skipped)
	}
	if !strings.Contains(strings.Join(report.SkipReasons, "\n"), "severity") {
		t.Errorf("the skip reason does not name the filter: %v", report.SkipReasons)
	}
}

// TestRunNamesTheVariablesThatRenderedEmpty: a template with a hole in it
// still runs, and the finding says where the hole was.
func TestRunNamesTheVariablesThatRenderedEmpty(t *testing.T) {
	hermeticScope(t)
	tpl := wordTemplate("hole")
	tpl.HTTP[0].Path = []string{"{{BaseURL}}/{{NoSuchVariable}}"}
	findings, err := Run(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{tpl},
		Probe:     fakeProbe(200, nil, "admin panel"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	if len(findings[0].EmptyVars) != 1 || findings[0].EmptyVars[0] != "NoSuchVariable" {
		t.Errorf("EmptyVars = %v, want the unknown variable named", findings[0].EmptyVars)
	}
}

// TestRunSortsTheFindingsBySeverity: two runs read the same.
func TestRunSortsTheFindingsBySeverity(t *testing.T) {
	hermeticScope(t)
	low := wordTemplate("low-one")
	low.Info.Severity = "low"
	critical := wordTemplate("critical-one")
	critical.Info.Severity = "critical"
	findings, err := Run(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{low, critical},
		Probe:     fakeProbe(200, nil, "admin panel"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}
	if findings[0].Severity != "critical" {
		t.Errorf("the findings are not ordered by severity: %+v", findings)
	}
}

// TestRunReadsATargetListFromAFile: the operator's list is the operator's
// list.
func TestRunReadsATargetListFromAFile(t *testing.T) {
	hermeticScope(t)
	dir := t.TempDir()
	list := dir + "/targets.txt"
	if err := os.WriteFile(list, []byte("https://example.com\n# a comment\n\nhttps://example.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe, calls := countingProbe(fakeProbe(200, nil, "admin panel"))
	report, err := RunDetailed(context.Background(), Options{
		Target:    list,
		Templates: []Template{wordTemplate("one")},
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	// One request per target, plus the one root read that lets a catch-all
	// response be named.
	if calls() != 3 {
		t.Errorf("the probe answered %d requests, want one per target plus the root read", calls())
	}
	if len(report.Findings) != 2 {
		t.Errorf("the run collected %d findings, want 2", len(report.Findings))
	}
}

// TestTheEmbeddedCorpusRunsAgainstACannedResponse: the shipped templates
// have to be templates the runner can actually run, not just YAML that
// parses.
func TestTheEmbeddedCorpusRunsAgainstACannedResponse(t *testing.T) {
	hermeticScope(t)
	templates, errs := LoadEmbedded()
	if len(errs) != 0 {
		t.Fatalf("the embedded set produced errors: %v", errs)
	}
	body := strings.Join([]string{
		"ref: refs/heads/main",
		"APP_KEY=base64:something",
		`"name": "demo"`,
		"name = \"worker\"",
		"User-agent: *",
		"Index of /uploads",
		`type="password"`,
		"phpinfo()",
		"<?php",
		`"openapi": "3.0.0"`,
	}, "\n")
	headers := map[string]string{
		"content-type":                "text/html",
		"access-control-allow-origin": "https://ceraa-origin-probe.example",
	}
	report, err := RunDetailed(context.Background(), Options{
		Target:    "https://example.com",
		Templates: templates,
		Probe:     fakeProbe(200, headers, body),
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Errorf("the embedded corpus produced errors: %v", report.Errors)
	}
	if report.Requests == 0 {
		t.Error("the embedded corpus fired no requests")
	}
	fired := map[string]bool{}
	for _, finding := range report.Findings {
		fired[finding.TemplateID] = true
	}
	for _, want := range []string{
		"exposed-git-directory", "exposed-env-file", "exposed-package-json",
		"exposed-wrangler-toml", "robots-txt-present", "cors-origin-reflection",
		"login-form-password-field", "phpinfo-page", "exposed-backup-file",
		"exposed-directory-listing", "exposed-openapi-document",
	} {
		if !fired[want] {
			t.Errorf("the embedded template %q did not match a response that carries its marker", want)
		}
	}
	// The HSTS template is skipped: the harness's probe does not keep that
	// header, and a check that cannot see the header would report every
	// site as missing it.
	if fired["missing-hsts-header"] {
		t.Error("the HSTS template matched a header the harness cannot see")
	}
	if !strings.Contains(strings.Join(report.SkipReasons, "\n"), "strict-transport-security") {
		t.Errorf("the skip reasons do not name the HSTS template: %v", report.SkipReasons)
	}
}

// TestAFindingOnTheCatchAllBodyIsMarked covers the single page application
// case: the same document answers every path, so a word matcher fires on the
// shell. The finding has to say so, because a reader who cannot see it will
// report a false positive.
func TestAFindingOnTheCatchAllBodyIsMarked(t *testing.T) {
	hermeticScope(t)
	probe := func(ctx context.Context, req recon.Request) (recon.Result, []byte, error) {
		// The root and the deep path answer with the same bytes.
		return recon.Result{URL: req.URL, Status: 200, SHA256: "samehash", BodyBytes: 10}, []byte("admin panel"), nil
	}
	findings, err := Run(context.Background(), Options{
		Target:    "https://example.com",
		Templates: []Template{wordTemplate("one")},
		Probe:     probe,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	if findings[0].CatchAll == "" {
		t.Error("a finding on the catch-all body was not marked")
	}
}

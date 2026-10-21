package findings

// report_test.go covers the disclosure side: the CVSS arithmetic a severity
// claim rests on, and the two renderings of the record.
//
// The CVSS scores are pinned against the published v3.1 examples. A severity
// that is off by a tenth is not a rounding error in a report, it is a number
// a triager checks.

import (
	"strings"
	"testing"
)

func TestCVSSScoreMatchesThePublishedExamples(t *testing.T) {
	cases := []struct {
		vector string
		want   float64
	}{
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		{"CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", 7.8},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1},
		{"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:N/A:N", 5.9},
		{"CVSS:3.1/AV:L/AC:H/PR:H/UI:R/S:U/C:N/I:N/A:N", 0.0},
	}
	for _, tc := range cases {
		got, err := CVSSScore(tc.vector)
		if err != nil {
			t.Errorf("CVSSScore(%q): %v", tc.vector, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CVSSScore(%q) = %v, want %v", tc.vector, got, tc.want)
		}
	}
}

// The privilege required is the one metric whose weight follows the scope.
// The same privileges are worth more when the attack crosses a boundary, and
// a score that ignored that would understate every scoped finding.
func TestThePrivilegeWeightFollowsTheScope(t *testing.T) {
	unchanged, err := CVSSScore("CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H")
	if err != nil {
		t.Fatalf("score the unchanged scope: %v", err)
	}
	changed, err := CVSSScore("CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H")
	if err != nil {
		t.Fatalf("score the changed scope: %v", err)
	}
	if unchanged != 8.8 {
		t.Errorf("the unchanged scope scored %v, want 8.8", unchanged)
	}
	if changed != 9.9 {
		t.Errorf("the changed scope scored %v, want 9.9", changed)
	}
}

func TestCVSSScoreRefusesAVectorItCannotRead(t *testing.T) {
	cases := []string{
		"",
		"CVSS:3.1/AV:Z/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H",
		"CVSS:3.1/AV:N/AC:L/PR:X/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:X/C:H/I:H/A:H",
		"CVSS:2.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H/AV:N",
		"nonsense",
	}
	for _, vector := range cases {
		if score, err := CVSSScore(vector); err == nil {
			t.Errorf("CVSSScore(%q) = %v, want an error", vector, score)
		}
	}
}

func TestTheMetricsVectorRoundTrips(t *testing.T) {
	metrics, err := ParseVector("CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	back, err := ParseVector(metrics.Vector())
	if err != nil {
		t.Fatalf("parse the vector back: %v", err)
	}
	if back != metrics {
		t.Errorf("the vector round trip changed the metrics: %+v then %+v", metrics, back)
	}
}

// The eight metrics on their own are the score; the version prefix is a
// label, not a requirement.
func TestTheEightMetricsAreAcceptedOnTheirOwn(t *testing.T) {
	score, err := CVSSScore("AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if score != 9.8 {
		t.Errorf("score = %v, want 9.8", score)
	}
}

func sampleCoverage() Coverage {
	return Coverage{
		Total:      3,
		ByStatus:   map[Status]int{StatusConfirmed: 1, StatusRuledOut: 1, StatusOpenProofGap: 1},
		BySeverity: map[string]int{SeverityHigh: 1, SeverityInfo: 1, SeverityLow: 1},
		OpenGaps:   []string{"Missing proof that /admin is unauthenticated"},
		RuledOut:   []string{"Reflected parameter in the search box"},
	}
}

func sampleFindings() []Finding {
	return []Finding{
		{
			ID:          "sql-injection-login-a1b2c3d4",
			Title:       "SQL injection in the login form",
			Severity:    SeverityHigh,
			Status:      StatusConfirmed,
			URL:         "https://example.com/login",
			Endpoint:    "POST /login",
			Description: "The username parameter is concatenated into a query.",
			PoC:         "curl -s 'https://example.com/login' -d \"u=' OR 1=1--\"",
			Evidence:    "Two rows came back for one username.",
			Remediation: "Use a parameterised query.",
			CVSSVector:  "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
			CVSSScore:   9.8,
			Tags:        []string{"injection"},
		},
		{
			ID:       "reflected-search-e5f6a7b8",
			Title:    "Reflected parameter in the search box",
			Severity: SeverityInfo,
			Status:   StatusRuledOut,
			URL:      "https://example.com/search",
			Evidence: "The response sets Content-Type: text/plain.",
		},
		{
			ID:       "admin-unauth-c9d0e1f2",
			Title:    "Missing proof that /admin is unauthenticated",
			Severity: SeverityLow,
			Status:   StatusOpenProofGap,
			URL:      "https://example.com/admin",
		},
	}
}

func TestTheMarkdownCarriesTheGapsAndTheRuledOut(t *testing.T) {
	out := RenderMarkdown(sampleFindings(), sampleCoverage())
	for _, want := range []string{
		"# Security finding report",
		"| Title | Severity | Status | URL |",
		"### SQL injection in the login form",
		"curl -s 'https://example.com/login' -d \"u=' OR 1=1--\"",
		"9.8 (CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H)",
		"## Open proof gaps",
		"## Ruled out",
		"## Coverage",
		"findings: 3",
		"open_proof_gap: 1",
		"ruled_out: 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is missing %q:\n%s", want, out)
		}
	}
}

// A field that is empty is skipped, and a finding with nothing but a title
// still renders as a title rather than as an empty section.
func TestTheMarkdownSkipsTheEmptyFields(t *testing.T) {
	out := RenderMarkdown([]Finding{{ID: "bare-a1b2c3d4", Title: "Only a title", Severity: SeverityMedium, Status: StatusConfirmed}}, Coverage{
		Total:      1,
		ByStatus:   map[Status]int{StatusConfirmed: 1},
		BySeverity: map[string]int{SeverityMedium: 1},
	})
	if !strings.Contains(out, "### Only a title") {
		t.Errorf("the finding did not render:\n%s", out)
	}
	for _, absent := range []string{"Evidence:", "Counterevidence:", "Remediation:", "Proof of concept:", "- url:", "- endpoint:"} {
		if strings.Contains(out, absent) {
			t.Errorf("an empty field was rendered as %q:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "None recorded.") {
		t.Errorf("the empty gap and ruled-out sections did not say so:\n%s", out)
	}
}

// A pipe in a title would end the table cell early and shift every column
// after it.
func TestTheMarkdownTableSurvivesAPipeInATitle(t *testing.T) {
	out := RenderMarkdown([]Finding{{ID: "pipe-a1b2c3d4", Title: "Header | injection", Severity: SeverityLow, Status: StatusConfirmed}}, Coverage{
		Total:      1,
		ByStatus:   map[Status]int{StatusConfirmed: 1},
		BySeverity: map[string]int{SeverityLow: 1},
	})
	if !strings.Contains(out, "Header \\| injection") {
		t.Errorf("the pipe was not escaped:\n%s", out)
	}
}

func TestTheHTMLCarriesTheGapsAndTheRuledOut(t *testing.T) {
	out := RenderHTML(sampleFindings(), sampleCoverage())
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<h2>Open proof gaps</h2>",
		"<h2>Ruled out</h2>",
		"<h2>Coverage</h2>",
		"SQL injection in the login form",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is missing %q", want)
		}
	}
}

// Everything a finding carries is model-written text, so the HTML rendering
// escapes it. A report that renders a finding raw can carry a tag.
func TestTheHTMLEscapesWhatTheFindingCarries(t *testing.T) {
	out := RenderHTML([]Finding{{
		ID:       "xss-a1b2c3d4",
		Title:    "<script>alert(1)</script>",
		Severity: SeverityHigh,
		Status:   StatusConfirmed,
		URL:      "https://example.com/?q=<script>",
		PoC:      "<img src=x onerror=alert(1)>",
	}}, Coverage{
		Total:      1,
		ByStatus:   map[Status]int{StatusConfirmed: 1},
		BySeverity: map[string]int{SeverityHigh: 1},
	})
	if strings.Contains(out, "<script>") || strings.Contains(out, "<img src=x") {
		t.Errorf("the report rendered a finding raw:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("the report did not escape the finding:\n%s", out)
	}
}

// The document is a file somebody opens, so it carries no script and nothing
// to fetch.
func TestTheHTMLHasNoScriptAndNothingToFetch(t *testing.T) {
	out := RenderHTML(sampleFindings(), sampleCoverage())
	for _, absent := range []string{"<script", "javascript:", "src=", "<link", "@import"} {
		if strings.Contains(out, absent) {
			t.Errorf("the report carries %q", absent)
		}
	}
}

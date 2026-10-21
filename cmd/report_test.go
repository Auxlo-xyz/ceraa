package cmd

// report_test.go covers the operator's view of the finding record.
//
// The command is exercised through its runner rather than through cobra's
// Execute, because Execute on a subcommand runs the root, and the root is
// not what is under test here.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/findings"
)

// findingRecord writes a small record into a scratch data directory and
// returns the store that holds it.
func findingRecord(t *testing.T) *findings.Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CERAA_HOME", home)
	store, err := findings.Open(filepath.Join(home, "findings"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	records := []findings.Finding{
		{
			ID:          "sql-injection-login-a1b2c3d4",
			Title:       "SQL injection in the login form",
			Severity:    findings.SeverityHigh,
			Status:      findings.StatusConfirmed,
			URL:         "https://example.com/login",
			PoC:         "curl -s 'https://example.com/login' -d \"u=' OR 1=1--\"",
			Evidence:    "Two rows came back for one username.",
			CVSSVector:  "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
			CVSSScore:   9.8,
			Description: "The username parameter is concatenated into a query.",
		},
		{
			ID:       "reflected-search-e5f6a7b8",
			Title:    "Reflected parameter in the search box",
			Severity: findings.SeverityInfo,
			Status:   findings.StatusRuledOut,
			Evidence: "The response sets Content-Type: text/plain.",
		},
		{
			ID:       "admin-unauth-c9d0e1f2",
			Title:    "Missing proof that /admin is unauthenticated",
			Severity: findings.SeverityLow,
			Status:   findings.StatusOpenProofGap,
		},
	}
	for _, record := range records {
		if _, err := store.Put(record); err != nil {
			t.Fatalf("put %s: %v", record.Title, err)
		}
	}
	return store
}

func TestRunReportRendersTheRecord(t *testing.T) {
	findingRecord(t)
	var out bytes.Buffer
	if err := runReport(&out, reportOptions{Format: "md"}); err != nil {
		t.Fatalf("run the report: %v", err)
	}
	got := out.String()
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
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report is missing %q:\n%s", want, got)
		}
	}
}

// A report that leaves out what was ruled out cannot tell the reader what
// was checked, so the coverage describes the whole record even when the
// findings are filtered down to one closure state.
func TestRunReportFiltersTheFindingsButNotTheCoverage(t *testing.T) {
	findingRecord(t)
	var out bytes.Buffer
	if err := runReport(&out, reportOptions{Format: "md", Status: "confirmed"}); err != nil {
		t.Fatalf("run the report: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "### SQL injection in the login form") {
		t.Errorf("the confirmed finding is missing:\n%s", got)
	}
	for _, absent := range []string{"### Reflected parameter in the search box", "### Missing proof that /admin is unauthenticated"} {
		if strings.Contains(got, absent) {
			t.Errorf("a filtered-out finding was rendered as %q:\n%s", absent, got)
		}
	}
	for _, want := range []string{"## Ruled out", "Reflected parameter in the search box", "## Open proof gaps", "findings: 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("the coverage is missing %q:\n%s", want, got)
		}
	}
}

func TestRunReportFiltersBySeverity(t *testing.T) {
	findingRecord(t)
	var out bytes.Buffer
	if err := runReport(&out, reportOptions{Format: "md", Severity: "high"}); err != nil {
		t.Fatalf("run the report: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "### SQL injection in the login form") {
		t.Errorf("the high severity finding is missing:\n%s", got)
	}
	if strings.Contains(got, "### Reflected parameter in the search box") {
		t.Errorf("a filtered-out finding was rendered:\n%s", got)
	}
}

func TestRunReportWritesTheFile(t *testing.T) {
	findingRecord(t)
	target := filepath.Join(t.TempDir(), "report.md")
	var out bytes.Buffer
	if err := runReport(&out, reportOptions{Format: "md", Out: target}); err != nil {
		t.Fatalf("run the report: %v", err)
	}
	if !strings.Contains(out.String(), "report written to "+target) {
		t.Errorf("the command did not say where it wrote the report:\n%s", out.String())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read the report: %v", err)
	}
	if !strings.Contains(string(data), "### SQL injection in the login form") {
		t.Errorf("the file does not carry the record:\n%s", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat the report: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the report is mode %o, want 0600", perm)
	}
}

func TestRunReportRendersHTML(t *testing.T) {
	findingRecord(t)
	var out bytes.Buffer
	if err := runReport(&out, reportOptions{Format: "html"}); err != nil {
		t.Fatalf("run the report: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "<!DOCTYPE html>") {
		t.Errorf("the html report does not open as a document:\n%s", got)
	}
	if strings.Contains(got, "<script") {
		t.Error("the html report carries a script")
	}
}

func TestRunReportRefusesWhatItCannotRender(t *testing.T) {
	findingRecord(t)
	for _, options := range []reportOptions{
		{Format: "pdf"},
		{Format: "md", Status: "probably"},
		{Format: "md", Severity: "catastrophic"},
	} {
		var out bytes.Buffer
		if err := runReport(&out, options); err == nil {
			t.Errorf("the command accepted %+v", options)
		}
	}
}

// An empty record is a state, not a failure.
func TestRunReportWithNoRecord(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	var out bytes.Buffer
	if err := runReport(&out, reportOptions{Format: "md"}); err != nil {
		t.Fatalf("run the report: %v", err)
	}
	if !strings.Contains(out.String(), "No finding record yet") {
		t.Errorf("the command did not report the empty state:\n%s", out.String())
	}
}

// The command has to be attachable to the root by one line, so its name and
// its flags are pinned here.
func TestReportCommandCarriesItsFlags(t *testing.T) {
	command := ReportCommand()
	if command.Name() != "report" {
		t.Errorf("the command is named %q, want report", command.Name())
	}
	for _, name := range []string{"format", "out", "status", "severity"} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("the command has no --%s flag", name)
		}
	}
}

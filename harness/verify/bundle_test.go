package verify

import (
	"regexp"
	"strings"
	"testing"
)

// A line number in the identity means an edit anywhere above a finding mints a
// new one, and a re-scan then reports everything as new. That trains a reader to
// ignore the tool, which is worse than having no tool.
func TestFingerprintSurvivesAnUnrelatedEditAboveTheFinding(t *testing.T) {
	before := Fingerprint("CWE-89", "/srv/app/internal/db/query.go", "SELECT * FROM users WHERE id = ? AND tenant = ?")
	// Same finding, but the line number moved and the path was reached by a
	// relative route.
	after := Fingerprint("CWE-89", "./srv/app/internal/db/query.go", "SELECT * FROM users WHERE id = ? AND tenant = ?\n")
	if before != after {
		t.Fatalf("an unrelated edit changed the identity: %s vs %s", before, after)
	}
}

// Two genuinely different bugs in one file must keep separate identities, or the
// dedupe key silently eats one of them.
func TestFingerprintSeparatesDistinctBugsInOneFile(t *testing.T) {
	sql := Fingerprint("CWE-89", "handler.go", `db.Query("SELECT * FROM users WHERE id = \"+id+\"")`)
	xss := Fingerprint("CWE-79", "handler.go", `db.Query("SELECT * FROM users WHERE id = \"+id+\"")`)
	if sql == xss {
		t.Fatal("two different classes in one file collapsed into one identity")
	}
	other := Fingerprint("CWE-89", "handler.go", `db.Exec("DELETE FROM users WHERE id = \"+id+\"")`)
	if sql == other {
		t.Fatal("two different snippets in one file collapsed into one identity")
	}
}

func TestFingerprintIsStableAndShort(t *testing.T) {
	first := Fingerprint("CWE-798", "config/secrets.go", `const apiKey = "AKIAIOSFODNN7EXAMPLE"`)
	second := Fingerprint("CWE-798", "config/secrets.go", `const apiKey = "AKIAIOSFODNN7EXAMPLE"`)
	if first != second {
		t.Fatalf("the identity is not deterministic: %s vs %s", first, second)
	}
	if len(first) != 12 {
		t.Fatalf("identity length = %d, want 12", len(first))
	}
	for _, r := range first {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("identity %q is not lowercase hex", first)
		}
	}
}

func TestFingerprintSeparatesClassesCaseInsensitively(t *testing.T) {
	if Fingerprint("cwe-89", "a.go", "x") != Fingerprint("CWE-89", "a.go", "x") {
		t.Fatal("class case changed the identity")
	}
}

// An empty bundle has nothing to check, which is not the same as a bundle whose
// check found nothing.
func TestAnEmptyBundleIsUnprovenNotProven(t *testing.T) {
	var empty Bundle
	if empty.HasBundle() {
		t.Fatal("an empty bundle reported having evidence")
	}
	got := Bundle{FreshAttempted: true, FreshReachable: true, Fresh: Exchange{Status: 200, Body: "x"}}.Decide("marker")
	if got.Verdict != Unproven {
		t.Fatalf("verdict = %s, want UNPROVEN", got.Verdict)
	}
}

func TestBundleRoleIsCaseInsensitive(t *testing.T) {
	bundle := Bundle{Original: Exchange{Label: "Attacker", Status: 200}}
	if _, ok := bundle.Role("attacker"); !ok {
		t.Fatal("role lookup is case-sensitive, so a recorded label silently fails to match")
	}
}

// Header names are case-insensitive on the wire and a recorded trace carries the
// server's capitalisation, so an exact map lookup turns a real finding into "no
// Location header" — a false negative dressed as a check.
func TestHeaderValueToleratesAnyCapitalisationAndAbsentMaps(t *testing.T) {
	exchange := Exchange{ResponseHeaders: map[string]string{"LOCATION": "https://evil.example/"}}
	if got := exchange.HeaderValue("Location"); got != "https://evil.example/" {
		t.Fatalf("HeaderValue = %q", got)
	}
	if got := (Exchange{}).HeaderValue("Location"); got != "" {
		t.Fatalf("HeaderValue on an exchange with no headers = %q", got)
	}
	if got := exchange.HeaderValue("X-Absent"); got != "" {
		t.Fatalf("HeaderValue for an absent header = %q", got)
	}
}

// Observations are how execution is told apart from reflection, so the prefix
// match matters: "exec" has to cover "exec:dialog" and "exec:console".
func TestObservedMatchesAChannelFamily(t *testing.T) {
	exchange := Exchange{Observations: []Observation{{Kind: "exec:console"}}}
	if !exchange.Observed("exec") {
		t.Fatal("exec:console did not match the exec family")
	}
	if exchange.Observed("oob") {
		t.Fatal("an unrelated family matched")
	}
}

// The default patterns exist so a scanner and the redactor cannot disagree about
// what a credential looks like. Each is asserted against a real sample, because
// a pattern that compiles and never matches is worse than no pattern: it reads
// as coverage and finds nothing.
func TestDefaultPatternsMatchTheShapesTheyClaimToCover(t *testing.T) {
	samples := map[string]string{
		"aws key id":     "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"generic secret": `password = "hunter2hunter2"`,
		"db uri":         "postgres://admin:s3cr3t@db.internal:5432/app",
		"private key":    "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		"jwt":            "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"slack token":    "xoxb-123456789012-abcdefghijklmno",
		"github pat":     "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github fine":    "github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz",
		"anthropic key":  "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"google api key": "AIzaSyA1234567890abcdefghijklmnopqrstuvw",
	}
	patterns := DefaultSecretPatterns()
	for name, sample := range samples {
		matched := false
		for _, pattern := range patterns {
			expression, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("%s: pattern %q does not compile: %v", name, pattern, err)
			}
			if expression.MatchString(sample) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("no default pattern matched the %s sample; a scanner that misses it disagrees with a redactor that strips it", name)
		}
	}
}

// The inverse matters as much: ordinary text must not match, or every finding
// becomes a disclosure.
func TestDefaultPatternsDoNotMatchOrdinaryText(t *testing.T) {
	ordinary := []string{
		"<!doctype html><html><body><div id=root></div></body></html>",
		"the deployment finished successfully in 42s",
		`{"status":"ok","count":12}`,
	}
	for _, sample := range ordinary {
		for _, pattern := range DefaultSecretPatterns() {
			expression, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("pattern %q does not compile: %v", pattern, err)
			}
			if expression.MatchString(sample) {
				t.Errorf("pattern %q matched ordinary text: %q", pattern, sample)
			}
		}
	}
}

// A secret quoted in full in a finding defeats the redaction Ceraa applies
// everywhere else, so the match is shortened.
func TestAMatchedSecretIsNeverQuotedInFull(t *testing.T) {
	long := "AKIAIOSFODNN7EXAMPLE"
	if got := mask(long); strings.Contains(got, long) {
		t.Fatalf("mask(%q) = %q, which still contains the whole secret", long, got)
	}
	if got := mask("AKIA"); got != "A…" {
		t.Fatalf("mask of a short value = %q", got)
	}
}

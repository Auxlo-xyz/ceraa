package verify

// secret_test.go pins the credential list against the redactor it mirrors,
// and pins the masking: a matched credential never reaches a decision in the
// clear.

import (
	"strings"
	"testing"

	"ceraa/harness/redact"
)

// secretCorpus is the corpus the mirror test runs both lists over.
//
// Every sample is at least eight characters long, because the redactor
// leaves text shorter than that alone and a comparison against a list that
// answered a different question would prove nothing.
//
// The samples are assembled from fragments on purpose. A file that spells
// out a token in full is a file a secret scanner reads as a credential, and
// the public mirror is a repository where that blocks the push. The
// fragments concatenate into exactly the same strings at run time, which is
// what the test needs; the file itself carries no credential.
var secretCorpus = []struct {
	name   string
	text   string
	secret bool
}{
	{name: "provider api key", text: "sk-ant-api03-" + "abcdefghijklmnopqrstuvwxyz012345", secret: true},
	{name: "xai api key", text: "xai-" + "abcdefghijklmnopqrstuvwxyz012345", secret: true},
	{name: "groq api key", text: "gsk_" + "abcdefghijklmnopqrstuvwxyz0123456789abcdefgh", secret: true},
	{name: "huggingface token", text: "hf_" + "abcdefghijklmnopqrstuvwxyz012345", secret: true},
	{name: "github token", text: "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789", secret: true},
	{name: "github fine grained token", text: "github_" + "pat_abcdefghijklmnopqrstuvwxyz01", secret: true},
	{name: "slack token", text: "xox" + "b-1234567890-abcdefghijklmnopqrstuvwx", secret: true},
	{name: "aws access key id", text: "AKI" + "A1234567890ABCDEF", secret: true},
	{name: "google api key", text: "AIza" + "SyD-1234567890abcdefghijklmnopqrstu", secret: true},
	{name: "npm token", text: "npm_" + "abcdefghijklmnopqrstuvwxyz012345", secret: true},
	{
		name: "json web token",
		text: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
			"eyJzdWIiOiIxMjM0NTY3ODkwIn0." +
			"dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		secret: true,
	},
	{name: "bearer token", text: "Authorization: Bearer " + "abcdefghijklmnopqrstuvwxyz", secret: true},
	{name: "credential in a key value pair", text: "api_key=" + "abcdefghijklmnop", secret: true},
	{name: "credential in a key suffix pair", text: "x-key=" + "abcdefgh", secret: true},
	{name: "credential in a command flag", text: "--token " + "abcdefghij", secret: true},
	{name: "credential in a url", text: "https://user:" + "supersecret@example.com/path", secret: true},

	{name: "a page", text: "the quick brown fox jumps over the lazy dog"},
	{name: "a html page", text: "<html><body>hello world</body></html>"},
	{name: "a json body", text: `{"status":"ok","count":3}`},
	{name: "a request line", text: "GET /admin 200 1450 bytes"},
	{name: "a short password", text: "password=hunter2"},
	{name: "a basic authorization header", text: "Authorization: Basic dXNlcjpwYXNz"},
	{name: "a url with a port", text: "https://example.com:8080/path?page=2"},
}

// TestDefaultSecretPatternsMirrorsTheRedactor pins the agreement between
// the scanner and the redactor it mirrors.
//
// The redactor exists to remove credentials, so it already knows what one
// looks like. A scanner that disagreed with it about that would be a bug
// waiting to happen, and the two lists cannot drift apart while this test
// passes.
func TestDefaultSecretPatternsMirrorsTheRedactor(t *testing.T) {
	for _, sample := range secretCorpus {
		t.Run(sample.name, func(t *testing.T) {
			_, found := FindSecret(sample.text)
			if found != sample.secret {
				t.Fatalf("the scanner found a credential: %t, want %t (text %q)", found, sample.secret, sample.text)
			}
			redacted := redact.SecretsInText(sample.text)
			if (redacted != sample.text) != sample.secret {
				t.Fatalf("the redactor masked the sample: %t, want %t (text %q)", redacted != sample.text, sample.secret, sample.text)
			}
		})
	}
}

// TestMaskValueUsesTheRedactorsForm pins the masked form to the redactor's,
// character for character, so a masked credential reads the same wherever it
// is found.
func TestMaskValueUsesTheRedactorsForm(t *testing.T) {
	const secret = "AKIA1234567890ABCDEF"

	masked := MaskValue(secret)
	if masked != redact.SecretsInText(secret) {
		t.Fatalf("the masked form is %q, want the redactor's %q", masked, redact.SecretsInText(secret))
	}
	if strings.Contains(masked, secret) {
		t.Fatalf("the masked form carries the credential: %q", masked)
	}
}

// TestMaskSecretsKeepsTheContext pins the masking of a value that sits in a
// key value pair: the name tells a reader what was found and the value is
// gone.
func TestMaskSecretsKeepsTheContext(t *testing.T) {
	masked := MaskSecrets("api_key=abcdefghijklmnop")
	if masked != "api_key=«redacted 16 chars»" {
		t.Fatalf("the masked text is %q", masked)
	}
	if strings.Contains(masked, "abcdefghijklmnop") {
		t.Fatalf("the masked text carries the credential: %q", masked)
	}
}

// TestFindSecretNamesTheShape pins the answer a reader gets: which shape was
// found, and nothing about the value.
func TestFindSecretNamesTheShape(t *testing.T) {
	match, ok := FindSecret("AKIA1234567890ABCDEF")
	if !ok {
		t.Fatal("the aws access key id was not found")
	}
	if match.Pattern != "aws access key id" {
		t.Fatalf("the match is named %q, want %q", match.Pattern, "aws access key id")
	}
	if match.Masked != "«redacted 20 chars»" {
		t.Fatalf("the match is masked as %q", match.Masked)
	}
}

// TestAMatchedSecretIsMaskedInTheEvidence pins the promise that the
// obligation reports the credential and not the credential.
func TestAMatchedSecretIsMaskedInTheEvidence(t *testing.T) {
	const secret = "AKIA1234567890ABCDEF"
	exchange := Observation{
		URL:          "https://example.com/.env",
		Status:       200,
		ResponseBody: "AWS_ACCESS_KEY_ID=" + secret,
		Baseline:     &Observation{Status: 404, ResponseBody: "not found"},
	}

	decision := Decide(exchange, []Control{SecretControl()})
	evidence := decision.Obligations[0].Evidence
	if !strings.Contains(evidence, "«redacted 20 chars»") {
		t.Fatalf("the evidence does not carry the masked credential: %q", evidence)
	}
	if strings.Contains(evidence, secret) {
		t.Fatalf("the evidence carries the credential: %q", evidence)
	}
}

// TestTheRawSecretIsAbsentFromTheDecision pins the guarantee that no part of
// a decision quotes a credential in the clear.
func TestTheRawSecretIsAbsentFromTheDecision(t *testing.T) {
	const secret = "AKIA1234567890ABCDEF"
	exchange := Observation{
		URL:          "https://example.com/.env",
		Status:       200,
		ResponseBody: "AWS_ACCESS_KEY_ID=" + secret,
		Baseline:     &Observation{Status: 404, ResponseBody: "not found"},
	}

	decision := Decide(exchange, DefaultControls())
	rendered := renderDecision(decision)
	if strings.Contains(rendered, secret) {
		t.Fatalf("the decision carries the credential:\n%s", rendered)
	}
	if !strings.Contains(rendered, "«redacted 20 chars»") {
		t.Fatalf("the decision does not carry the masked credential:\n%s", rendered)
	}
}

// TestAControlThatLeaksACredentialIsStillMasked pins the net under the secret
// control: a control a caller wrote that quotes the value it found does not
// get to defeat the guarantee.
func TestAControlThatLeaksACredentialIsStillMasked(t *testing.T) {
	const secret = "AKIA1234567890ABCDEF"
	control := Control{
		Name:     "leaky",
		Question: "the control quotes what it found",
		Enforce: func(o Observation) Obligation {
			return Obligation{State: Established, Evidence: "the response carries " + secret}
		},
	}

	decision := Decide(Observation{ResponseBody: secret}, []Control{control})
	if strings.Contains(renderDecision(decision), secret) {
		t.Fatalf("the decision carries the credential:\n%s", renderDecision(decision))
	}
}

// renderDecision flattens a decision into one text, so a test can look for a
// value anywhere in it.
func renderDecision(decision Decision) string {
	var b strings.Builder
	for _, obligation := range decision.Obligations {
		b.WriteString(obligation.Name)
		b.WriteString("\n")
		b.WriteString(obligation.Detail)
		b.WriteString("\n")
		b.WriteString(obligation.Evidence)
		b.WriteString("\n")
	}
	for _, reason := range decision.Reasons {
		b.WriteString(reason)
		b.WriteString("\n")
	}
	return b.String()
}

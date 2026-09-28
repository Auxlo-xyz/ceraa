package redact

import (
	"strings"
	"testing"
)

func TestSecretsInText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantMark string // substring that should be present after redaction
		wontMark string // substring that must NOT be present
	}{
		{
			name:     "bearer token",
			input:    "Authorization: Bearer sk-ant-api-12345678901234567890",
			wantMark: "«redacted",
			wontMark: "sk-ant-api-12345678901234567890",
		},
		{
			name:     "api key assignment",
			input:    "OPENAI_API_KEY=sk-proj-12345678901234567890",
			wantMark: "«redacted",
			wontMark: "sk-proj-12345678901234567890",
		},
		{
			name:     "xai key",
			input:    "export XAI_API_KEY=xai-12345678901234567890",
			wantMark: "«redacted",
			wontMark: "xai-12345678901234567890",
		},
		{
			name:     "github token",
			input:    "token=ghp_123456789012345678901234567890",
			wantMark: "«redacted",
			wontMark: "ghp_123456789012345678901234567890",
		},
		{
			name:     "url with password",
			input:    "postgres://user:secretpass@localhost:5432/db",
			wantMark: "«redacted",
			wontMark: "secretpass",
		},
		{
			name:     "no secrets preserved",
			input:    "The quick brown fox jumps over the lazy dog",
			wantMark: "",
			wontMark: "«redacted",
		},
		{
			name:     "short strings untouched",
			input:    "hi there",
			wantMark: "",
			wontMark: "«redacted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SecretsInText(tt.input)
			if tt.wantMark != "" && !strings.Contains(result, tt.wantMark) {
				t.Errorf("expected result to contain %q, got %q", tt.wantMark, result)
			}
			if tt.wontMark != "" && strings.Contains(result, tt.wontMark) {
				t.Errorf("result contains secret %q (should be redacted): %q", tt.wontMark, result)
			}
		})
	}
}

func TestIsSecretName(t *testing.T) {
	cases := []string{
		"API_KEY", "apikey", "secret", "token", "password",
		"XAI_API_KEY", "OPENAI_API_KEY", "private_key", "auth_token",
		"consumer-key", "client.key",
	}
	for _, c := range cases {
		if !isSecretName(c) {
			t.Errorf("expected %q to be a secret name", c)
		}
	}

	nonSecrets := []string{"name", "keyboard", "hotkey", "monkey", "public_key"}
	for _, c := range nonSecrets {
		if isSecretName(c) {
			t.Errorf("expected %q to NOT be a secret name", c)
		}
	}
}

func TestSecretsMap(t *testing.T) {
	input := map[string]string{
		"API_KEY": "sk-12345678901234567890",
		"name":    "not-a-secret",
		"DB_PASS": "postgres://user:p4ssw0rd@localhost",
	}
	result := Secrets(input).(map[string]string)

	if result["API_KEY"] == "sk-12345678901234567890" {
		t.Error("API_KEY should be redacted")
	}
	if !strings.Contains(result["API_KEY"], "«redacted") {
		t.Errorf("expected redaction marker in API_KEY, got %q", result["API_KEY"])
	}
	if result["name"] != "not-a-secret" {
		t.Error("non-secret values should be preserved")
	}
	if result["DB_PASS"] == "postgres://user:p4ssw0rd@localhost" {
		t.Error("DB_PASS should be redacted")
	}
}

func TestRedactedMarkerStable(t *testing.T) {
	input := "key=sk-12345678901234567890"
	first := SecretsInText(input)
	second := SecretsInText(first)
	if first != second {
		t.Errorf("redaction should be idempotent:\nfirst:  %q\nsecond: %q", first, second)
	}
}

// The reason literals exist: a user's own secret has no vendor prefix, so the
// patterns cannot see it. A value the process already holds can be matched
// exactly, which is what makes it safe to put one in a subprocess environment at
// all — without this, one `env` puts it in the model's context.
func TestARegisteredValueIsScrubbedWhereNoPatternWouldMatch(t *testing.T) {
	ResetLiterals()
	t.Cleanup(ResetLiterals)

	const value = "correct-horse-battery-staple-9f2a"
	// Nothing pattern-shaped about it.
	if SecretsInText(value) != value {
		t.Fatal("the fixture is not pattern-shaped, so it proves nothing")
	}
	RegisterLiteral(value)
	if RegisteredLiterals() != 1 {
		t.Fatalf("registered %d literals, want 1", RegisteredLiterals())
	}
	scrubbed := SecretsInText("the token is " + value + " ok")
	if strings.Contains(scrubbed, value) {
		t.Errorf("the value survived redaction: %s", scrubbed)
	}
	if !strings.Contains(scrubbed, redactionMarker) {
		t.Errorf("no marker in %q", scrubbed)
	}
}

// A value the process holds is often a shell command's whole output, so the
// scrub has to work mid-line and repeatedly.
func TestRegisteredValuesAreScrubbedEverywhere(t *testing.T) {
	ResetLiterals()
	t.Cleanup(ResetLiterals)
	const value = "supersecretvalue123"
	RegisterLiteral(value)
	for _, text := range []string{
		value,
		"env: " + value,
		"a=" + value + " b=" + value,
		"prefix" + value + "suffix",
	} {
		if strings.Contains(SecretsInText(text), value) {
			t.Errorf("survived in %q", text)
		}
	}
}

// Overlapping values: the token and a prefix of it are both registered, and
// replacing the short one first would leave most of the long one behind.
func TestOverlappingValuesAreScrubbedLongestFirst(t *testing.T) {
	ResetLiterals()
	t.Cleanup(ResetLiterals)
	const long = "abcdefgh12345678"
	const short = "abcdefgh"
	RegisterLiteral(long)
	RegisterLiteral(short)
	got := SecretsInText("value=" + long)
	if strings.Contains(got, "12345678") || strings.Contains(got, "abcdefgh") {
		t.Errorf("a tail of the secret survived: %q", got)
	}
}

// A short secret would corrupt unrelated text everywhere it appeared, so it is
// not registered. Claiming to protect it would do more damage than leaving it.
func TestShortValuesAreNotRegistered(t *testing.T) {
	ResetLiterals()
	t.Cleanup(ResetLiterals)
	RegisterLiteral("abc")
	RegisterLiteral("")
	if RegisteredLiterals() != 0 {
		t.Errorf("registered %d short values, want none", RegisteredLiterals())
	}
	if got := SecretsInText("abc is fine"); got != "abc is fine" {
		t.Errorf("unrelated text was rewritten: %q", got)
	}
}

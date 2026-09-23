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

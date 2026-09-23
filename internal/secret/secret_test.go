package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitAndSetGetDelete(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "secret.key")
	secretFile := filepath.Join(dir, "secrets.json")

	// Override default paths for test
	// Override HOME for any future tests, though we use explicit paths
	t.Setenv("HOME", dir)

	if err := Init(keyFile, secretFile); err != nil {
		t.Fatal(err)
	}

	// Set a secret
	if err := Set("test-key", "sk-12345678901234567890"); err != nil {
		t.Fatal(err)
	}

	// Get the secret
	val, err := Get("test-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "sk-12345678901234567890" {
		t.Errorf("expected secret value, got %q", val)
	}

	// Verify it's not stored in plaintext
	data, _ := os.ReadFile(secretFile)
	plaintext := string(data)
	if containsPlaintext(plaintext, "sk-12345678901234567890") {
		t.Error("secret stored in plaintext")
	}

	// List should show the key
	names := List()
	if len(names) != 1 || names[0] != "test-key" {
		t.Errorf("expected [test-key], got %v", names)
	}

	// Delete
	if err := Delete("test-key"); err != nil {
		t.Fatal(err)
	}

	// Verify deleted
	_, err = Get("test-key")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestEmptyStore(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "secret.key")
	secretFile := filepath.Join(dir, "secrets.json")

	t.Setenv("HOME", dir)

	if err := Init(keyFile, secretFile); err != nil {
		t.Fatal(err)
	}

	_, err := Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent secret")
	}

	names := List()
	if len(names) != 0 {
		t.Errorf("expected empty list, got %v", names)
	}
}

func containsPlaintext(s, secret string) bool {
	for i := 0; i <= len(s)-len(secret); i++ {
		if s[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}

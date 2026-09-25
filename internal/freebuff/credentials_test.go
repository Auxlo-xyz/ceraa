package freebuff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeCLICredentials(dir, fingerprintID, token string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body := map[string]any{
		"default": map[string]any{
			"id":              "user-1",
			"name":            nil,
			"email":           "dev@example.com",
			"authToken":       token,
			"fingerprintId":   fingerprintID,
			"fingerprintHash": "hash-1",
			"credits":         12.5,
		},
		"other": map[string]any{"email": "x@example.com", "authToken": "keep-me"},
	}
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0o600)
}

// TestImportFromCLIHandlesNullName pins the CLI's lenient handling: a null name
// must not log the account out.
func TestImportFromCLIHandlesNullName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FREEBUFF_CONFIG_DIR", dir)
	if err := writeCLICredentials(dir, "fp", "token"); err != nil {
		t.Fatal(err)
	}
	user, err := importFromCLI()
	if err != nil {
		t.Fatal(err)
	}
	if user == nil {
		t.Fatal("expected an imported user")
	}
	if user.Name != "" {
		t.Fatalf("name = %q, want empty string for null", user.Name)
	}
	if user.AuthToken != "token" || user.Email != "dev@example.com" || user.Credits != 12.5 {
		t.Fatalf("user = %+v", user)
	}
}

// TestImportFromCLIIsReadOnly ensures Ceraa never writes the Freebuff CLI
// credentials file: two writers on one file is a guaranteed clobber.
func TestImportFromCLIIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FREEBUFF_CONFIG_DIR", dir)
	if err := writeCLICredentials(dir, "fp", "token"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the full save path: an encrypted store write plus an import.
	initTestSecret(t)
	if err := saveUser(User{AuthToken: "new-token", FingerprintID: "fp2", Email: "dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("freebuff CLI credentials.json was modified by Ceraa")
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("freebuff CLI credentials.json mtime changed")
	}
}

// TestImportFromCLIMissingFile covers the common case: no CLI install.
func TestImportFromCLIMissingFile(t *testing.T) {
	t.Setenv("FREEBUFF_CONFIG_DIR", t.TempDir())
	user, err := importFromCLI()
	if err != nil || user != nil {
		t.Fatalf("user=%+v err=%v", user, err)
	}
}

// TestImportIgnoresRelativeConfigDir mirrors the CLI, which refuses a relative
// FREEBUFF_CONFIG_DIR rather than resolving it against the process cwd.
func TestImportIgnoresRelativeConfigDir(t *testing.T) {
	t.Setenv("FREEBUFF_CONFIG_DIR", "relative/path")
	if got := cliConfigDir(); got == "relative/path" {
		t.Fatalf("cliConfigDir = %q, want the absolute default", got)
	}
}

// TestEncryptedStoreWinsOverCLIImport pins the resolution precedence.
func TestEncryptedStoreWinsOverCLIImport(t *testing.T) {
	initTestSecret(t)
	dir := t.TempDir()
	t.Setenv("FREEBUFF_CONFIG_DIR", dir)
	if err := writeCLICredentials(dir, "fp", "cli-token"); err != nil {
		t.Fatal(err)
	}
	if err := saveUser(User{AuthToken: "ceraa-token", FingerprintID: "fp", Email: "dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	user, err := loadUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.AuthToken != "ceraa-token" {
		t.Fatalf("token = %q, want the encrypted store to win", user.AuthToken)
	}
}

// TestSaveUserRejectsEmptyToken keeps an unusable credential out of the store.
func TestSaveUserRejectsEmptyToken(t *testing.T) {
	initTestSecret(t)
	if err := saveUser(User{AuthToken: "  "}); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

// TestFingerprintIsStableAndPrefixed pins the legacy fingerprint shape.
func TestFingerprintIsStableAndPrefixed(t *testing.T) {
	first := newFingerprintID()
	second := newFingerprintID()
	if first != second {
		t.Fatalf("fingerprint is not stable: %q != %q", first, second)
	}
	if len(first) != len(fingerprintPrefix)+8 {
		t.Fatalf("fingerprint %q has unexpected length", first)
	}
	if first[:len(fingerprintPrefix)] != fingerprintPrefix {
		t.Fatalf("fingerprint %q missing the %q prefix", first, fingerprintPrefix)
	}
}

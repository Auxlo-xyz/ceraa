package secret

import (
	"path/filepath"
	"testing"
)

// A provider key is Ceraa's own credential. If it is a user secret, list_secrets
// names it and run_command_with_secret hands it to a command of the model's
// choosing, which is the escalation the reserved prefixes exist to prevent.
func TestProviderKeysAreReserved(t *testing.T) {
	for _, name := range []string{
		"provider_api_key_custom",
		"provider_api_key_minimax",
		"provider_api_key_openai",
	} {
		if !IsInternal(name) {
			t.Errorf("IsInternal(%q) = false, want true; the model can reach this credential", name)
		}
	}
}

func TestReservedNamesAreInvisibleToTheAgent(t *testing.T) {
	dir := t.TempDir()
	if err := Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Set("provider_api_key_custom", "sk-provider-key-not-for-the-model"); err != nil {
		t.Fatal(err)
	}
	if err := Set("GitHub_token", "ghp-a-real-user-secret"); err != nil {
		t.Fatal(err)
	}

	user := ListUser()
	for _, name := range user {
		if IsInternal(name) {
			t.Errorf("ListUser returned the reserved name %q", name)
		}
	}
	if len(user) != 1 || user[0] != "GitHub_token" {
		t.Errorf("ListUser = %v, want only the user secret [GitHub_token]", user)
	}
}

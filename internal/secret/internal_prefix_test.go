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
	if err := SetUser("GitHub_token", "ghp-a-real-user-secret"); err != nil {
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

// The OAuth providers that keep credentials outside the ceraa_ namespace. Each
// one of these was a live exposure through list_secrets until it was reserved:
// the store held the name, the model could read it, and the provider's own
// tokens were one run_command_with_secret away.
func TestOAuthProviderCredentialsAreReserved(t *testing.T) {
	for _, name := range []string{
		"gemini_access_token",
		"gemini_refresh_token",
		"gemini_expires_at",
		"qoder_access_token",
		"qoder_refresh_token",
		"qoder_machine_id",
		"workbuddy_access_token",
		"workbuddy_refresh_token",
		"buddy_access_token",
		"buddy_refresh_token",
	} {
		if !IsInternal(name) {
			t.Errorf("IsInternal(%q) = false, want true; the model can reach this credential", name)
		}
	}
}

// Every write is internal unless a surface asks for a user secret. The provider
// packages call Set, so a new credential is invisible to the agent without that
// package having to know this list exists.
func TestWritesAreInternalByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Set("some_future_provider_token", "not-for-the-model"); err != nil {
		t.Fatal(err)
	}
	if err := SetUser("MY_API_KEY", "mine-to-share"); err != nil {
		t.Fatal(err)
	}

	if !IsInternal("some_future_provider_token") {
		t.Error("a default write was classified as a user secret")
	}
	user := ListUser()
	if len(user) != 1 || user[0] != "MY_API_KEY" {
		t.Errorf("ListUser = %v, want only [MY_API_KEY]", user)
	}
}

// A surface that owns user secrets must not be able to overwrite Ceraa's own
// credential at the same name: the name is refused, not reassigned.
func TestSetUserRefusesReservedNames(t *testing.T) {
	dir := t.TempDir()
	if err := Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := Set("provider_api_key_custom", "the-real-one"); err != nil {
		t.Fatal(err)
	}
	if err := SetUser("provider_api_key_custom", "an-attacker-chosen-one"); err == nil {
		t.Fatal("SetUser accepted a reserved name")
	}
	got, err := Get("provider_api_key_custom")
	if err != nil || got != "the-real-one" {
		t.Fatalf("the reserved credential was replaced: value=%q err=%v", got, err)
	}
}

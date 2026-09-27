package hermesauth

import (
	"path/filepath"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// SignedIn decides whether the Telegram menu offers "sign in" or "switch to it",
// so a false negative leaves a configured user unable to reach the provider, and
// a false positive offers a switch that then fails.
func TestSignedInReflectsAStoredCredential(t *testing.T) {
	cases := []struct {
		name     string
		seed     func(t *testing.T, store *Store)
		signedIn bool
	}{
		{"nothing stored", func(t *testing.T, store *Store) {}, false},
		{
			"a stored nous credential",
			func(t *testing.T, store *Store) {
				if err := store.saveNative(Credential{
					Provider:    "nous",
					AccessToken: "acc-1",
					BaseURL:     "https://inference-api.nousresearch.com/v1",
					ExpiresAt:   time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			true,
		},
		{
			"a stored credential with no token",
			func(t *testing.T, store *Store) {
				// A record that exists but carries nothing usable must not read as
				// signed in, or the menu offers a switch that cannot work.
				if err := store.saveNative(Credential{
					Provider: "nous",
					BaseURL:  "https://inference-api.nousresearch.com/v1",
				}); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
				t.Fatalf("secret store: %v", err)
			}
			store := NewStore()
			store.Home = t.TempDir()
			tc.seed(t, store)

			// SignedIn builds its own store from the environment, so the temporary
			// home has to be visible to it.
			t.Setenv("HERMES_HOME", store.Home)
			if got := SignedIn(); got != tc.signedIn {
				t.Errorf("SignedIn() = %v, want %v", got, tc.signedIn)
			}
		})
	}
}

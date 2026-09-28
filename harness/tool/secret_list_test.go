package tool

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/harness/operation"
	"ceraa/internal/secret"
)

// operationSpecForTest runs the spec the way the tool itself translates it, so the
// test exercises the real shape rather than one built by hand.
func operationSpecForTest(t *testing.T) operation.Spec {
	t.Helper()
	spec, err := (&ListSecretsTool{}).Translate(nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return spec
}

func TestListSecretsShowsNamesButNeverValues(t *testing.T) {
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("init store: %v", err)
	}

	// A user secret, and one of Ceraa's own provider credentials.
	const value = "ghp_supersecretvalue"
	if err := secret.Set("GitHub_token", value); err != nil {
		t.Fatalf("set user secret: %v", err)
	}
	if err := secret.Set("ceraa_minimax_api_key", "internal-value-not-for-the-model"); err != nil {
		t.Fatalf("set internal secret: %v", err)
	}

	out, err := (&ListSecretsTool{}).Run(operationSpecForTest(t))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out, "GitHub_token") {
		t.Errorf("the user secret name is missing from the listing:\n%s", out)
	}
	if want := fmt.Sprintf("%d chars", len([]rune(value))); !strings.Contains(out, want) {
		t.Errorf("the listing is missing %q:\n%s", want, out)
	}

	// The name is discoverable; the value is not, in any form.
	if strings.Contains(out, value) {
		t.Error("the listing contains the secret value")
	}
	if strings.Contains(out, "internal-value-not-for-the-model") {
		t.Error("the listing contains an internal secret value")
	}
	// A reserved name is Ceraa's own provider credential. Enumerating it would let
	// the model aim run_command_with_secret at it.
	if strings.Contains(out, "ceraa_minimax_api_key") {
		t.Error("the listing exposes a reserved internal secret name")
	}
}

func TestListSecretsEmptyStore(t *testing.T) {
	dir := t.TempDir()
	if err := secret.Init(filepath.Join(dir, "key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("init store: %v", err)
	}
	out, err := (&ListSecretsTool{}).Run(operationSpecForTest(t))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "No user secrets") {
		t.Errorf("an empty store should say so plainly, got:\n%s", out)
	}
}

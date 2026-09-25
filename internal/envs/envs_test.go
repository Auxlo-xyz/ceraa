package envs

import "testing"

// TestCeraaWinsOverLegacy pins the precedence rule.
func TestCeraaWinsOverLegacy(t *testing.T) {
	t.Setenv("CERAA_MODEL", "ceraa")
	t.Setenv("SOORAA_MODEL", "legacy")
	if got := Get("MODEL"); got != "ceraa" {
		t.Fatalf("Get = %q, want the Ceraa value", got)
	}
	t.Setenv("CERAA_MODEL", "")
	if got := Get("MODEL"); got != "legacy" {
		t.Fatalf("Get = %q, want the legacy fallback", got)
	}
}

// TestUnsetIsEmpty covers a clean install.
func TestUnsetIsEmpty(t *testing.T) {
	t.Setenv("CERAA_NOT_SET_ANYWHERE", "")
	t.Setenv("SOORAA_NOT_SET_ANYWHERE", "")
	if got := Get("NOT_SET_ANYWHERE"); got != "" {
		t.Fatalf("Get = %q, want empty", got)
	}
}

// TestSetWritesCanonicalPrefix pins that Set does not write the legacy name.
func TestSetWritesCanonicalPrefix(t *testing.T) {
	t.Setenv("CERAA_SETME", "")
	t.Setenv("SOORAA_SETME", "")
	if err := Set("SETME", "value"); err != nil {
		t.Fatal(err)
	}
	if got := Get("SETME"); got != "value" {
		t.Fatalf("Get after Set = %q", got)
	}
}

// TestGetWithPrefersOrder pins the multi-name variant used for settings that
// also have a third-party name.
func TestGetWithPrefersOrder(t *testing.T) {
	t.Setenv("SOORAA_TITLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	if got := GetWith("CERAA_TITLE_API_KEY", "SOORAA_TITLE_API_KEY", "GEMINI_API_KEY"); got != "" {
		t.Fatalf("GetWith = %q, want empty", got)
	}
	t.Setenv("GEMINI_API_KEY", "gemini")
	if got := GetWith("CERAA_TITLE_API_KEY", "SOORAA_TITLE_API_KEY", "GEMINI_API_KEY"); got != "gemini" {
		t.Fatalf("GetWith = %q, want the third-party fallback", got)
	}
	t.Setenv("SOORAA_TITLE_API_KEY", "legacy")
	if got := GetWith("CERAA_TITLE_API_KEY", "SOORAA_TITLE_API_KEY", "GEMINI_API_KEY"); got != "legacy" {
		t.Fatalf("GetWith = %q, want the legacy prefix to win over the third party", got)
	}
}

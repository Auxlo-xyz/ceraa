package envs

import "testing"

// TestGetReadsCeraaPrefix pins the single supported prefix.
func TestGetReadsCeraaPrefix(t *testing.T) {
	t.Setenv("CERAA_MODEL", "ceraa-model")
	if got := Get("MODEL"); got != "ceraa-model" {
		t.Fatalf("Get = %q", got)
	}
	t.Setenv("CERAA_MODEL", "")
	if got := Get("MODEL"); got != "" {
		t.Fatalf("Get = %q, want empty when unset", got)
	}
}

// TestSetWritesCeraaPrefix pins that Set round-trips through Get.
func TestSetWritesCeraaPrefix(t *testing.T) {
	t.Setenv("CERAA_SETME", "")
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
	t.Setenv("CERAA_TITLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	if got := GetWith("CERAA_TITLE_API_KEY", "GEMINI_API_KEY"); got != "" {
		t.Fatalf("GetWith = %q, want empty", got)
	}
	t.Setenv("GEMINI_API_KEY", "gemini")
	if got := GetWith("CERAA_TITLE_API_KEY", "GEMINI_API_KEY"); got != "gemini" {
		t.Fatalf("GetWith = %q, want the third-party fallback", got)
	}
	t.Setenv("CERAA_TITLE_API_KEY", "ceraa")
	if got := GetWith("CERAA_TITLE_API_KEY", "GEMINI_API_KEY"); got != "ceraa" {
		t.Fatalf("GetWith = %q, want the Ceraa value to win", got)
	}
}

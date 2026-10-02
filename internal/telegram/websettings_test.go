package telegram

import (
	"strings"
	"testing"
)

func TestApplyWebSettingsChangesWhatTheNextRunReads(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	if err := applyWebSettings(map[string]any{
		"model":           "m1",
		"agent":           "plan",
		"auto_compaction": true,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := webSettings()
	if got["model"] != "m1" {
		t.Fatalf("model = %v, want m1", got["model"])
	}
	if got["agent"] != "plan" {
		t.Fatalf("agent = %v, want plan", got["agent"])
	}
	if got["auto_compaction"] != true {
		t.Fatalf("auto_compaction = %v, want true", got["auto_compaction"])
	}
}

// TestApplyWebSettingsRefusesABodyItCannotUnderstand is the check that runs
// before anything is written. A key nobody would apply and a value of the wrong
// type both have to be rejected up front, because a settings form that saves
// three of its four fields and reports success is a form nobody will trust.
func TestApplyWebSettingsRefusesABodyItCannotUnderstand(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	cases := []struct {
		name   string
		update map[string]any
	}{
		{"unknown key", map[string]any{"modle": "x"}},
		{"model as a number", map[string]any{"model": 7}},
		{"provider as a bool", map[string]any{"provider": true}},
		{"auto_compaction as text", map[string]any{"auto_compaction": "yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyWebSettings(tc.update); err == nil {
				t.Fatalf("apply(%v) succeeded, want an error", tc.update)
			}
			if got := webSettings(); got["model"] != "" || got["agent"] != "" {
				t.Fatalf("a refused body changed state: %v", got)
			}
		})
	}
}

// TestWebSettingsNeverCarriesACredential pins a property rather than a value:
// no matter what the provider state holds, this snapshot cannot name it.
func TestWebSettingsNeverCarriesACredential(t *testing.T) {
	restore := stubBothState(t)
	defer restore()

	for key := range webSettings() {
		lower := strings.ToLower(key)
		for _, banned := range []string{"key", "token", "secret", "password", "auth"} {
			if strings.Contains(lower, banned) {
				t.Errorf("snapshot exposes %q: a browser tab holds this for as long as it is open", key)
			}
		}
	}
}

package llm

import (
	"strings"
	"testing"
)

// Wire ids travel verbatim: the endpoint routes on these exact strings, so
// the picker offers them directly rather than through a display-name mapping
// that could drift from what the server accepts.
func TestBuddyModelPools(t *testing.T) {
	if len(BuddyModels) == 0 || len(WorkBuddyModels) == 0 {
		t.Fatal("empty model pool")
	}
	seen := map[string]string{}
	for _, id := range append(append([]string{}, BuddyModels...), WorkBuddyModels...) {
		if strings.TrimSpace(id) == "" {
			t.Error("blank model id in a pool")
		}
	}
	for _, id := range BuddyModels {
		if prev, dup := seen[id]; dup {
			t.Errorf("id %q in both pools (%s); one region serves it", id, prev)
		}
		seen[id] = "buddy"
	}
	if !IsBuddyModel("deepseek-v4.1-flash") || !IsBuddyModel("default-model") || !IsBuddyModel("DeepSeek-V4.1-Flash") {
		t.Error("known ids not recognised (matching is case-insensitive)")
	}
	if IsBuddyModel("gpt-9") || IsBuddyModel("") {
		t.Error("unknown or empty id accepted")
	}
	if DefaultBuddyModel == "" || DefaultWorkBuddyModel == "" {
		t.Error("a pool without a default cannot activate with an empty model")
	}
}

// Identity headers are exact: the backend attributes usage by them, and a
// wrong product name bills the conversation to nobody.
func TestBuddyRequestHeaders(t *testing.T) {
	domestic := BuddyRequestHeaders()
	for key, want := range map[string]string{
		"X-Domain":        "copilot.tencent.com",
		"X-Product-Code":  "codebuddy",
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "CodeBuddy",
		"X-Product":       "CodeBuddy",
		"User-Agent":      "CodeBuddyIDE/1.106.1",
	} {
		if domestic[key] != want {
			t.Errorf("domestic %s = %q, want %q", key, domestic[key], want)
		}
	}
	intl := WorkBuddyRequestHeaders("deepseek-v4.1-flash")
	if intl["X-Domain"] != "www.workbuddy.ai" || intl["X-Product-Code"] != "workbuddy" {
		t.Errorf("intl headers = %v", intl)
	}
	// Model families ride different client shapes on the international pool.
	if got := WorkBuddyRequestHeaders("gpt-6-astra")["User-Agent"]; !strings.Contains(got, "WorkBuddy AI") {
		t.Errorf("gpt family UA = %q, want the intl shape", got)
	}
	if got := WorkBuddyRequestHeaders("kimi-k3")["User-Agent"]; strings.Contains(got, "WorkBuddy AI") {
		t.Errorf("kimi family UA = %q, want the domestic shape", got)
	}
	if got := WorkBuddyRequestHeaders("whatever-unknown")["User-Agent"]; got == "" {
		t.Error("unknown family has no UA fallback")
	}
}

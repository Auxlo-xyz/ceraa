package llm

import (
	"strings"
	"testing"
)

// Wire ids travel verbatim: the endpoint routes on these exact strings, so
// the picker offers them directly rather than through a display-name mapping
// that could drift from what the server accepts.
func TestWorkBuddyModelPool(t *testing.T) {
	if len(WorkBuddyModels) == 0 {
		t.Fatal("empty model pool")
	}
	seen := map[string]bool{}
	for _, id := range WorkBuddyModels {
		if strings.TrimSpace(id) == "" {
			t.Error("blank model id in the pool")
		}
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if !IsWorkBuddyModel("deepseek-v4.1-flash") || !IsWorkBuddyModel("default-model") || !IsWorkBuddyModel("DeepSeek-V4.1-Flash") {
		t.Error("known ids not recognised (matching is case-insensitive)")
	}
	if IsWorkBuddyModel("gpt-9") || IsWorkBuddyModel("") {
		t.Error("unknown or empty id accepted")
	}
	if DefaultWorkBuddyModel == "" {
		t.Error("a pool without a default cannot activate with an empty model")
	}
}

// A stored bare domain must heal to the versioned API root: it has no chat
// route, and the WAF in front of it blocks large streaming bodies with a 403
// page while small ones fall through to a 405.
func TestNormalizeWorkBuddyURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.workbuddy.ai":    WorkBuddyBaseURL,
		"https://www.workbuddy.ai/":   WorkBuddyBaseURL,
		"http://www.workbuddy.ai":     WorkBuddyBaseURL,
		"www.workbuddy.ai":            WorkBuddyBaseURL,
		"":                            WorkBuddyBaseURL,
		WorkBuddyBaseURL:              WorkBuddyBaseURL,
		"https://example.com/v1":      "https://example.com/v1",
		"https://www.workbuddy.ai/v3": "https://www.workbuddy.ai/v3",
	} {
		if got := NormalizeWorkBuddyURL(raw); got != want {
			t.Errorf("NormalizeWorkBuddyURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
func TestWorkBuddyRequestHeaders(t *testing.T) {
	intl := WorkBuddyRequestHeaders("deepseek-v4.1-flash", "u-1")
	if intl["X-Domain"] != "www.workbuddy.ai" || intl["X-Product"] != "SaaS" {
		t.Errorf("intl headers = %v", intl)
	}
	// The identity set is the desktop client's: Origin/Referer and the
	// X-Requested-With marker ride every chat, and the old CLI-only headers
	// that the post-2026-10-16 gateway rejects must not come back.
	for key, want := range map[string]string{
		"Origin":             "https://www.workbuddy.ai",
		"Referer":            "https://www.workbuddy.ai/",
		"X-Requested-With":   "XMLHttpRequest",
		"X-IDE-Type":         "WorkBuddy",
		"X-IDE-Name":         "WorkBuddy",
		"X-IDE-Version":      "5.5.2",
		"X-No-Enterprise-Id": "1",
		"X-User-Id":          "u-1",
	} {
		if intl[key] != want {
			t.Errorf("%s = %q, want %q", key, intl[key], want)
		}
	}
	for _, stale := range []string{"X-Product-Code", "X-Agent-Purpose"} {
		if value, ok := intl[stale]; ok {
			t.Errorf("stale CLI-only header %s = %q still present", stale, value)
		}
	}
	if anon := WorkBuddyRequestHeaders("glm-5.3", "  "); anon["X-No-User-Id"] != "1" {
		t.Errorf("blank uid must send X-No-User-Id, got %v", anon)
	}
	// Model families ride different client shapes on the international pool,
	// and neither invents a CLI version it does not know.
	if got := WorkBuddyRequestHeaders("gpt-6-astra", "u-1")["User-Agent"]; !strings.Contains(got, "WorkBuddy AI") {
		t.Errorf("gpt family UA = %q, want the intl shape", got)
	}
	if got := WorkBuddyRequestHeaders("kimi-k3", "u-1")["User-Agent"]; strings.Contains(got, "WorkBuddy AI") {
		t.Errorf("kimi family UA = %q, want the domestic shape", got)
	}
	if got := WorkBuddyRequestHeaders("whatever-unknown", "u-1")["User-Agent"]; got == "" || strings.Contains(got, "CLI/") {
		t.Errorf("unknown family UA = %q, want a CLI-free fallback", got)
	}
}

// The conversation trio is the 2026-10-16 precondition: a chat without it is
// answered 400/11128 regardless of the body, so a single reused id would make
// every request after the first look like a replay if the gateway ever starts
// enforcing uniqueness (the official client mints fresh ones per request).
func TestWorkBuddyConversationHeadersAreFreshPerRequest(t *testing.T) {
	first := WorkBuddyConversationHeaders()
	second := WorkBuddyConversationHeaders()
	if len(first) != 3 {
		t.Fatalf("trio = %v, want exactly three headers", first)
	}
	for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		value := first[key]
		if len(value) != 36 {
			t.Errorf("%s = %q, want a uuid", key, value)
		}
		if second[key] == value {
			t.Errorf("%s reused across requests: %q", key, value)
		}
	}
}

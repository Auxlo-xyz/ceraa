package llm

import "strings"

// WorkBuddySubscription identifies a Tencent WorkBuddy (intl) profile.
//
// The domestic CodeBuddy login is China-gated (WeChat/phone/SSO with no
// alternative path), so only the international product is offered. Its
// account system, endpoint and model pool are separate from the domestic
// one: sharing a subscription would route one region's token at the other's
// host.
const WorkBuddySubscription = "workbuddy"

// WorkBuddyBaseURL is the international chat endpoint root. The transport
// appends /chat/completions, so this stops at the version prefix.
const WorkBuddyBaseURL = "https://www.workbuddy.ai/v2"

// WorkBuddyModels are the international directory ids, in the IDE's display
// order. Aliases (Auto, Fast, Balanced, Primary, Deep) are real choices that
// let the gateway route the turn.
var WorkBuddyModels = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"deep-model",
	"hy4-preview-f",
	"hy4-preview",
	"hy3",
	"deepseek-v4.1-flash",
	"deepseek-v4.1-flash-sg",
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"glm-5.2",
	"kimi-k3",
	"kimi-k2.8-preview",
	"kimi-k2.6",
}

// DefaultWorkBuddyModel is the international default.
const DefaultWorkBuddyModel = "default-model"

// IsWorkBuddyModel reports whether an id belongs to the international pool.
func IsWorkBuddyModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range WorkBuddyModels {
		if trimmed == strings.ToLower(known) {
			return true
		}
	}
	return false
}

// workBuddyCNUserAgent is the domestic client shape, used for the CN model
// families on the international endpoint. The wrong shape does not fail the
// request; it misattributes usage in the vendor's backend.
const workBuddyCNUserAgent = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"

// workBuddyUserAgent picks the client shape by model family. Prefix match,
// because the directory carries variants (flash, preview, sg-suffixed) that
// all belong to the same line.
func workBuddyUserAgent(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"glm-", "hy", "kimi-", "minimax-"} {
		if strings.HasPrefix(name, prefix) {
			return workBuddyCNUserAgent
		}
	}
	return "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
}

// WorkBuddyRequestHeaders returns the product identity headers for
// international inference.
func WorkBuddyRequestHeaders(model string) map[string]string {
	return map[string]string{
		"X-Domain":        "www.workbuddy.ai",
		"X-Product-Code":  "workbuddy",
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   "5.5.2",
		"X-Product":       "WorkBuddy",
		"User-Agent":      workBuddyUserAgent(model),
	}
}

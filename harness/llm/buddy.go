package llm

import "strings"

// BuddySubscription identifies a Tencent CodeBuddy profile.
const BuddySubscription = "buddy"

// WorkBuddySubscription identifies a Tencent WorkBuddy (intl) profile. A
// separate account system with its own credential, endpoint and model pool:
// sharing one subscription would route one region's token at the other's
// host.
const WorkBuddySubscription = "workbuddy"

// BuddyBaseURL is the domestic chat endpoint root. The transport appends
// /chat/completions, so this stops at the version prefix.
const BuddyBaseURL = "https://copilot.tencent.com/v2"

// WorkBuddyBaseURL is the international chat endpoint root.
const WorkBuddyBaseURL = "https://www.workbuddy.ai/v2"

// BuddyModels are the domestic directory ids. They travel verbatim: the
// endpoint routes on these exact strings.
var BuddyModels = []string{
	"hy4-preview",
	"hy3",
	"hy3-x",
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"glm-5.3",
	"glm-5.3-flash",
	"glm-5.2",
	"glm-5.1",
	"glm-5v-turbo",
	"kimi-k3-1",
	"kimi-k2.8-preview",
	"kimi-k2.7",
	"kimi-k2.6",
	"minimax-m3",
}

// DefaultBuddyModel is the domestic default.
const DefaultBuddyModel = "deepseek-v4.1-flash"

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

// IsBuddyModel reports whether an id belongs to either Tencent pool. Either,
// because a model id alone does not say which region serves it; the profile's
// subscription decides the host, and the host decides.
func IsBuddyModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range BuddyModels {
		if trimmed == strings.ToLower(known) {
			return true
		}
	}
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

// BuddyRequestHeaders returns the product identity headers for domestic
// inference. Authorization travels separately through the profile key, the
// way every OpenAI-compatible transport carries it.
func BuddyRequestHeaders() map[string]string {
	return map[string]string{
		"X-Domain":        "copilot.tencent.com",
		"X-Product-Code":  "codebuddy",
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "CodeBuddy",
		"X-IDE-Type":      "CodeBuddy",
		"X-IDE-Version":   "1.106.1",
		"X-Product":       "CodeBuddy",
		"User-Agent":      "CodeBuddyIDE/1.106.1",
	}
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

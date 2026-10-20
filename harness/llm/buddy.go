package llm

import (
	"crypto/rand"
	"fmt"
	"strings"
)

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
// order. Four aliases (default, fast, balanced, primary) are real choices that
// let the gateway route the turn; the rest are concrete models. The pool was
// read from the account's /v3/config roster rather than the IDE build, so it
// lists what this login can actually select.
var WorkBuddyModels = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"hy3",
	"deepseek-v4.1-flash",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gemini-3.5-flash",
	"glm-5.3-flash",
	"glm-5.3",
	"glm-5.2",
	"minimax-m3.1-flash",
	"kimi-k3",
	"kimi-k2.6",
	"kimi-k2.8-preview",
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

// NormalizeWorkBuddyURL repairs a stored endpoint that points at the product
// domain instead of the versioned API root.
//
// Activation used to save product.Endpoint (the bare domain, which is the
// login surface) as the provider URL, shadowing this file's /v2 default. The
// bare domain has no /chat/completions route: small requests come back 405
// from nginx while large streaming ones are stopped by the Tencent WAF in
// front of it, which reads as a provider 403 block page. Anything already
// versioned, empty, or on another host passes through untouched.
func NormalizeWorkBuddyURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	bare := strings.TrimRight(trimmed, "/")
	if bare == "" {
		return WorkBuddyBaseURL
	}
	rest := bare
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if strings.EqualFold(rest, "www.workbuddy.ai") {
		return WorkBuddyBaseURL
	}
	return trimmed
}

// workBuddyCNUserAgent is the domestic client shape, used for the CN model
// families on the international endpoint. The wrong shape does not fail the
// request; it misattributes usage in the vendor's backend.
const workBuddyCNUserAgent = "WorkBuddy/5.5.2 WorkBuddy/5.5.2"

// workBuddyIntlUserAgent is the international client shape. The trailing
// CLI/... token is deliberately absent: the official client drops it when the
// bundled CLI's own version cannot be read, and 5.5.2 is the app version, not
// a CLI version, so sending it would be a wrong value in a header.
const workBuddyIntlUserAgent = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2"

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
	return workBuddyIntlUserAgent
}

// WorkBuddyRequestHeaders returns the product identity headers every
// international inference request carries.
//
// The shape is the official desktop client's, measured against the live
// gateway on 2026-10-20. It must not drift back to the older CLI-only set:
// since 2026-10-16 the gateway answers any /v2/chat/completions request that
// does not look like the desktop client with HTTP 400 / code 11128
// ("request illegal", displayMsg zh: the request was blocked), including the
// variant that carried X-Product-Code and X-Agent-Purpose but no Origin.
// X-Product is the deployment-type tag ("SaaS"), not the product name, and
// X-Requested-With / Origin / Referer are part of that same shape.
//
// X-User-Id carries the account identity the gateway attributes usage to;
// without a uid the client says X-No-User-Id: 1 instead. X-No-Enterprise-Id
// marks a personal account, which is the only kind the login flow here
// produces (an enterprise account would send X-Enterprise-Id instead).
func WorkBuddyRequestHeaders(model, uid string) map[string]string {
	headers := map[string]string{
		"Origin":             "https://www.workbuddy.ai",
		"Referer":            "https://www.workbuddy.ai/",
		"User-Agent":         workBuddyUserAgent(model),
		"X-Domain":           "www.workbuddy.ai",
		"X-IDE-Name":         "WorkBuddy",
		"X-IDE-Type":         "WorkBuddy",
		"X-IDE-Version":      "5.5.2",
		"X-No-Enterprise-Id": "1",
		"X-Product":          "SaaS",
		"X-Requested-With":   "XMLHttpRequest",
	}
	if trimmed := strings.TrimSpace(uid); trimmed != "" {
		headers["X-User-Id"] = trimmed
	} else {
		headers["X-No-User-Id"] = "1"
	}
	return headers
}

// WorkBuddyConversationHeaders mints the conversation-channel trio the
// international chat endpoint requires.
//
// Since 2026-10-16 the gateway answers a chat request that omits all three
// headers with HTTP 400 / code 11128, the same envelope the identity checks
// use, which is why this failure read as a body problem for so long. The
// official desktop client sends the trio on every chat. A fresh UUID per
// request is enough: the upstream keeps no history keyed by the id, so the
// trio marks the channel rather than carrying conversation state, and fresh
// ids keep concurrent runs off one shared conversation.
func WorkBuddyConversationHeaders() map[string]string {
	return map[string]string{
		"X-Conversation-ID":         workBuddyUUID(),
		"X-Conversation-Request-ID": workBuddyUUID(),
		"X-Conversation-Message-ID": workBuddyUUID(),
	}
}

// workBuddyUUID mints a v4 UUID. Fresh per request, same shape as the
// transport-local id helpers.
func workBuddyUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

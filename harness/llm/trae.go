package llm

import "strings"

// TraeSubscription identifies a Trae (ByteDance, international) profile.
//
// The login is email-based on trae.ai, so unlike the CN surface (phone-bound)
// it is sign-up-able without Chinese details. The transport rides Trae's
// raw-chat endpoint with a PAT exchanged for a short-lived ide token, the
// same shape the Trae CLI itself uses.
const TraeSubscription = "trae"

// TraeBaseURL is the international API root. The transport appends
// /api/ide/v2/llm_raw_chat, so this stops at the host.
const TraeBaseURL = "https://api.trae.ai"

// TraeAppID and TraeIDEVersionCode are the client identity the raw-chat
// endpoint expects. They are fixed client constants, captured from the live
// client, not secrets.
const TraeAppID = "7b3f9dc2-8a4e-5c6d-2f1b-9e4a3c5b7df0"
const TraeIDEVersionCode = "20260206"

// TraeModels are the international directory ids, in the order they should
// be offered. config_name is the wire id; rawModelName is the model_name the
// endpoint expects alongside it.
var TraeModels = []string{
	"glm-5.1",
	"kimi-k2.6",
	"deepseek-v4-pro",
	"doubao-seed-2.0-code",
	"deepseek-v3.2",
	"minimax-m3",
}

// DefaultTraeModel is the international default.
const DefaultTraeModel = "glm-5.1"

// traeRawModels maps a display id to the (config_name, model_name) pair the
// raw-chat endpoint expects. Unknown ids pass through unchanged: config and
// model fall back to the id itself, which is what the endpoint accepts for
// ids it already knows.
var traeRawModels = map[string][2]string{
	"glm-5.1":              {"glm-5.1", "glm-5__v2"},
	"kimi-k2.6":            {"kimi-k2.6", "kimi-k2.6__v2"},
	"deepseek-v4-pro":      {"deepseek-V4-Pro", "deepseek-V4-Pro__v2"},
	"doubao-seed-2.0-code": {"doubao-seed-2.0-code", "doubao-seed-2.0-code"},
	"deepseek-v3.2":        {"deepseek-v3.2", "deepseek-v3.2"},
	"minimax-m3":           {"minimax-m3", "minimax-m3"},
}

// TraeWireModel resolves a user-facing model id to the config_name and
// model_name the raw-chat endpoint expects.
func TraeWireModel(model string) (configName, modelName string) {
	trimmed := strings.TrimSpace(model)
	trimmed = strings.TrimPrefix(trimmed, "trae/")
	if pair, ok := traeRawModels[strings.ToLower(trimmed)]; ok {
		return pair[0], pair[1]
	}
	if trimmed == "" {
		trimmed = DefaultTraeModel
	}
	return trimmed, trimmed
}

// IsTraeModel reports whether an id belongs to the international pool.
func IsTraeModel(model string) bool {
	trimmed := strings.TrimSpace(model)
	trimmed = strings.TrimPrefix(trimmed, "trae/")
	for _, known := range TraeModels {
		if strings.EqualFold(trimmed, known) {
			return true
		}
	}
	return false
}

// NormalizeTraeURL fills in an empty endpoint with the API root. The stored
// value is always a bare host (the transport appends the raw-chat path), so
// unlike the buddy surface there is no versioned prefix to heal.
func NormalizeTraeURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return TraeBaseURL
	}
	return trimmed
}

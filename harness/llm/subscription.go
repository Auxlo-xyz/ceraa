package llm

import "strings"

// Subscription provider identifiers.
//
// A subscription provider authenticates with a credential held in the encrypted
// store rather than an API key in its profile. That difference matters to
// anything deciding whether a provider is configured: the profile has no
// api_key field to find, so a check that requires one reports a working
// subscription install as unconfigured.

// HermesSubscription identifies a Nous/Hermes subscription profile.
const HermesSubscription = "hermes"

// IsSubscription reports whether a profile authenticates by subscription rather
// than by an API key stored in the profile.
//
// One predicate for every such check. The distinction was previously spelled as
// an inline comparison against "hermes" at three separate sites, so a Codex
// install was treated as an unconfigured plain provider by all of them: doctor
// warned, the startup banner claimed onboarding was required, and /start sent an
// authenticated user back to the provider picker.
func IsSubscription(subscription string) bool {
	switch subscription {
	case HermesSubscription, CodexSubscription, XAISubscription, MiniMaxSubscription, ClineSubscription, QoderSubscription, OpenCodeSubscription, DeepSeekSubscription, ClaudeSubscription, GeminiSubscription, WorkBuddySubscription:
		return true
	default:
		return false
	}
}

// OpenCodeSubscription identifies an OpenCode Zen profile.
//
// Counted as a subscription by IsSubscription even though its credential is an
// API key rather than an OAuth token, which is the difference the predicate
// exists to describe: a Zen profile carries no api_key field to find, so a check
// that requires one would report a working install as unconfigured. The key is
// held in the encrypted store beside the OAuth tokens for exactly that reason.
const OpenCodeSubscription = "opencode"

// OpenCodeBaseURL is the Zen chat-completions root.
//
// Read from opencode's own docs, where it appears as
// https://opencode.ai/zen/v1/<method>, rather than guessed. Verified against the
// live endpoint before it was written here: GET /zen/v1/models answers 200 with
// a catalog and POST /zen/v1/chat/completions answers 401 with an empty body,
// which is the pair that confirms the path is right and the credential is what is
// missing. A near-miss on this path would be an authenticated request answered by
// a 404 instead.
const OpenCodeBaseURL = "https://opencode.ai/zen/v1"

// DefaultOpenCodeModel is the model selected when Zen is chosen without one.
//
// Space Bunny, because it was the first of the models measured to answer from
// outside OpenCode and it streams, returns reasoning, and costs nothing.
//
// The free name on Zen is not a promise that the model answers here. Sweeping
// all eighty-five ids with a real key on 2026-10-01 gave eight that answer a
// third-party client, eleven refused with FreeTierError ("can only be used from
// within OpenCode"), seventy asking for funds, one that does not speak this
// protocol at all, and one with no route on this endpoint. Re-measured
// 2026-10-14: the free tier is closed to third-party clients entirely and every
// free id refuses, so the default is the documented free id to pick if the
// policy reopens, not one that works today. The gate is per
// model, not per client, so a default picked on the strength of a name ending
// in -free would be a default that does not answer.
const DefaultOpenCodeModel = "space-bunny-free"

// OpenCodeFreeModels are the Zen ids upstream lists as free, in the order they
// should be offered, with the default first.
//
// This is the list the picker reads, so a model the catalog shows is a model
// that exists upstream rather than a retired id. It is no longer a list of ids
// that answer from here: re-measured live on 2026-10-14, every one of them
// refuses a third-party client with FreeTierError. exo-free and
// fledge-alpha-free are absent because upstream answers 410 ModelDeprecated
// for them, and cat-jam-free and step-5-preview-free are present because the
// current upstream docs list them.
//
// Not here: the two Muse Spark contributor models speak /responses rather than
// chat/completions, which is a different transport this program does not
// speak; jev-1.13-free reports ModelProtocolUnsupported and
// ling-3.0-flash-fin-free has no route on this endpoint.
var OpenCodeFreeModels = []string{
	"space-bunny-free",
	"big-pickle",
	"mimo-v2.6-flash-free",
	"mimo-v2.5-free",
	"longcat-2.5-preview-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
	"cat-jam-free",
	"step-5-preview-free",
}

// OpenCodeModels lists the model ids Zen serves, in the order they should be
// offered.
//
// Read from a live GET /zen/v1/models rather than copied from another provider's
// catalog: the remote model catalog this codebase already has is keyed on
// OpenAI-compatible hosts and would contain none of these. Eighty-five entries,
// sorted so the free tier comes first, followed by the paid ones.
//
// Free here means it bills nothing, not that it is reachable. Since
// 2026-10-14 every free id is refused with FreeTierError from a third-party
// client, and seventy paid ones need balance in the account. All of them stay
// in the list because either stops applying the moment OpenCode changes its
// mind or the account is topped up, and a picker that silently dropped models
// would hide the reason a user's choice did not work. What does not change is
// the default, which is the documented free id to pick if the policy reopens.
//
// The ids are lowercase and the endpoint serves them case-insensitively, but
// IsOpenCodeModel compares case-insensitively anyway so a user typing Claude-Sonnet-5
// is not told the model does not exist.
var OpenCodeModels = []string{
	// Free tier. OpenCodeFreeModels first, in the same order, so the top of
	// the picker is the upstream free set. None of them answers a
	// third-party client since 2026-10-14; the order is catalog truth, not a
	// promise.
	"space-bunny-free",
	"big-pickle",
	"mimo-v2.6-flash-free",
	"mimo-v2.5-free",
	"longcat-2.5-preview-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
	"cat-jam-free",
	"step-5-preview-free",
	// Still free, still listed, refuses from here: Muse Spark speaks
	// /responses only, jev-1.13 reports ModelProtocolUnsupported,
	// ling-3.0-flash-fin has no route on chat/completions, and
	// deepseek-v4-flash is unavailable upstream. fledge-alpha-free and
	// exo-free are gone entirely: upstream answers 410 ModelDeprecated.
	"muse-spark-1.3-contributor-free",
	"muse-spark-1.2-contributor-free",
	"jev-1.13-free",
	"ling-3.0-flash-fin-free",
	"deepseek-v4-flash-free",

	// Paid, grouped by vendor.
	"claude-fable-5",
	"claude-fable-5-1",
	"claude-opus-5-5",
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-sonnet-5-5",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-sonnet-4",
	"claude-haiku-4-5",
	"gemini-3.6-flash",
	"gemini-3.8-flash",
	"gemini-3.7-flash",
	"gemini-3.5-flash-lite",
	"gemini-3.5-flash",
	"gemini-3.1-pro",
	"gemini-3-flash",
	"gpt-6-astra",
	"gpt-6.1-sol",
	"gpt-6-sol",
	"gpt-6-luna",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.5-pro",
	"gpt-5.4",
	"gpt-5.4-pro",
	"gpt-5.4-mini",
	"gpt-5.4-nano",
	"gpt-5.3-codex-spark",
	"gpt-5.3-codex",
	"gpt-5.2",
	"gpt-5.2-codex",
	"gpt-5.1",
	"gpt-5.1-codex-max",
	"gpt-5.1-codex",
	"gpt-5.1-codex-mini",
	"gpt-5",
	"gpt-5-codex",
	"gpt-5-nano",
	"grok-build-0.1",
	"grok-4.7",
	"grok-4.6",
	"grok-4.5",
	"muse-spark-1.3",
	"muse-spark-1.2",
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"deepseek-v4-flash-vision-exp",
	"glm-5.3-flash",
	"glm-5.3",
	"glm-5.2",
	"glm-5.1",
	"glm-5",
	"minimax-m3",
	"minimax-m2.7",
	"minimax-m2.5",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"kimi-k2.5",
	"qwen3.8-flash",
	"qwen3.6-plus",
	"qwen3.5-plus",
	"qwen3.8-max",
	"jev-1.13",
}

// IsOpenCodeModel reports whether a model id is one the gateway is known to serve.
func IsOpenCodeModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range OpenCodeModels {
		if trimmed == known {
			return true
		}
	}
	return false
}

// OpenCodeTrafficTools are the tool names Zen's free tier looks for in a
// request body.
//
// Measured, not guessed. On 2026-10-01 a live key was swept through the free
// tier one variable at a time: dropping the tools array from an otherwise
// accepted request refused it, an empty array refused it, a single hand-written
// tool refused it, and any two tools other than this pair refused it. The names
// have to be exactly these two and both have to be present, in any order, with
// any other tools alongside them and any schema at all inside them.
//
// Ceraa is not borrowing these names to pass a check it does not satisfy: both
// are its own tools, registered in harness/tool/registry.go, and a normal
// conversation sends them anyway. The only requests that do not are the ones
// that deliberately carry no tools, such as a compaction.
var OpenCodeTrafficTools = []string{"bash", "read"}

// EnsureOpenCodeTraffic rewrites a chat-completions body into the shape Zen's
// free tier accepts.
//
// Three things, all of them measured: the request streams, it carries a tools
// array containing OpenCodeTrafficTools, and a request that had no tools of its
// own does not advertise the ones this added as callable.
//
// It mutates in place so the caller keeps the body it built and merges its own
// extras into afterwards. It is safe to call twice.
func EnsureOpenCodeTraffic(body map[string]any) {
	if body == nil {
		return
	}
	body["stream"] = true
	tools, _ := body["tools"].([]map[string]any)
	carriedTools := len(tools) > 0
	have := make(map[string]bool, len(tools))
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name != "" {
			have[name] = true
		}
	}
	missing := false
	for _, name := range OpenCodeTrafficTools {
		if have[name] {
			continue
		}
		missing = true
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name":        name,
			"description": "Not offered on this request.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}})
	}
	if !missing {
		return
	}
	body["tools"] = tools
	if !carriedTools {
		// The caller wanted no tools. Advertising a pair that exists only to
		// satisfy the gateway would invite the model to call them, and there is
		// nothing on the other end of a compaction to run them. "none" keeps the
		// tools in the body, where the gateway reads them, while telling the
		// model to leave them alone.
		body["tool_choice"] = "none"
	}
}

// HermesBaseURL is the Nous Portal inference endpoint.
//
// The Portal is not a public API, and Hermes is the only thing that knows this
// address: the value was taken from the Hermes source, where it is the
// DEFAULT_NOUS_INFERENCE_URL registry constant. Guessing an equivalent public
// URL would send requests to an endpoint that does not serve this credential.
const HermesBaseURL = "https://inference-api.nousresearch.com/v1"

// DefaultHermesModel is the model selected when Hermes is chosen without one.
const DefaultHermesModel = "upstage/solar-pro4:free"

// HermesModels lists the free-tier model IDs the Nous Portal inference endpoint
// serves.
//
// Kept as a list so IsHermesModel can refuse anything else, the same way Codex
// does. The endpoint is closed and returns a hard 400 for a model it does not
// serve, so picking a plausible-looking ID is a guaranteed failure rather than a
// maybe.
//
// The list is the free tier only, and the `:free` suffix is load-bearing: most of
// these also exist as a paid route under the bare name, and the suffix is what
// selects the free one. The account is served roughly 417 models in total,
// including paid ones, so this is a policy about which routes to offer rather
// than a description of the endpoint. `TestLiveModelCatalog` reads the live
// catalog and fails if anything here stops being served.
//
// An earlier list named `hermes-4-405b` and friends. Those name no model the
// endpoint serves, and the free names without their suffix draw a 400.
var HermesModels = []string{
	"inclusionai/ling-3.0-flash-sante:free",
	"inclusionai/ling-3.0-flash-fin:free",
	"poolside/laguna-s-2.1:free",
	"poolside/laguna-xs-2.1:free",
	"meituan/longcat-2.0:free",
	"stepfun/step-3.7-flash:free",
	"upstage/solar-pro4:free",
}

// IsHermesModel reports whether a model ID is known to work on the endpoint.
func IsHermesModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range HermesModels {
		if trimmed == known {
			return true
		}
	}
	return false
}

// XAISubscription identifies an xAI/Grok subscription profile.
//
// Distinct from the plain xAI API-key provider: this one authenticates with an
// OAuth credential from a SuperGrok or X Premium subscription and is served by
// the Responses API rather than chat/completions.
const XAISubscription = "xai"

// XaiBaseURL is the Grok inference endpoint.
//
// Read from the OpenClaw xAI plugin manifest rather than guessed. It is not the
// public xAI console URL that a plain API key uses, which is the kind of
// near-miss that produces an authenticated request answered by a 404.
const XaiBaseURL = "https://api.x.ai/v1"

// XaiModels lists the model IDs the xAI provider is known to serve, with their
// context windows.
//
// From the same manifest. The default is grok-4.7, which is also what OpenClaw
// selects when no model is named, so an account that works there works here.
var XaiModels = []struct {
	ID            string
	ContextWindow int
}{
	{"grok-4.7", 500_000},
	{"grok-4.6", 500_000},
	{"grok-4.5", 500_000},
	{"grok-build-0.1", 256_000},
	{"grok-4.3", 1_000_000},
	{"grok-4.20-0309-reasoning", 1_000_000},
	{"grok-4.20-0309-non-reasoning", 1_000_000},
}

// DefaultXaiModel is the model selected when xAI is chosen without one.
const DefaultXaiModel = "grok-4.7"

// XaiMaxOutputTokens is the output cap OpenClaw uses for xAI.
const XaiMaxOutputTokens = 64_000

// IsXaiModel reports whether a model ID is one the xAI endpoint is known to serve.
func IsXaiModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range XaiModels {
		if trimmed == known.ID {
			return true
		}
	}
	return false
}

// XaiContextWindow returns a model's context window, or 0 when it is unknown.
//
// Zero rather than a guess: an invented window either triggers compaction far
// too early or lets a request grow past what the endpoint accepts.
func XaiContextWindow(model string) int {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range XaiModels {
		if trimmed == known.ID {
			return known.ContextWindow
		}
	}
	return 0
}

// MiniMaxSubscription identifies a MiniMax subscription profile.
//
// Distinct from the MiniMax API-key provider: this one authenticates with an OAuth
// credential from an account login and is served by the Messages API, so it needs
// the Anthropic transport rather than chat completions.
const MiniMaxSubscription = "minimax"

// MiniMaxBaseURL is the global subscription inference endpoint for the agent grant.
//
// This is the agent backend, not the platform API, and the two are not
// interchangeable. A token from the mcode grant is only accepted here: the
// platform host answers it with a 401 whose message asks for an API secret key in
// X-Api-Key, which is a request for a credential this login does not have and
// never will, because the grant issues an agent token.
//
// The path already ends in the API version, so the transport's /v1/messages is
// appended to it rather than substituted for it.
const MiniMaxBaseURL = "https://agent.minimax.io/mavis/api/v1/llm/v1"

// MiniMaxMessagesAuth is the header style MiniMaxBaseURL authenticates with.
//
// The agent backend wants Authorization: Bearer and answers an x-api-key request
// with {"code":401,"message":"token is required"}. The platform API at
// api.minimax.io/anthropic is the opposite and is where an API secret key goes.
const MiniMaxMessagesAuth = MessagesAuthBearer

// MiniMaxCNBaseURL is the Chinese service. It is a separate account system, so a
// credential is not interchangeable with the global one.
//
// The agent host is agent.minimaxi.com, matching the api.minimaxi.com inference
// host above it; this is the one pair that could not be observed from here,
// because no Chinese credential is stored, and it is written from the vendor's
// own host list rather than from a probe.
const MiniMaxCNBaseURL = "https://agent.minimaxi.com/mavis/api/v1/llm/v1"

// DefaultMiniMaxModel is the model selected when MiniMax is chosen without one.
//
// M3 on the /anthropic path. It used to carry a comment saying M2.7 belonged to
// the OpenAI-compatible /v1 host instead, which the live check in
// TestLiveEveryServedModelIDIsHonoured disproves: the Messages API answers 200
// with model "MiniMax-M2.7" for that id, so it is served here too.
const DefaultMiniMaxModel = "MiniMax-M3"

// MiniMaxModels lists the model ids the subscription is known to serve.
//
// MiniMax-M3.1-Flash-Preview is not in the account's own /v1/models response,
// which still lists only the M3/M2 series with has_more: false. It was found by
// asking the Messages API and reading the model field the response echoes back,
// which is the only trustworthy signal here: the endpoint answers 200 to any
// model id at all and quietly serves MiniMax-M3, so a list that omits a real
// model is misleading in the same way a wrong id looks like it works.
var MiniMaxModels = []string{
	"MiniMax-M3.1-Flash-Preview",
	"MiniMax-M3",
	"MiniMax-M2.7",
}

// DeepSeekSubscription identifies a DeepSeek account profile.
//
// Distinct from a DeepSeek API-key profile: this one authenticates with the
// account grant from a browser sign-in and is served by the Messages API at
// /anthropic, so it needs the Anthropic transport with the dsh header style.
const DeepSeekSubscription = "deepseek"

// DeepSeekBaseURL is the account inference endpoint: the Anthropic Messages
// API on the grant's own host. The grant is accepted only here; the platform
// host that issued it answers the same token with a 401.
const DeepSeekBaseURL = "https://api.deepseek.com/anthropic"

// DeepSeekMessagesAuth is the header style DeepSeekBaseURL authenticates
// with: x-dsh-auth-token, not x-api-key and not a bearer token.
const DeepSeekMessagesAuth = MessagesAuthDsh

// DefaultDeepSeekModel is the model selected when DeepSeek is chosen without
// one.
//
// The provider's own catalog lists deepseek-flash first, so this is a judgment
// call rather than a mirror: v4-pro is described as suited to complex or
// quality-critical work, which is what an agent turn is. Both ids below come
// from packages/llm/llm-deepseek/src/models.ts; whether the account endpoint
// accepts them on the wire is unverified pending a live probe, and if it
// rejects one the picker still offers it, which is the failure to revisit.
const DefaultDeepSeekModel = "deepseek-v4-pro"

// DeepSeekModels lists the model ids the account catalog names.
var DeepSeekModels = []string{
	"deepseek-v4-pro",
	"deepseek-flash",
}

// DeepSeekContextWindow returns a model's context window, or 0 when unknown.
// Their defaults.ts states 1,000,000 for the catalog; unknown ids get zero
// rather than a guess, for the reason MiniMax's comment above gives.
func DeepSeekContextWindow(model string) int {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range DeepSeekModels {
		if trimmed == known {
			return 1000000
		}
	}
	return 0
}

// ClaudeSubscription identifies a Claude account profile: a Pro/Max OAuth
// grant from a browser sign-in, served by the native Messages API with the
// Bearer plus Claude Code beta header style.
const ClaudeSubscription = "claude"

// ClaudeBaseURL is the account inference endpoint: the native Anthropic
// Messages API. The grant is accepted only here.
const ClaudeBaseURL = "https://api.anthropic.com"

// ClaudeMessagesAuth is the header style ClaudeBaseURL authenticates an
// account grant with: Bearer plus the OAuth beta pair, not x-api-key.
const ClaudeMessagesAuth = MessagesAuthClaudeOAuth

// DefaultClaudeModel is the model selected when Claude is chosen without
// one. Sonnet is the agent workhorse: opus for the turns that earn it.
const DefaultClaudeModel = "claude-sonnet-4-5"

// ClaudeModels lists the model ids an account grant may serve. Bare family
// names resolve server-side on OAuth. Whether the endpoint accepts each id
// on the wire is unverified pending a live probe: if it rejects one the
// picker still offers it, which is the failure to revisit.
var ClaudeModels = []string{
	"claude-opus-4-1",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
	"claude-opus-4-0",
	"claude-sonnet-4-0",
	"claude-3-5-sonnet-latest",
	"claude-3-5-haiku-latest",
}

// IsClaudeModel reports whether the account endpoint serves a model id.
// Same shape as IsDeepSeekModel so every activation path asks the question
// the same way, rather than one path inheriting and another forcing.
func IsClaudeModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range ClaudeModels {
		if trimmed == strings.ToLower(known) {
			return true
		}
	}
	return false
}

// ClaudeContextWindow returns a model's context window, or 0 when unknown.
// Unknown ids get zero rather than a guess.
func ClaudeContextWindow(model string) int {
	if IsClaudeModel(model) {
		return 200000
	}
	return 0
}

// QoderModels are the models Qoder's own CLI reports for an account, established by
// running qodercli --list-models against a signed-in session rather than by
// guessing from another provider's catalogue.
//
// Seventeen entries, from the routing aliases (Auto, Ultimate) through to the
// third-party models, so this list is refreshed from the CLI rather than curated
// by hand. An alias such as Auto is a real choice: it lets Qoder route the turn,
// which is a different request from naming a model.
var QoderModels = []string{
	"Auto",
	"Ultimate",
	"Performance",
	"Efficient",
	"Sonus",
	"Cantus",
	"Qwen3.8-Max",
	"Qwen3.8-Flash",
	"Qwen3.7-Max",
	"Qwen3.7-Plus",
	"Kimi-K3",
	"Kimi-K2.8-Preview",
	"GLM-5.3",
	"GLM-5.3-Flash",
	"DeepSeek-V4-Pro",
	"DeepSeek-Flash",
	"MiniMax-M3",
}

// IsQoderModel reports whether a model id is one the subscription serves.
func IsQoderModel(model string) bool {
	for _, m := range QoderModels {
		if m == model {
			return true
		}
	}
	return false
}

// IsMiniMaxModel reports whether a model id is one the subscription serves.
func IsMiniMaxModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range MiniMaxModels {
		if trimmed == strings.ToLower(known) {
			return true
		}
	}
	return false
}

// IsDeepSeekModel reports whether the account endpoint serves a model id.
// Same shape as IsMiniMaxModel so every activation path asks the question
// the same way, rather than one path inheriting and another forcing.
func IsDeepSeekModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range DeepSeekModels {
		if trimmed == strings.ToLower(known) {
			return true
		}
	}
	return false
}

// ClineSubscription identifies a Cline Account profile.
//
// Not a vendor subscription but a hosted gateway: Cline serves chat completions
// for a set of models it pays for, including a free tier, metered against the
// account's plan. Signing in yields a token for that gateway rather than for
// Anthropic or anyone else.
const ClineSubscription = "cline"

// ClineGatewayBase is the Cline gateway's API root.
//
// OpenAI-compatible, so the existing chat transport serves it with no adaptation.
const ClineGatewayBase = "https://api.cline.bot/api/v1"

// ClineModels are the gateway's free-tier model ids, from the public catalog.
//
// Read from /ai/cline/recommended-models rather than assumed. The free tier is
// what a signed-in account can use without a paid plan, so it is the set worth
// defaulting to. The ids are namespaced by the tier that serves them.
var ClineModels = []struct {
	ID   string
	Name string
}{
	{"cline-free/deepseek-v4.1-flash", "Deepseek-v4.1-Flash"},
	{"cline-free/gemini-3.8-flash", "Gemini 3.8 Flash"},
	{"cline-free/mimo-v2.6-flash", "Mimo V2.6 Flash"},
	{"cline-free/muse-spark-1.3-contributor", "Muse Spark 1.3"},
	{"stealth/pixel-canary", "Pixel Canary"},
	{"stealth/space-bunny-alpha", "space-bunny-alpha"},
}

// DefaultClineModel is the model selected when Cline is chosen without one.
//
// The free Mimo flash model, because the free DeepSeek flash model returned
// successful but contentless completions in live Telegram use while Mimo
// answered normally.
const DefaultClineModel = "cline-free/mimo-v2.6-flash"

// IsClineModel reports whether a model id is one the gateway's free tier serves.
func IsClineModel(model string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	for _, known := range ClineModels {
		if trimmed == strings.ToLower(known.ID) {
			return true
		}
	}
	return false
}

// SubscriptionModels returns the model ids a subscription provider is known to
// serve, in the order they should be offered.
//
// The remote model catalog is not usable for these. It is a shared list keyed on
// OpenAI-compatible hosts and it only filters by a poolside prefix, so every
// subscription provider's models were absent from it and there was no way to
// pick one: the provider activated with a hardcoded default and nothing could
// change it. The lists here are the ones the transports already validate
// against, so a picker offers exactly what the endpoint will accept.
func SubscriptionModels(subscription string) []string {
	switch subscription {
	case CodexSubscription:
		return append([]string(nil), CodexModels...)
	case HermesSubscription:
		return append([]string(nil), HermesModels...)
	case XAISubscription:
		out := make([]string, 0, len(XaiModels))
		for _, model := range XaiModels {
			out = append(out, model.ID)
		}
		return out
	case MiniMaxSubscription:
		return append([]string(nil), MiniMaxModels...)
	case DeepSeekSubscription:
		return append([]string(nil), DeepSeekModels...)
	case ClaudeSubscription:
		return append([]string(nil), ClaudeModels...)
	case ClineSubscription:
		out := make([]string, 0, len(ClineModels))
		for _, model := range ClineModels {
			out = append(out, model.ID)
		}
		return out
	case QoderSubscription:
		return append([]string(nil), QoderModels...)
	case GeminiSubscription:
		return append([]string(nil), GeminiCodeAssistModels...)
	case WorkBuddySubscription:
		return append([]string(nil), WorkBuddyModels...)
	case OpenCodeSubscription:
		return append([]string(nil), OpenCodeModels...)
	default:
		return nil
	}
}

// IsSubscriptionModel reports whether a model belongs to a subscription provider.
//
// Kept separate from IsClineModel and friends because a caller picking a model is
// asking "does this provider serve it", not "is it the default".
func IsSubscriptionModel(subscription, model string) bool {
	for _, known := range SubscriptionModels(subscription) {
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(known)) {
			return true
		}
	}
	return false
}

// DefaultModelFor returns a subscription provider's default model.
//
// One place, so a picker and an activation cannot disagree about what the default
// is. Returns empty for a provider that is not a subscription.
func DefaultModelFor(subscription string) string {
	switch subscription {
	case CodexSubscription:
		return DefaultCodexModel
	case HermesSubscription:
		return DefaultHermesModel
	case XAISubscription:
		return DefaultXaiModel
	case MiniMaxSubscription:
		return DefaultMiniMaxModel
	case DeepSeekSubscription:
		return DefaultDeepSeekModel
	case ClaudeSubscription:
		return DefaultClaudeModel
	case ClineSubscription:
		return DefaultClineModel
	case OpenCodeSubscription:
		return DefaultOpenCodeModel
	case QoderSubscription:
		return DefaultQoderModel
	case GeminiSubscription:
		return DefaultGeminiCodeAssistModel
	case WorkBuddySubscription:
		return DefaultWorkBuddyModel
	default:
		return ""
	}
}

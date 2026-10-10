// Package llm handles outbound calls to OpenAI-compatible providers (Atria, etc.).
// It owns authentication, cancellation, and provider error normalization.
package llm

import (
	"bufio"
	"bytes"
	"ceraa/internal/envs"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ceraa/internal/paths"
)

type ProviderConfig struct {
	BaseURL                 string
	APIKey                  string
	Model                   string
	GeminiNative            bool
	GeminiThoughtSignatures bool
	// CodexNative selects the Codex subscription transport, which speaks the
	// Responses API against ChatGPT rather than chat completions.
	CodexNative bool
	// CodexTokens supplies the credential for CodexNative. Required when
	// CodexNative is set; without it the transport refuses the request instead of
	// sending an unauthenticated one.
	CodexTokens CodexTokenSource
	// XaiNative selects the xAI/Grok subscription transport.
	//
	// It speaks the same Responses API as Codex and differs only in where the
	// credential comes from, so it shares the transport rather than repeating it.
	// Verified from the xAI provider manifest: the base URL, the model list and
	// the transport choice. The request body itself is standard Responses API and
	// has not been exercised against a live Grok credential.
	XaiNative bool
	// XaiTokens supplies the credential for XaiNative. Required when XaiNative is
	// set, for the same reason CodexTokens is required for CodexNative.
	XaiTokens CodexTokenSource
	// AnthropicNative selects the Messages API transport, used by Anthropic
	// proper and by any provider that speaks it, which is currently MiniMax.
	AnthropicNative bool
	// AnthropicTokens supplies the credential for AnthropicNative. For a
	// subscription profile it is required; for an API-key profile the key is
	// carried on the profile instead.
	AnthropicTokens CodexTokenSource
	// QoderNative selects the Qoder subscription transport: the encrypted
	// agent_chat_generation endpoint with per-request WASM signing, not the
	// public OpenAI-compatible host.
	QoderNative bool
	// QoderCredentials supplies the identity a Qoder signature is bound to:
	// uid, access token and machine id. Required when QoderNative is set; the
	// transport refuses the request without it instead of sending one the
	// server must reject.
	QoderCredentials QoderCredentialSource
	// CodeAssistNative selects the Gemini Code Assist transport: the
	// v1internal streaming method with an OAuth bearer and the client
	// identity headers, not the public generateContent endpoint.
	CodeAssistNative bool
	// CodeAssistTokens supplies the Google OAuth access token, renewing on
	// expiry. Required when CodeAssistNative is set.
	CodeAssistTokens CodexTokenSource
	MaxRetries       int
	// RetryBackoff scales the delay between attempts. Zero uses the production
	// 1s/3s/8s schedule; tests set it to keep the retry path fast to exercise.
	RetryBackoff time.Duration
	// Timeout bounds how long the provider may go without sending anything, not
	// how long a whole request may take.
	//
	// It was a total budget on http.Client, which is the wrong shape for a
	// streaming API: a Gemini model at HIGH thinking can spend longer than two
	// minutes producing its first token, and every intermediate `data:` line
	// resets nothing because the deadline was set once when the request began.
	// A model that was working was reported as having timed out.
	//
	// As an idle window it has the meaning it should have had. The deadline resets
	// on every byte the provider sends, so a long stream runs as long as it keeps
	// making progress and a genuinely wedged connection still fails. Zero
	// disables it.
	Timeout         time.Duration
	ToolChoice      string
	PromptCacheMode string
	PromptCacheTTL  string
	Compatibility   *Compatibility
	// OnRetry fires before each retry wait with the error, the attempt number and
	// the delay chosen. A gateway backoff can run to about a minute, and a silent
	// wait that long looks exactly like a hang.
	OnRetry func(error, int, time.Duration)
	// ExtraBody is merged into every chat-completions request body. Used by
	// providers that require extra top-level fields. Set by the caller; never
	// mutated here.
	ExtraBody map[string]any
	// ExtraBodyFunc is evaluated per request, for metadata that must carry a
	// fresh correlation id on every call. Its result is merged after ExtraBody
	// and is likewise never allowed to shadow a field Ceraa owns.
	ExtraBodyFunc func() map[string]any
	// ExtraHeaders are added to every request. Used for provider-required
	// client identification.
	ExtraHeaders map[string]string
	// ExtraHeadersFunc is evaluated per request, for header metadata that must
	// carry a fresh correlation id on every call. It receives the session id the
	// transport resolved for this request, which is the value a static map cannot
	// know: the map is computed once when the config is built, so a per-request id
	// placed there would be identical for the life of the provider.
	//
	// Its result is merged over ExtraHeaders, so a value it sets wins, and it is
	// never allowed to shadow the Content-Type, Accept or Authorization the
	// transport owns.
	ExtraHeadersFunc func(sessionID string) map[string]string
	// OpenCodeGateway marks a provider as opencode.ai's Zen gateway, whose free
	// tier answers only requests that look like the OpenCode client's own.
	//
	// The header half of that contract is the caller's, carried in ExtraHeaders.
	// This is the body half, and it exists because the two are checked together:
	// a correct header set on a body the gateway does not recognise still comes
	// back as FreeTierError, and so does a correct body on a request with
	// somebody else's User-Agent.
	//
	// Set by opencodeauth.ApplyCredential, so a Zen profile picks it up without
	// the four call sites that build one having to know about it.
	OpenCodeGateway bool
	// MessagesAuthStyle chooses the header the Messages transport authenticates
	// with. Empty means x-api-key, which is what Anthropic and MiniMax's platform
	// API both want.
	//
	// This is not a preference. The two disagree, and each rejects the other's
	// credential with a 401 whose message names the header it wanted:
	//
	//	api.minimax.io/anthropic      wants x-api-key, refuses a bearer token
	//	agent.minimax.io/mavis/...    wants Authorization: Bearer, and answers
	//	                              x-api-key with {"token is required"}
	//
	// A token from MiniMax's agent grant is only accepted by the second, so a
	// correct OAuth flow still produces a provider that cannot be called.
	MessagesAuthStyle string
	// RecoverError is consulted when the provider returns an error that a plain
	// retry cannot fix. It returns whether the request may be retried once
	// after the hook repaired provider state, plus a user-facing message.
	//
	// Typical use: a hosted provider gates requests behind a session, so a gate
	// rejection must re-admit the session before the request can succeed. The
	// hook must be fast and idempotent, and retries are bounded by
	// MaxRecoveries so a broken hook cannot spin.
	RecoverError func(ctx context.Context, status int, body []byte) (retry bool, message string)
	// HTTPClient overrides the transport. Used for a custom TLS trust store or
	// proxy; Timeout still applies when unset.
	HTTPClient *http.Client
}

// MaxRecoveries bounds RecoverError-driven retries per request. Two is enough
// for a session re-admission: one to detect the gate, one to confirm the retry
// actually worked before giving up.
const MaxRecoveries = 2

type Compatibility struct {
	MaxTokensField         string
	SupportsUsageStreaming bool
	SupportsFinishReason   bool
	SupportsDeveloperRole  bool
	RequiresToolResultName bool
	StrictTools            bool
	ReasoningField         string
}

func DefaultConfig() ProviderConfig {
	return FromEnv()
}

// FromEnv reads provider config from environment.
func FromEnv() ProviderConfig {
	api := envs.Get("API_URL")
	key := envs.GetWith("CERAA_API_KEY", "OPENAI_API_KEY")
	model := envs.Get("MODEL")
	return ProviderConfig{
		BaseURL:    api,
		APIKey:     key,
		Model:      model,
		MaxRetries: 3,
		// Idle window, not a total budget. See ProviderConfig.Timeout.
		Timeout: 120 * time.Second,
	}
}

// FromProvider builds ProviderConfig from individual settings.
//
// Transport selection happens here so every call site gets it: a Gemini model
// on Google's host uses the native API unless the URL explicitly asks for the
// OpenAI-compatibility surface.
func FromProvider(baseURL, apiKey, model string) ProviderConfig {
	return ProviderConfig{
		BaseURL:                 baseURL,
		APIKey:                  apiKey,
		Model:                   model,
		GeminiNative:            UsesGeminiNative(model, baseURL),
		GeminiThoughtSignatures: UsesGeminiThoughtSignatures(model, baseURL),
		MaxRetries:              3,
		// Idle window, not a total budget. See ProviderConfig.Timeout.
		Timeout: 120 * time.Second,
	}
}

func defaultCompatibility() Compatibility {
	return Compatibility{
		MaxTokensField:         "max_tokens",
		SupportsUsageStreaming: true,
		SupportsFinishReason:   false,
		RequiresToolResultName: true,
		ReasoningField:         "reasoning_content",
	}
}

func compatibilityFor(cfg ProviderConfig) Compatibility {
	compatibility := defaultCompatibility()
	base := strings.ToLower(cfg.BaseURL)
	model := strings.ToLower(cfg.Model)
	if strings.Contains(base, "openai.com") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
		compatibility.MaxTokensField = "max_completion_tokens"
		compatibility.SupportsDeveloperRole = true
		compatibility.SupportsFinishReason = true
	}
	if strings.Contains(base, "openrouter.ai") {
		compatibility.MaxTokensField = "max_tokens"
		compatibility.SupportsDeveloperRole = false
	}
	if cfg.Compatibility != nil {
		provided := *cfg.Compatibility
		if provided.MaxTokensField != "" {
			compatibility.MaxTokensField = provided.MaxTokensField
		}
		if provided.ReasoningField != "" {
			compatibility.ReasoningField = provided.ReasoningField
		}
		compatibility.SupportsUsageStreaming = provided.SupportsUsageStreaming
		compatibility.SupportsFinishReason = provided.SupportsFinishReason
		compatibility.SupportsDeveloperRole = provided.SupportsDeveloperRole
		compatibility.RequiresToolResultName = provided.RequiresToolResultName
		compatibility.StrictTools = provided.StrictTools
	}
	return compatibility
}

type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type StreamedToolCall struct {
	Index            int
	ID               string
	Name             string
	Arguments        string
	ThoughtSignature string
}

type ProviderResponse struct {
	Content          string
	Reasoning        string
	ToolCalls        []StreamedToolCall
	Done             bool
	FinishReason     string
	ResponseID       string
	ResponseModel    string
	ReasoningDetails json.RawMessage
	Usage            *UsageInfo
	// UsageRaw is the provider's usage object exactly as it arrived.
	//
	// It exists because the parsed form cannot answer the question it was added
	// for. Ceraa reads cached tokens from input_tokens_details.cached_tokens, the
	// Responses API spelling; the Chat Completions spelling is
	// prompt_tokens_details.cached_tokens. A provider that returns the other shape
	// is reporting a perfectly good cache hit in a field we do not read, and the
	// run records a confident zero. Only the raw object separates "no cache hit"
	// from "hit reported somewhere else".
	UsageRaw json.RawMessage
}

type UsageInfo struct {
	InputTokens       int `json:"input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   int `json:"reasoning_tokens,omitempty"`
	TotalTokens       int `json:"total_tokens,omitempty"`
}

type ProviderError struct {
	Status     int
	Body       string
	Retryable  bool
	RetryAfter time.Duration
	// Recovered is set when RecoverError repaired provider state and the
	// request was re-issued.
	Recovered bool
	// RecoverMessage is a user-facing explanation produced by RecoverError.
	RecoverMessage string
}

type StreamError struct {
	Err       error
	Retryable bool
	Partial   bool
}

func (e *StreamError) Error() string { return e.Err.Error() }
func (e *StreamError) Unwrap() error { return e.Err }

func (e *ProviderError) Error() string {
	detail := e.Body
	if e.Status == 0 {
		detail = "provider request failed: " + e.Body
	} else {
		detail = fmt.Sprintf("provider error %d: %s", e.Status, e.Body)
	}
	// A provider-supplied explanation is far more useful to the user than a raw
	// status line, so it leads and the raw detail follows for diagnosis.
	if e.RecoverMessage != "" {
		return e.RecoverMessage + " (" + detail + ")"
	}
	return detail
}

// RequestTokenBreakdown is one request's share of the input, by source.
type RequestTokenBreakdown struct {
	// Step is the loop iteration, so the requests line up with the transcript.
	Step int `json:"step"`
	// PromptTokens is the provider-reported input for this request when it is
	// available, and zero when it is not. A provider that reports a running total
	// leaves this empty rather than a wrong per-request number.
	PromptTokens int `json:"prompt_tokens,omitempty"`
	// CachedTokens is the cached part of PromptTokens. Present only when the
	// provider distinguishes it, because the ratio is the number that decides
	// whether a cache change worked.
	CachedTokens int            `json:"cached_tokens,omitempty"`
	Breakdown    TokenBreakdown `json:"breakdown"`
}

type TokenBreakdown struct {
	System       int `json:"system,omitempty"`
	Tools        int `json:"tools,omitempty"`
	User         int `json:"user,omitempty"`
	Conversation int `json:"conversation,omitempty"`
	ToolOutput   int `json:"tool_output,omitempty"`
	Reasoning    int `json:"reasoning,omitempty"`
	Compaction   int `json:"compaction,omitempty"`
	Other        int `json:"other,omitempty"`
}

func (u UsageInfo) normalized() UsageInfo {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
		return u
	}
	if u.InputTokens == 0 {
		u.InputTokens = u.TotalTokens - u.OutputTokens
		if u.InputTokens < 0 {
			u.InputTokens = 0
		}
	}
	if u.OutputTokens == 0 {
		u.OutputTokens = u.TotalTokens - u.InputTokens
		if u.OutputTokens < 0 {
			u.OutputTokens = 0
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

func (u UsageInfo) UncachedInputTokens() int {
	value := u.normalized().InputTokens - u.CachedInputTokens
	if value < 0 {
		return 0
	}
	return value
}

// ContextTokens is the size of the request that produced this usage: the whole
// prompt the provider counted, cached or not, plus the tokens it returned.
//
// This is the figure a context budget must be compared against, and it is not
// TotalTokens. On a split-reporting endpoint (DeepSeek, native Anthropic)
// InputTokens holds only the fresh tokens and the cache hit is reported beside
// it, so TotalTokens drops to a few thousand on a turn that re-read a 76k
// prefix. Adding the cached figure back is what the CacheReadRatio doc below
// calls selecting the denominator; everywhere else a cache hit is a subset of
// the input and adding it would count the same tokens twice.
func (u UsageInfo) ContextTokens() int {
	v := u.normalized()
	prompt := v.InputTokens
	if v.CachedInputTokens > v.InputTokens {
		// Split reporting: the cached prefix is reported separately and is not
		// part of InputTokens.
		prompt += v.CachedInputTokens
	}
	total := prompt + v.OutputTokens + v.ReasoningTokens
	if v.TotalTokens > total {
		total = v.TotalTokens
	}
	return total
}

func (u UsageInfo) CacheReadRatio() float64 {
	if u.InputTokens <= 0 || u.CachedInputTokens <= 0 {
		return 0
	}
	// Split-reporting endpoints (DeepSeek, native Anthropic) count only fresh
	// tokens in input, so cached/input exceeds 1 and saturates. Cached can only
	// exceed input under that convention, since a cache hit is a subset of the
	// prompt everywhere else, so the comparison itself selects the denominator.
	if u.CachedInputTokens > u.InputTokens {
		return float64(u.CachedInputTokens) / float64(u.InputTokens+u.CachedInputTokens)
	}
	ratio := float64(u.CachedInputTokens) / float64(u.InputTokens)
	if ratio > 1 {
		return 1
	}
	return ratio
}

func (u *UsageInfo) Add(delta UsageInfo) {
	delta = delta.normalized()
	u.InputTokens += delta.InputTokens
	u.OutputTokens += delta.OutputTokens
	u.CachedInputTokens += delta.CachedInputTokens
	u.CacheWriteTokens += delta.CacheWriteTokens
	u.ReasoningTokens += delta.ReasoningTokens
	u.TotalTokens += delta.TotalTokens
}

func (u *UsageInfo) UnmarshalJSON(data []byte) error {
	var raw struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		PromptTokens             int `json:"prompt_tokens"`
		CompletionTokens         int `json:"completion_tokens"`
		TotalTokens              int `json:"total_tokens"`
		CachedInputTokens        int `json:"cached_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheWriteTokens         int `json:"cache_write_tokens"`
		ReasoningTokens          int `json:"reasoning_tokens"`
		PromptTokensDetails      struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	u.InputTokens = raw.InputTokens
	if u.InputTokens == 0 {
		u.InputTokens = raw.PromptTokens
	}
	u.OutputTokens = raw.OutputTokens
	if u.OutputTokens == 0 {
		u.OutputTokens = raw.CompletionTokens
	}
	u.TotalTokens = raw.TotalTokens
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	u.CachedInputTokens = raw.PromptTokensDetails.CachedTokens
	if u.CachedInputTokens == 0 {
		u.CachedInputTokens = raw.InputTokensDetails.CachedTokens
	}
	if u.CachedInputTokens == 0 {
		u.CachedInputTokens = raw.CachedInputTokens
	}
	if u.CachedInputTokens == 0 {
		u.CachedInputTokens = raw.CacheReadInputTokens
	}
	u.CacheWriteTokens = raw.CacheWriteTokens
	if u.CacheWriteTokens == 0 {
		u.CacheWriteTokens = raw.CacheCreationInputTokens
	}
	u.ReasoningTokens = raw.ReasoningTokens
	if u.ReasoningTokens == 0 {
		u.ReasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}
	return nil
}

func (b TokenBreakdown) Total() int {
	return b.System + b.Tools + b.User + b.Conversation + b.ToolOutput + b.Reasoning + b.Compaction + b.Other
}

func (b *TokenBreakdown) Add(other TokenBreakdown) {
	b.System += other.System
	b.Tools += other.Tools
	b.User += other.User
	b.Conversation += other.Conversation
	b.ToolOutput += other.ToolOutput
	b.Reasoning += other.Reasoning
	b.Compaction += other.Compaction
	b.Other += other.Other
}

func EstimateToolDefinitionTokens(tools []ToolDef) int {
	total := 0
	for _, item := range tools {
		data, _ := json.Marshal(item)
		total += estimateTokens(string(data))
	}
	return total
}

func EstimateTokenBreakdown(messages []Item, tools []ToolDef) TokenBreakdown {
	var breakdown TokenBreakdown
	breakdown.Tools = EstimateToolDefinitionTokens(tools)
	for _, item := range messages {
		tokens := estimateTokens(item.Text) + estimateTokens(item.Args) + estimateTokens(item.Output) + estimateTokens(item.Reason) + estimateTokens(string(item.ReasoningDetails))
		tokens += estimateTokens(item.CallID) + estimateTokens(item.Name)
		for _, call := range item.ToolCalls {
			tokens += estimateTokens(call.ID) + estimateTokens(call.Name) + estimateTokens(call.Args)
		}
		tokens += 10
		switch {
		case strings.Contains(item.Text, "<conversation-checkpoint>"):
			breakdown.Compaction += tokens
		case item.Role == RoleSystem:
			breakdown.System += tokens
		case item.Kind == "reasoning" || item.Reason != "":
			breakdown.Reasoning += tokens
		case item.Role == RoleTool:
			breakdown.ToolOutput += tokens
		case item.Role == RoleUser:
			breakdown.User += tokens
		case item.Kind == "tool_call" || item.Kind == "tool_calls" || item.Role == RoleAssistant:
			breakdown.Conversation += tokens
		default:
			breakdown.Other += tokens
		}
	}
	return breakdown
}

func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s) / 4
}

type StreamRequest struct {
	Messages       []Item
	Tools          []ToolDef
	Reason         ReasonLevel
	MaxTokens      int
	ToolChoice     string
	PromptCacheKey string
	SessionID      string
	// SystemCacheSeam is the byte offset in the system prompt where stable text
	// ends and volatile text begins. Everything before it is eligible for a cache
	// breakpoint; everything after is re-read. Zero means no seam, which leaves
	// the whole prompt uncached rather than guessing.
	SystemCacheSeam int
}

type StreamingClient struct {
	cfg  ProviderConfig
	http *http.Client
}

func New(cfg ProviderConfig) *StreamingClient {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// No total Timeout. It capped the whole exchange including the body, so a
		// long stream was killed while it was still producing tokens. cfg.Timeout
		// is applied as an idle window on the response body instead, and the header
		// wait is bounded here since body traffic cannot reset that phase.
		httpClient = &http.Client{Transport: idleTimeoutTransport(cfg.Timeout)}
	}
	return &StreamingClient{cfg: cfg, http: httpClient}
}

// skipThoughtSignatureValidator is one of the two sentinels Google documents
// for a functionCall part that did not come from the API and therefore can
// never carry a real signature. The native transport uses it for exactly that
// case; the OpenAI-compatibility adapter does not use it at all, because
// replaying a fabricated signature through the adapter is guesswork about how
// a shim translates an ad-hoc field.
const skipThoughtSignatureValidator = "skip_thought_signature_validator"

// geminiToolCallExtraContent replays a thought signature on the
// OpenAI-compatibility adapter.
//
// Only a signature the model actually returned is replayed. Nothing is
// invented: a call with no signature stays unsigned, because the compat layer
// gives no way to tell whether its shim would honour a bypass value or reject
// it. Gemini configured against Google's native API is the supported path.
func geminiToolCallExtraContent(signature string, enabled bool) map[string]any {
	if !enabled {
		return nil
	}
	if strings.TrimSpace(signature) == "" {
		return nil
	}
	return map[string]any{"google": map[string]any{"thought_signature": signature}}
}

func encodeMessages(items []Item, geminiThoughtSignatures bool) []map[string]any {
	return encodeMessagesWithCompatibility(items, geminiThoughtSignatures, defaultCompatibility())
}

func encodeMessagesWithCompatibility(items []Item, geminiThoughtSignatures bool, compatibility Compatibility) []map[string]any {
	items = normalizeItemsForChat(items)
	// Resolve tool-call names up front so a tool result can always name the call
	// it answers, even when the stored message lost the name.
	callNames := toolCallNames(items)
	// A function call must be answered by exactly one tool message. History
	// recorded across an interrupted run can hold two results for the same call
	// id, which every OpenAI-compatible provider treats as malformed; Gemini
	// reports it as a missing thought_signature. Collapse to one here so an
	// already-broken session recovers without rewriting its history.
	canonicalResult := canonicalToolResults(items)
	emittedResult := map[string]bool{}
	msgs := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Kind {
		case "text":
			role := string(item.Role)
			if item.Role == RoleSystem && compatibility.SupportsDeveloperRole {
				role = "developer"
			}
			if item.Role == RoleAssistant && strings.TrimSpace(item.Text) == "" {
				continue
			}
			msg := map[string]any{"role": role}
			if item.Role == RoleTool {
				if item.CallID != "" {
					chosen, ok := canonicalResult[item.CallID]
					if ok {
						// Emit only the canonical result for this call id.
						if emittedResult[item.CallID] || item.Output != chosen {
							continue
						}
						emittedResult[item.CallID] = true
					}
				}
				msg["tool_call_id"] = item.CallID
				output := item.Output
				if strings.TrimSpace(output) == "" {
					output = "No output was produced."
				}
				// An image a read attached becomes a multimodal tool result, in
				// the chat shape. Done here rather than in the Messages encoder so
				// reading a picture is not a property of one transport.
				if parts := chatImageParts(item.Images); len(parts) > 0 {
					msg["content"] = append([]any{map[string]any{"type": "text", "text": output}}, parts...)
				} else {
					msg["content"] = output
				}
				if compatibility.RequiresToolResultName {
					// Some providers reject the whole request when a tool result
					// has no name. Gemini reports it as
					// "function_response.name: Name cannot be empty", so the
					// name is guaranteed non-empty rather than omitted.
					name := strings.TrimSpace(item.Name)
					if name == "" {
						name = strings.TrimSpace(callNames[item.CallID])
					}
					if name == "" {
						name = fallbackToolResultName(item.CallID)
					}
					msg["name"] = name
				}
			} else {
				msg["content"] = item.Text
				if item.Role == RoleAssistant {
					addReasoningFields(msg, item, compatibility)
				}
			}
			msgs = append(msgs, msg)
		case "reasoning":
			if item.Reason != "" || len(item.ReasoningDetails) > 0 {
				message := map[string]any{"role": "assistant"}
				if item.Reason != "" {
					message[compatibility.ReasoningField] = item.Reason
				}
				if len(item.ReasoningDetails) > 0 {
					message["reasoning_details"] = item.ReasoningDetails
				}
				msgs = append(msgs, message)
			}
		case "tool_call":
			args := item.Args
			if repaired, changed := RepairToolArgumentsJSON(args); changed {
				args = repaired
			}
			call := map[string]any{
				"id": item.CallID, "type": "function", "function": map[string]any{"name": item.Name, "arguments": args},
			}
			if extra := geminiToolCallExtraContent(item.ThoughtSignature, geminiThoughtSignatures); extra != nil {
				call["extra_content"] = extra
			}
			assistant := map[string]any{"role": "assistant", "tool_calls": []map[string]any{call}}
			if item.Text != "" {
				assistant["content"] = item.Text
			}
			addReasoningFields(assistant, item, compatibility)
			msgs = append(msgs, assistant)
		case "tool_calls":
			calls := make([]map[string]any, 0, len(item.ToolCalls))
			for _, call := range item.ToolCalls {
				args := call.Args
				if repaired, changed := RepairToolArgumentsJSON(args); changed {
					args = repaired
				}
				encoded := map[string]any{
					"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": args},
				}
				if extra := geminiToolCallExtraContent(call.ThoughtSignature, geminiThoughtSignatures); extra != nil {
					encoded["extra_content"] = extra
				}
				calls = append(calls, encoded)
			}
			assistant := map[string]any{"role": "assistant", "tool_calls": calls}
			if item.Text != "" {
				assistant["content"] = item.Text
			}
			addReasoningFields(assistant, item, compatibility)
			msgs = append(msgs, assistant)
		}
	}
	return msgs
}

func addReasoningFields(message map[string]any, item Item, compatibility Compatibility) {
	if item.Reason != "" {
		message[compatibility.ReasoningField] = item.Reason
	}
	if len(item.ReasoningDetails) > 0 {
		message["reasoning_details"] = item.ReasoningDetails
	}
}

// chatImageParts turns image paths into chat-completions image_url parts, loading
// and resizing at send time. A file that has gone missing yields no part, so the
// turn degrades to the result text rather than failing.
func chatImageParts(paths []string) []any {
	parts := make([]any, 0, len(paths))
	for _, path := range paths {
		attachment, err := LoadMediaAttachment(path)
		if err != nil || attachment == nil {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + attachment.MimeType + ";base64," + attachment.Base64Data()},
		})
	}
	return parts
}

func normalizeItemsForChat(items []Item) []Item {
	results := make(map[string]struct{})
	for _, item := range items {
		if item.Kind == "text" && item.Role == RoleTool && item.CallID != "" {
			results[item.CallID] = struct{}{}
		}
	}
	normalized := make([]Item, 0, len(items))
	for _, item := range items {
		normalized = append(normalized, item)
		var calls []ToolCall
		switch item.Kind {
		case "tool_call":
			if item.CallID != "" {
				calls = []ToolCall{{ID: item.CallID, Name: item.Name}}
			}
		case "tool_calls":
			calls = item.ToolCalls
		}
		for _, call := range calls {
			if call.ID == "" {
				continue
			}
			if _, ok := results[call.ID]; ok {
				continue
			}
			results[call.ID] = struct{}{}
			normalized = append(normalized, Item{Kind: "text", Role: RoleTool, CallID: call.ID, Name: call.Name, Output: "Error: tool result was not recorded", Text: "Error: tool result was not recorded"})
		}
	}
	return normalized
}

func isOpenRouterEndpoint(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
}

func isPromptCacheKeyEndpoint(baseURL string) bool {
	base := strings.ToLower(baseURL)
	return strings.Contains(base, "openai.com") || isOpenRouterEndpoint(base)
}

// promptCacheKeyMaxLen is the longest prompt cache key OpenAI accepts. A longer
// key is not an error, it is silently useless, so keys are clamped to fit.
const promptCacheKeyMaxLen = 64

// clampPromptCacheKey trims a cache key to the length the API accepts. The
// coordinator's key is a prefix plus two hex digests and lands at 83
// characters, well past the limit; over-length keys still return 200 but do
// not buy reliable reuse.
func clampPromptCacheKey(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= promptCacheKeyMaxLen {
		return key
	}
	return string(runes[:promptCacheKeyMaxLen])
}

func promptCacheMode(cfg ProviderConfig) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.PromptCacheMode))
	if mode == "" {
		return "auto"
	}
	return mode
}

func addPromptCacheFields(body map[string]any, cfg ProviderConfig, req StreamRequest) string {
	mode := promptCacheMode(cfg)
	if mode == "off" || req.PromptCacheKey == "" {
		return ""
	}
	key := clampPromptCacheKey(req.PromptCacheKey)
	if isPromptCacheKeyEndpoint(cfg.BaseURL) || mode == "explicit" {
		body["prompt_cache_key"] = key
	}
	if !isOpenRouterEndpoint(cfg.BaseURL) {
		return ""
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = req.PromptCacheKey
	}
	body["session_id"] = sessionID
	if mode != "explicit" {
		return sessionID
	}
	// The marker is placed on content blocks by applyCacheBreakpoints, not here.
	// Writing cache_control onto the request body put it in a field no API reads,
	// so explicit mode returned 200 and changed nothing.
	return sessionID
}

// applyCacheBreakpoints marks where the provider should end its cached prefix.
//
// Only called in explicit mode. Auto mode is left alone: it is the default for
// every provider, and turning a content string into a block array changes the
// request shape for endpoints that may not accept it.
func applyCacheBreakpoints(msgs []map[string]any, cfg ProviderConfig, systemSeam int) {
	if promptCacheMode(cfg) != "explicit" {
		return
	}
	ttl, err := ParseCacheTTL(cfg.PromptCacheTTL)
	if err != nil {
		// An unparseable TTL falls back to the default rather than sending
		// something the provider will reject, and the misconfiguration is
		// reported by the caller reading the run's cache notes.
		ttl = CacheTTL5m
	}
	for i := range msgs {
		if role, _ := msgs[i]["role"].(string); role == "system" || role == "developer" {
			if text, ok := msgs[i]["content"].(string); ok {
				msgs[i]["content"] = toAnyBlocks(SystemBlocks(text, systemSeam, ttl))
			}
		}
	}
	markLastStableMessage(msgs, ttl)
}

func toAnyBlocks(blocks []map[string]any) []any {
	out := make([]any, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, block)
	}
	return out
}

func (c *StreamingClient) Stream(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	// An empty endpoint used to surface as "unsupported protocol scheme \"\""
	// after three pointless retries, which reads like a network fault instead of
	// a missing provider. Name the actual problem instead.
	if strings.TrimSpace(c.cfg.BaseURL) == "" {
		note := fmt.Sprintf("gemini-no-endpoint model=%s gemini=%v items=%d\n", c.cfg.Model, c.cfg.GeminiNative, len(req.Messages))
		diagNote(note)
		writeDebugNote("gemini.log", note)
		return fmt.Errorf("no provider endpoint configured for model %q: set a base URL via provider onboarding or CERAA_API_URL", c.cfg.Model)
	}
	// Gemini is served by Google's native API, where a thought signature is a
	// first-class field rather than an ad-hoc extra_content field. Only an
	// explicitly configured OpenAI-compatibility URL uses the adapter below.
	if c.cfg.GeminiNative {
		return c.streamGemini(ctx, req, onDelta)
	}
	// Codex is served by ChatGPT's own endpoint, which is neither Gemini's native
	// API nor an OpenAI-compatible chat-completions host, so it gets its own
	// transport rather than a compatibility mode.
	if c.cfg.CodexNative {
		if strings.TrimSpace(c.cfg.BaseURL) == "" {
			c.cfg.BaseURL = CodexBaseURL
		}
		return c.streamCodex(ctx, req, onDelta)
	}
	if c.cfg.XaiNative {
		if strings.TrimSpace(c.cfg.BaseURL) == "" {
			c.cfg.BaseURL = XaiBaseURL
		}
		return c.streamXai(ctx, req, onDelta)
	}
	if c.cfg.AnthropicNative {
		return c.streamAnthropic(ctx, req, onDelta)
	}
	// Qoder's encrypted endpoint needs per-request signing, so it rides its own
	// transport rather than the OpenAI-compatible path.
	if c.cfg.QoderNative {
		return c.streamQoder(ctx, req, onDelta)
	}
	// Code Assist speaks native generateContent shapes on its own method with
	// its own envelope, so it rides its own transport too.
	if c.cfg.CodeAssistNative {
		return c.streamCodeAssist(ctx, req, onDelta)
	}
	return c.streamWithReasoningRetry(ctx, req, onDelta, c.streamOpenAI)
}

// streamWithReasoningRetry calls a transport and, if the provider rejected the
// request over the reasoning level before a single token arrived, asks once
// more without it.
//
// A level the model does not accept is a request the provider refuses, and
// refusing is fatal: the run dies with a 400 and no diagnosis. The retry is a
// cache read of the same prefix, so the second attempt costs almost nothing,
// and it only fires when nothing was streamed, because a retry after deltas
// would repeat content that has already been shown.
func (c *StreamingClient) streamWithReasoningRetry(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse), stream func(context.Context, StreamRequest, func(ProviderResponse)) error) error {
	emitted := 0
	tracked := func(delta ProviderResponse) {
		if onDelta != nil {
			onDelta(delta)
		}
		emitted++
	}
	err := stream(ctx, req, tracked)
	if !retryWithoutReasoning(err, req.Reason) || emitted > 0 {
		return err
	}
	req.Reason = ""
	return stream(ctx, req, tracked)
}

// retryWithoutReasoning reports whether an error is a provider refusing the
// reasoning level, which is the one failure worth asking again about.
//
// The bodies differ per vendor - "reasoning_effort", "output_config.effort",
// "thinking", and a plain mention of the parameter - so the match is on the
// parameter name rather than on one vendor's wording. A 400 without any of
// them is a genuinely bad request and retrying would only repeat it.
func retryWithoutReasoning(err error, reason ReasonLevel) bool {
	if err == nil || strings.TrimSpace(string(reason)) == "" {
		return false
	}
	status, body := errorStatusAndBody(err)
	if status != http.StatusBadRequest {
		return false
	}
	lowered := strings.ToLower(body)
	for _, needle := range []string{"reasoning", "effort", "thinking", "output_config"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// streamOpenAI is the OpenAI-compatible path: chat completions on any host
// that speaks it.
func (c *StreamingClient) streamOpenAI(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	compatibility := compatibilityFor(c.cfg)
	url := chatCompletionsURL(c.cfg.BaseURL)
	msgs := encodeMessagesWithCompatibility(req.Messages, c.cfg.GeminiThoughtSignatures, compatibility)

	body := map[string]any{
		"model":    c.cfg.Model,
		"messages": msgs,
		"stream":   true,
	}
	if compatibility.SupportsUsageStreaming {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.Reason != "" {
		body["reasoning_effort"] = string(req.Reason)
	}
	if req.MaxTokens > 0 {
		body[compatibility.MaxTokensField] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		toolMaps := make([]map[string]any, len(req.Tools))
		for i, tool := range req.Tools {
			function := map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			}
			if compatibility.StrictTools {
				function["strict"] = true
			}
			toolMaps[i] = map[string]any{"type": "function", "function": function}
		}
		body["tools"] = toolMaps
		body["tool_choice"] = req.ToolChoice
		if body["tool_choice"] == "" {
			body["tool_choice"] = c.cfg.ToolChoice
		}
		if body["tool_choice"] == "" {
			body["tool_choice"] = "auto"
		}
	}
	if c.cfg.OpenCodeGateway {
		// After the tools are in the body and before anything is cached or
		// merged in, so the shape check sees what the model was actually offered
		// and ExtraBody still wins where it overlaps.
		EnsureOpenCodeTraffic(body)
	}
	sessionID := addPromptCacheFields(body, c.cfg, req)
	// After the messages are in the body, so the breakpoints can land on them.
	if msgs, ok := body["messages"].([]map[string]any); ok {
		applyCacheBreakpoints(msgs, c.cfg, req.SystemCacheSeam)
	}
	extras := c.cfg.ExtraBody
	if c.cfg.ExtraBodyFunc != nil {
		// Evaluated per request so correlation ids are never reused.
		extras = c.cfg.ExtraBodyFunc()
	}
	for key, value := range extras {
		if _, reserved := body[key]; reserved {
			// Never let a provider extra shadow a field Ceraa owns.
			continue
		}
		body[key] = value
	}

	// A Gemini 400 naming a thought_signature cannot be diagnosed from the error
	// text, which has produced several wrong diagnoses in a row. Record the exact
	// turn shape that goes on the wire: which messages, which tool calls, and for
	// each one whether a signature is present. Message content is never logged.
	if c.cfg.GeminiThoughtSignatures {
		stats := geminiSignatureStats(req.Messages)
		note := fmt.Sprintf("gemini-compat model=%s host=%s items=%d signed_calls=%d unsigned_calls=%d turn=%s\n",
			c.cfg.Model, requestHost(c.cfg.BaseURL), len(req.Messages), stats.signed, stats.unsigned, describeEncodedTurn(msgs))
		// Written to the log file, and to whatever diagNote currently points at.
		// The file is the durable copy: a run's stderr belongs to whatever
		// terminal launched it and cannot be read from afterwards.
		diagNote(note)
		writeDebugNote("gemini.log", note)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode provider request: %w", err)
	}

	var lastErr error
	recoveries := 0
	// attempts tracks completed provider round trips, including the one in flight
	// when the loop exits, so the reported count is the number actually made.
	attempts := 0
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		attempts = attempt + 1
		hreq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		if sessionID != "" {
			hreq.Header.Set("x-session-id", sessionID)
		}
		for key, value := range c.cfg.ExtraHeaders {
			hreq.Header.Set(key, value)
		}
		if c.cfg.ExtraHeadersFunc != nil {
			// Evaluated per request so correlation ids are never reused.
			//
			// The session handed over is req.SessionID rather than the sessionID
			// local above: that one is the prompt-cache session, which is empty
			// for every provider that is not OpenRouter and whenever caching is
			// off. Passing it through would hand a header builder an empty string
			// for most installs.
			headerSession := req.SessionID
			if headerSession == "" {
				headerSession = sessionID
			}
			for key, value := range c.cfg.ExtraHeadersFunc(headerSession) {
				switch http.CanonicalHeaderKey(key) {
				case "Content-Type", "Accept", "Authorization":
					// The transport owns these. A provider extra naming one of
					// them would replace the credential the config set above.
					continue
				}
				hreq.Header.Set(key, value)
			}
		}
		if c.cfg.APIKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
		resp, err := c.doRequest(hreq)
		if err != nil {
			lastErr = fmt.Errorf("request: %w", err)
			if attempt < c.cfg.MaxRetries {
				if !sleepContext(ctx, backoffScaled(attempt, c.cfg.RetryBackoff)) {
					break
				}
				continue
			}
			break
		}
		if resp.StatusCode >= 300 {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				raw = []byte(readErr.Error())
			}
			providerErr := &ProviderError{Status: resp.StatusCode, Body: string(raw), Retryable: shouldRetry(resp), RetryAfter: retryAfter(resp.Header)}
			lastErr = providerErr
			// A gate rejection (for example a hosted provider refusing a request
			// with no live session) cannot be fixed by repeating the same call,
			// but it can be fixed by repairing provider state and asking once
			// more. Recovery is attempted before plain retries so a session can
			// be re-admitted instead of burning the retry budget on a request
			// that is certain to fail the same way.
			if c.cfg.RecoverError != nil && recoveries < MaxRecoveries {
				if retry, message := c.cfg.RecoverError(ctx, resp.StatusCode, raw); retry {
					recoveries++
					if message != "" {
						providerErr.RecoverMessage = message
					}
					providerErr.Recovered = true
					attempt-- // a recovery is not a failed attempt
					continue
				} else if message != "" {
					providerErr.RecoverMessage = message
				}
			}
			if providerErr.Retryable && attempt < c.cfg.MaxRetries {
				delay := providerErr.RetryAfter
				if delay <= 0 {
					delay = retryDelay(providerErr, attempt, c.cfg.RetryBackoff)
				}
				// Surface the wait: a gateway backoff runs to about a minute and
				// silence that long is indistinguishable from a hang.
				if c.cfg.OnRetry != nil {
					c.cfg.OnRetry(providerErr, attempt+1, delay)
				}
				if !sleepContext(ctx, delay) {
					break
				}
				continue
			}
			break
		}
		err = c.readStreamWithCompatibility(resp.Body, onDelta, compatibility)
		_ = resp.Body.Close()
		if streamErr, ok := err.(*StreamError); ok && streamErr.Retryable && !streamErr.Partial && attempt < c.cfg.MaxRetries {
			delay := retryDelay(streamErr, attempt, c.cfg.RetryBackoff)
			if c.cfg.OnRetry != nil {
				c.cfg.OnRetry(streamErr, attempt+1, delay)
			}
			if !sleepContext(ctx, delay) {
				break
			}
			continue
		}
		return err
	}
	// The attempt count belongs in the message. Returning a terminal ProviderError
	// bare made a request that failed on attempt 1 look identical to one that was
	// retried three times and still failed, which is exactly the ambiguity that
	// made a retried 502 look unretried in a session log. attempts counts the
	// initial call plus every retry, so it is the number of provider round trips.
	if providerErr, ok := lastErr.(*ProviderError); ok {
		return fmt.Errorf("after %d attempt(s): %w", attempts, providerErr)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("provider request failed")
	}
	return fmt.Errorf("after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

type accumulatedToolCall struct {
	ID               string
	Name             string
	Arguments        string
	ThoughtSignature string
}

func (c *StreamingClient) readStream(r io.Reader, onDelta func(ProviderResponse)) error {
	return c.readStreamWithCompatibility(r, onDelta, Compatibility{SupportsFinishReason: false})
}

func (c *StreamingClient) readStreamWithCompatibility(r io.Reader, onDelta func(ProviderResponse), compatibility Compatibility) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	acc := map[string]*accumulatedToolCall{}
	var finishReason, responseID, responseModel string
	terminal := false
	emitted := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			terminal = true
			break
		}
		var event struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content         *string         `json:"content"`
					Reasoning       *string         `json:"reasoning_content"`
					ReasoningAlt    *string         `json:"reasoning"`
					ReasoningText   *string         `json:"reasoning_text"`
					ReasoningDetail json.RawMessage `json:"reasoning_details"`
					ToolCalls       []struct {
						Index        *int   `json:"index"`
						ID           string `json:"id"`
						Type         string `json:"type"`
						ExtraContent struct {
							Google struct {
								ThoughtSignature string `json:"thought_signature"`
							} `json:"google"`
						} `json:"extra_content"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *UsageInfo `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return &StreamError{Err: fmt.Errorf("decode provider stream event: %w", err), Retryable: false, Partial: emitted}
		}
		if event.ID != "" {
			responseID = event.ID
		}
		if event.Model != "" {
			responseModel = event.Model
		}
		var delta ProviderResponse
		delta.ResponseID = responseID
		delta.ResponseModel = responseModel
		if len(event.Choices) > 0 {
			choice := event.Choices[0]
			if choice.Delta.Content != nil {
				delta.Content = *choice.Delta.Content
			}
			if len(choice.Delta.ReasoningDetail) > 0 && string(choice.Delta.ReasoningDetail) != "null" {
				delta.ReasoningDetails = choice.Delta.ReasoningDetail
			}
			for _, candidate := range []*string{choice.Delta.Reasoning, choice.Delta.ReasoningAlt, choice.Delta.ReasoningText} {
				if candidate != nil && *candidate != "" {
					delta.Reasoning = *candidate
					break
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
				delta.FinishReason = finishReason
			}
			for _, toolCall := range choice.Delta.ToolCalls {
				key := streamedToolKey(toolCall.Index, toolCall.ID, len(acc))
				if toolCall.Index == nil && toolCall.ID != "" {
					if _, exists := acc[key]; !exists {
						for existingKey, existing := range acc {
							if existing.ID == "" {
								key = existingKey
								break
							}
						}
					}
				}
				existing := acc[key]
				if existing == nil {
					existing = &accumulatedToolCall{}
					acc[key] = existing
				}
				if toolCall.ID != "" {
					existing.ID = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					existing.Name = toolCall.Function.Name
				}
				if toolCall.ExtraContent.Google.ThoughtSignature != "" {
					existing.ThoughtSignature = toolCall.ExtraContent.Google.ThoughtSignature
				}
				existing.Arguments = mergeStreamedArguments(existing.Arguments, toolCall.Function.Arguments)
				index := len(acc) - 1
				if toolCall.Index != nil {
					index = *toolCall.Index
				}
				delta.ToolCalls = append(delta.ToolCalls, StreamedToolCall{
					Index:            index,
					ID:               existing.ID,
					Name:             existing.Name,
					Arguments:        existing.Arguments,
					ThoughtSignature: existing.ThoughtSignature,
				})
			}
		}
		if event.Usage != nil {
			delta.Usage = event.Usage
		}
		if delta.Content != "" || delta.Reasoning != "" || len(delta.ToolCalls) > 0 || delta.Usage != nil || delta.FinishReason != "" {
			emitted = true
		}
		onDelta(delta)
	}
	if err := scanner.Err(); err != nil {
		return &StreamError{Err: fmt.Errorf("read provider stream: %w", err), Retryable: true, Partial: emitted}
	}
	if !terminal {
		if finishReason == "" {
			return &StreamError{Err: fmt.Errorf("provider stream ended without a terminal event"), Retryable: true, Partial: emitted}
		}
		terminal = true
	}
	if compatibility.SupportsFinishReason && finishReason == "" {
		return &StreamError{Err: fmt.Errorf("provider stream ended without finish_reason"), Retryable: true, Partial: emitted}
	}
	onDelta(ProviderResponse{Done: true, FinishReason: finishReason, ResponseID: responseID, ResponseModel: responseModel})
	return nil
}

func streamedToolKey(index *int, id string, fallback int) string {
	if index != nil {
		return "index:" + strconv.Itoa(*index)
	}
	if id != "" {
		return "id:" + id
	}
	return "fallback:" + strconv.Itoa(fallback)
}

func mergeStreamedArguments(current, incoming string) string {
	if incoming == "" {
		return current
	}
	if current == "" || strings.HasPrefix(incoming, current) {
		return incoming
	}
	return current + incoming
}

// UsesGeminiThoughtSignatures reports whether a provider needs Gemini's
// thought_signature round-trip.
//
// Gemini 3 rejects a conversation whose functionCall parts carry no
// thought_signature with a 400, so the signature the model returned must be
// replayed on the next request. It is also the only way Ceraa's reasoning
// display works for these models, since the signature rides along with the
// thought block.
//
// This is decided by the model, never by which profile slot the provider sits
// in. Keying it on the profile id previously disabled it for the default
// provider, which is exactly the path an env-configured Gemini model takes.
func UsesGeminiThoughtSignatures(model, baseURL string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(name, "gemini") || strings.Contains(name, "gemma") {
		return true
	}
	// A proxied or renamed model still needs it when the host is Gemini's.
	host := strings.ToLower(baseURL)
	return strings.Contains(host, "generativelanguage.googleapis.com")
}

// toolCallNames maps a tool call id to the function name that produced it, from
// both single and batched tool-call items in the same request.
func toolCallNames(items []Item) map[string]string {
	names := make(map[string]string)
	for _, item := range items {
		switch item.Kind {
		case "tool_call":
			if item.CallID != "" && item.Name != "" {
				names[item.CallID] = item.Name
			}
		case "tool_calls":
			for _, call := range item.ToolCalls {
				if call.ID != "" && call.Name != "" {
					names[call.ID] = call.Name
				}
			}
		}
	}
	return names
}

// requestHost returns just the host of a base URL, so a diagnostic can name the
// endpoint without recording the full URL or any credential.
func requestHost(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "unparseable"
	}
	return parsed.Host
}

// diagMu guards diagOut. Diagnostics are written from whichever goroutine the
// request runs on, and the destination is swapped once at startup, so the read
// and the write are paired under one lock rather than trusted to a plain var.
var (
	diagMu  sync.Mutex
	diagOut io.Writer = os.Stderr
)

// SetDiagOutput changes where the live copy of a per-request diagnostic goes. A
// nil writer silences it.
//
// The log file always gets the note; this is only the copy printed for whoever
// is watching. The default of stderr is right when the answer is on stdout and
// wrong for the interactive session, which renders to stderr itself: a note
// written straight to the same stream lands wherever the cursor happens to be,
// inside a spinner frame or across an answer line. The session turns this off
// and leaves the note in the file, where it can be read afterwards.
func SetDiagOutput(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	diagMu.Lock()
	diagOut = w
	diagMu.Unlock()
}

// diagNote prints the live copy of a note that writeDebugNote has already put in
// the file. Best effort by construction: a destination that has gone away must
// never fail a request.
func diagNote(note string) {
	diagMu.Lock()
	w := diagOut
	diagMu.Unlock()
	fmt.Fprint(w, "ceraa: "+note)
}

// writeDebugNote appends a diagnostic line to the data directory's log folder.
// Failures are ignored: a diagnostic must never break a request.
func writeDebugNote(name, line string) {
	dir := filepath.Join(paths.DataDir(), "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprint(f, time.Now().Format(time.RFC3339)+" "+line)
}

// describeEncodedTurn renders the structure of an encoded request: every
// message role in order, and for assistant tool calls the function name plus
// whether a thought_signature rides along. Message content, arguments and tool
// output are deliberately omitted, so the diagnostic can never capture a
// conversation.
func describeEncodedTurn(msgs []map[string]any) string {
	parts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		calls, _ := msg["tool_calls"].([]map[string]any)
		switch {
		case len(calls) == 0:
			parts = append(parts, role)
		default:
			descriptions := make([]string, 0, len(calls))
			for _, call := range calls {
				name := "?"
				if fn, ok := call["function"].(map[string]any); ok {
					if value, ok := fn["name"].(string); ok {
						name = value
					}
				}
				signature := "none"
				if extra, ok := call["extra_content"].(map[string]any); ok {
					if google, ok := extra["google"].(map[string]any); ok {
						if value, ok := google["thought_signature"].(string); ok && value != "" {
							if value == skipThoughtSignatureValidator {
								signature = "sentinel"
							} else {
								signature = fmt.Sprintf("sig%d", len(value))
							}
						}
					}
				}
				descriptions = append(descriptions, name+":"+signature)
			}
			parts = append(parts, role+"/tool_calls("+strings.Join(descriptions, ",")+")")
		}
	}
	return strings.Join(parts, "|")
}

// geminiSignatureStats summarises how signatures were applied to a request, for
// the diagnostic line above.
func geminiSignatureStats(items []Item) struct{ signed, unsigned int } {
	var stats struct{ signed, unsigned int }
	for _, item := range items {
		switch item.Kind {
		case "tool_call":
			if strings.TrimSpace(item.ThoughtSignature) != "" {
				stats.signed++
			} else {
				stats.unsigned++
			}
		case "tool_calls":
			for _, call := range item.ToolCalls {
				if strings.TrimSpace(call.ThoughtSignature) != "" {
					stats.signed++
				} else {
					stats.unsigned++
				}
			}
		}
	}
	return stats
}

// canonicalToolResults picks the single result to send for each tool call id.
// A non-empty result always wins over an empty one, because an empty result is
// the placeholder the encoder would otherwise substitute and carries no
// information; otherwise the first recorded result is kept.
func canonicalToolResults(items []Item) map[string]string {
	chosen := map[string]string{}
	for _, item := range items {
		if item.Kind != "text" || item.Role != RoleTool || item.CallID == "" {
			continue
		}
		existing, seen := chosen[item.CallID]
		if !seen {
			chosen[item.CallID] = item.Output
			continue
		}
		if strings.TrimSpace(existing) == "" && strings.TrimSpace(item.Output) != "" {
			chosen[item.CallID] = item.Output
		}
	}
	return chosen
}

// fallbackToolResultName produces a stable, non-empty function name for a tool
// result whose original name cannot be recovered. The call id is preferred
// because it keeps distinct results distinguishable; the charset is reduced to
// what providers accept in a function name.
func fallbackToolResultName(callID string) string {
	sanitized := make([]rune, 0, len(callID))
	for _, r := range callID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sanitized = append(sanitized, r)
		}
		if len(sanitized) == 40 {
			break
		}
	}
	if len(sanitized) == 0 {
		return "tool"
	}
	return "tool_" + string(sanitized)
}

// isTransient returns true for status codes that should be retried.
func chatCompletionsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

func retryAfter(header http.Header) time.Duration {
	for _, name := range []string{"Retry-After-Ms", "Retry-After"} {
		value := strings.TrimSpace(header.Get(name))
		if value == "" {
			continue
		}
		if milliseconds, err := strconv.ParseFloat(value, 64); err == nil && strings.EqualFold(name, "Retry-After-Ms") {
			if milliseconds > 0 {
				return time.Duration(milliseconds * float64(time.Millisecond))
			}
		}
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		if when, err := http.ParseTime(value); err == nil {
			if delay := time.Until(when); delay > 0 {
				return delay
			}
		}
	}
	return 0
}

func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	if providerErr, ok := err.(*ProviderError); ok {
		body := strings.ToLower(providerErr.Body)
		return strings.Contains(body, "context_length") || strings.Contains(body, "context window") || strings.Contains(body, "maximum context") || strings.Contains(body, "too many tokens")
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context_length") || strings.Contains(message, "context window") || strings.Contains(message, "maximum context")
}

func shouldRetry(response *http.Response) bool {
	if value := strings.TrimSpace(response.Header.Get("X-Should-Retry")); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return isTransient(response.StatusCode)
}

// isTransient returns true for status codes that should be retried.
func isTransient(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// backoff returns exponential backoff duration for the given attempt (0-based).
// gatewayBackoff is deliberately much longer than the default schedule.
//
// A 429 means the caller is going too fast and a short wait is right. A 502 or 503
// means the provider's own upstream is unreachable, and repeating after a second
// just finds it still down. The default spans about twelve seconds in total, which
// rides out a blip but not an outage: a live run lost four attempts inside that
// window to a gateway that had been failing intermittently. This spans about a
// minute, which covers the case that actually happens.
var gatewayBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}

// IsGatewayError reports whether an upstream gateway fault is what failed.
//
// Deliberately narrow. A 4xx is the caller's fault and repeating it only wastes the
// budget, and a cancelled context is the user's decision rather than a fault.
func IsGatewayError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.Status {
		case 500, 502, 503, 504:
			return true
		}
		return false
	}
	// A transport fault never reached the provider, so it is an outage rather than
	// a bad request.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// retryDelay returns how long to wait before the given attempt.
func retryDelay(err error, attempt int, scale time.Duration) time.Duration {
	if scale > 0 {
		return scale
	}
	if IsGatewayError(err) {
		if attempt < len(gatewayBackoff) {
			return gatewayBackoff[attempt]
		}
		return gatewayBackoff[len(gatewayBackoff)-1]
	}
	return backoffScaled(attempt, scale)
}

func backoff(attempt int) time.Duration { return backoffScaled(attempt, 0) }

// backoffScaled returns the delay before the given attempt, optionally shortened
// for tests. A zero scale means production timings.
func backoffScaled(attempt int, scale time.Duration) time.Duration {
	if scale > 0 {
		return scale
	}
	durations := []time.Duration{1 * time.Second, 3 * time.Second, 8 * time.Second}
	delay := 8 * time.Second
	if attempt < len(durations) {
		delay = durations[attempt]
	}
	jitter := time.Duration(rand.Int63n(int64(250 * time.Millisecond)))
	return delay + jitter
}

// newAuthorizedRequest builds a request with the provider's credentials applied.
// Shared by the one-shot media path so it cannot drift from the streaming one.
func newAuthorizedRequest(ctx context.Context, url string, body []byte, apiKey string, extra map[string]string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		// Gemini authenticates with its own header; the compatibility layer uses
		// a bearer token.
		if strings.Contains(url, "generativelanguage.googleapis.com") {
			request.Header.Set("x-goog-api-key", apiKey)
		} else {
			request.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	for key, value := range extra {
		request.Header.Set(key, value)
	}
	return request, nil
}

// readLimited reads a response body under a cap, so a large error page cannot
// be pulled into memory whole.
func readLimited(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// errorStatusAndBody pulls the status and body out of whatever a transport
// wrapped. Providers answer a rejected parameter in several envelopes, and the
// reasoning retry has to see through all of them: StreamError unwraps to the
// ProviderError a stream was built from, and both are unwrapped here.
func errorStatusAndBody(err error) (int, string) {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Status, providerErr.Body
	}
	return http.StatusOK, err.Error()
}

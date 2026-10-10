package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GeminiSubscription identifies a Gemini Code Assist profile: Google OAuth
// against the Cloud Code endpoint, not an API key against generativelanguage.
const GeminiSubscription = "gemini"

// CodeAssistBaseURL is the Cloud Code daily endpoint. The sandbox host is the
// single retry target; the two behave identically except when one is down, so
// rotation is a failover, never a quota strategy.
const CodeAssistBaseURL = "https://daily-cloudcode-pa.googleapis.com"

// CodeAssistSandboxURL is the failover endpoint.
const CodeAssistSandboxURL = "https://daily-cloudcode-pa.sandbox.googleapis.com"

// CodeAssistStreamPath is the streaming method. No model segment: the model
// travels inside the envelope, and a /models/{id} path here is a 404.
const CodeAssistStreamPath = "/v1internal:streamGenerateContent?alt=sse"

// CodeAssistProject is the fallback project for accounts whose loadCodeAssist
// probe returns empty, which is the entire free tier. Sending it is what every
// capture shows; per-account detection is a follow-up, and pinning a stale
// detection would misroute paid accounts, so an explicit value is never
// persisted from a probe.
const CodeAssistProject = "aicode-consumers"

// codeAssistIdentityHeaders are byte-exact. Any character off and the server
// reads the client as foreign. Never add x-goog-api-key or x-goog-api-client:
// the reference client does not send them.
var codeAssistIdentityHeaders = map[string]string{
	"User-Agent":         "antigravity/4.3.0 (cmdc-pak)",
	"x-client-name":      "antigravity",
	"x-client-version":   "4.3.0",
	"x-machine-id":       "cmdc-pak",
	"x-vscode-sessionid": "proxy",
}

// GeminiCodeAssistModels is the served set: one model, chosen by tier suffix
// rather than by name.
var GeminiCodeAssistModels = []string{
	"Gemini 3.8 Flash",
}

// DefaultGeminiCodeAssistModel is the only served model.
const DefaultGeminiCodeAssistModel = "Gemini 3.8 Flash"

// geminiCodeAssistUpstream is the directory id the envelope routes on.
const geminiCodeAssistUpstream = "gemini-3.8-flash"

// geminiCodeAssistTiers are the selectable thinking tiers.
var geminiCodeAssistTiers = []string{"low", "medium", "high", "tiered"}

// geminiCodeAssistBudgets are the output budgets per tier. Tiered carries no
// budget: the server adapts. Thinking cannot be disabled upstream (an
// includeThoughts-only request still thinks and still bills), so there is no
// off tier, only a minimum.
var geminiCodeAssistBudgets = map[string]int{
	"low":    1000,
	"medium": 4000,
	"high":   10000,
}

// CodeAssistTier maps a Ceraa reasoning level onto an upstream tier. Unknown
// levels land on medium, the reference default, rather than failing the turn.
func CodeAssistTier(reason ReasonLevel) string {
	switch reason {
	case ReasonLow, ReasonMinimal, ReasonOff:
		return "low"
	case ReasonMedium:
		return "medium"
	case ReasonHigh, ReasonXHigh, ReasonMax:
		return "high"
	default:
		return "medium"
	}
}

// codeAssistEnvelope is the v1internal body: the standard generateContent
// shape nested under request, with routing fields around it. A bare
// generateContent body here is a 400: the method serves the envelope, not the
// public shape.
type codeAssistEnvelope struct {
	Model     string            `json:"model"`
	Project   string            `json:"project"`
	Request   codeAssistRequest `json:"request"`
	RequestID string            `json:"requestId"`
	UserAgent string            `json:"userAgent"`
}

type codeAssistRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
	SessionID         string                  `json:"sessionId"`
}

// codeAssistSessionID derives a stable conversation id: same conversation
// stable (keeping upstream cache hits), different conversations isolated.
// The Ceraa session id when the run carries one, else a hash of the opening
// user text, which is what the reference derives from.
func codeAssistSessionID(sessionID, firstUserText string) string {
	if strings.TrimSpace(sessionID) != "" {
		return "ceraa-" + strings.TrimSpace(sessionID)
	}
	sum := sha256.Sum256([]byte(firstUserText))
	return fmt.Sprintf("ceraa-%x", sum[:8])
}

func codeAssistFirstUserText(req StreamRequest) string {
	for _, item := range req.Messages {
		if item.Role == RoleUser && item.Kind == "text" && strings.TrimSpace(item.Text) != "" {
			return item.Text
		}
	}
	return ""
}

func codeAssistUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildCodeAssistBody renders a turn into the envelope. Contents, tools and
// thought signatures ride the shared native encoders; only the generation
// config and the wrapper are Code Assist specific, because the v1internal
// thinking shape (budget tiers) is not the Interactions shape (levels).
func buildCodeAssistBody(req StreamRequest, tier string) (map[string]any, error) {
	inner := buildGeminiRequest(req, "")
	budget, ok := geminiCodeAssistBudgets[tier]
	if !ok {
		return nil, fmt.Errorf("codeassist: unknown tier %q", tier)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	inner.GenerationConfig = &geminiGenerationConfig{
		MaxOutputTokens: maxTokens,
		ThinkingConfig: &geminiThinkingConfig{
			IncludeThoughts: true,
			ThinkingBudget:  &budget,
		},
	}
	envelope := map[string]any{
		"model":   geminiCodeAssistUpstream + "-" + tier,
		"project": CodeAssistProject,
		"request": map[string]any{
			"contents":         inner.Contents,
			"generationConfig": inner.GenerationConfig,
			"sessionId":        codeAssistSessionID(req.SessionID, codeAssistFirstUserText(req)),
		},
		"requestId": codeAssistUUID(),
		"userAgent": "antigravity",
	}
	if inner.SystemInstruction != nil {
		envelope["request"].(map[string]any)["systemInstruction"] = inner.SystemInstruction
	}
	if len(inner.Tools) > 0 {
		envelope["request"].(map[string]any)["tools"] = inner.Tools
	}
	if inner.ToolConfig != nil {
		envelope["request"].(map[string]any)["toolConfig"] = inner.ToolConfig
	}
	return envelope, nil
}

// streamCodeAssist is the Gemini Code Assist transport: the v1internal
// streaming method with an OAuth bearer, the client identity headers, and the
// envelope body. The response chunks are standard generateContent SSE, so they
// ride the shared native parser.
func (c *StreamingClient) streamCodeAssist(ctx context.Context, req StreamRequest, onDelta func(ProviderResponse)) error {
	if c.cfg.CodeAssistTokens == nil {
		return fmt.Errorf("codeassist: no credential source configured; sign in from the provider menu first")
	}
	bases := []string{strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/")}
	if bases[0] == "" {
		bases[0] = CodeAssistBaseURL
	}
	tier := CodeAssistTier(req.Reason)
	body, err := buildCodeAssistBody(req, tier)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode codeassist request: %w", err)
	}

	refreshed := false
	triedSandbox := strings.Contains(bases[0], "sandbox")
	for {
		token, err := c.cfg.CodeAssistTokens.AccessToken(ctx)
		if err != nil {
			return err
		}
		url := bases[0] + CodeAssistStreamPath
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		hreq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
		for key, value := range codeAssistIdentityHeaders {
			hreq.Header.Set(key, value)
		}
		resp, err := c.doRequest(hreq)
		if err != nil {
			return fmt.Errorf("codeassist: request: %w", err)
		}
		if resp.StatusCode < 300 {
			err := c.readGeminiStream(resp.Body, onDelta)
			_ = resp.Body.Close()
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			raw = []byte(readErr.Error())
		}
		// 401 renews the grant once, then retries. Anything else retries only
		// across endpoints, never twice on the same one.
		if resp.StatusCode == http.StatusUnauthorized && !refreshed {
			if refresher, ok := c.cfg.CodeAssistTokens.(CodexRefresher); ok {
				if _, err := refresher.RefreshToken(ctx); err == nil {
					refreshed = true
					continue
				}
			}
		}
		if !triedSandbox && codeAssistFailover(resp.StatusCode, raw) {
			triedSandbox = true
			bases[0] = CodeAssistSandboxURL
			continue
		}
		return &ProviderError{Status: resp.StatusCode, Body: string(raw), Retryable: shouldRetryStatus(resp.StatusCode)}
	}
}

// codeAssistFailover reports failures worth retrying on the sandbox host:
// auth-shape rejections, missing routes, and quota/permission/project
// refusals. Anything else failed on the merits and fails identically there.
func codeAssistFailover(status int, raw []byte) bool {
	if status == http.StatusForbidden || status == http.StatusNotFound {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	lowered := strings.ToLower(string(raw))
	for _, needle := range []string{"quota", "permission", "unsupported", "project"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

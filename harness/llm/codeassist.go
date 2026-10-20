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
	"net/url"
	"strings"
	"time"
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

// CodeAssistValidationError is Google's second sign-in step. The OAuth grant
// is valid, but the account must be verified before Code Assist will serve a
// request. It is a typed error so every surface can show the verification
// link, rather than dumping the raw 403 JSON that left the user with a
// dead-end "Verify your account to continue" body.
type CodeAssistValidationError struct {
	Message       string // Google's validation_error_message
	URL           string // validation_url, https only
	LinkText      string // validation_url_link_text
	LearnMoreURL  string // validation_learn_more_url, https only
	LearnMoreText string // validation_learn_more_link_text
}

func (e *CodeAssistValidationError) Error() string {
	var b strings.Builder
	b.WriteString("Gemini Code Assist needs account verification")
	if message := strings.TrimSpace(e.Message); message != "" {
		b.WriteString(": ")
		b.WriteString(strings.TrimSuffix(message, "."))
		b.WriteString(".")
	} else {
		b.WriteString(".")
	}
	if e.URL != "" {
		label := strings.TrimSpace(e.LinkText)
		if label == "" {
			label = "Verify your account"
		}
		b.WriteString("\n\n")
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(e.URL)
		b.WriteString("\n\nFinish the check in the browser, then send your message again.")
	}
	if e.LearnMoreURL != "" {
		label := strings.TrimSpace(e.LearnMoreText)
		if label == "" {
			label = "Learn more"
		}
		b.WriteString("\n")
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(e.LearnMoreURL)
	}
	return b.String()
}

// codeAssistValidationFromBody extracts the verification link from a 403.
// The URL is surfaced only when it is https: the body comes from the
// provider, and an http link in a chat is not something to make clickable.
func codeAssistValidationFromBody(raw []byte) *CodeAssistValidationError {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Details []struct {
				Metadata map[string]string `json:"metadata"`
				Links    []struct {
					Description string `json:"description"`
					URL         string `json:"url"`
				} `json:"links"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}
	for _, detail := range envelope.Error.Details {
		link := httpsURL(detail.Metadata["validation_url"])
		if link == "" {
			continue
		}
		message := detail.Metadata["validation_error_message"]
		if message == "" {
			message = envelope.Error.Message
		}
		return &CodeAssistValidationError{
			Message:       message,
			URL:           link,
			LinkText:      detail.Metadata["validation_url_link_text"],
			LearnMoreURL:  httpsURL(detail.Metadata["validation_learn_more_url"]),
			LearnMoreText: detail.Metadata["validation_learn_more_link_text"],
		}
	}
	for _, detail := range envelope.Error.Details {
		for _, link := range detail.Links {
			if !strings.Contains(strings.ToLower(link.Description), "verify") {
				continue
			}
			if target := httpsURL(link.URL); target != "" {
				return &CodeAssistValidationError{
					Message:  envelope.Error.Message,
					URL:      target,
					LinkText: link.Description,
				}
			}
		}
	}
	return nil
}

// httpsURL returns the candidate only when it is a usable https URL, so a
// malformed or non-TLS link never becomes a button in the chat.
func httpsURL(raw string) string {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return ""
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return candidate
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

// codeAssistRequest is a struct, so encoding/json emits fields in DECLARATION
// order.
//
// The order puts the regions that are stable within a conversation before the
// one that is not. sessionId is stable per conversation, systemInstruction and
// tools are stable per install, and contents is the only part that grows. The
// benefit here is UNVERIFIED: this transport mints a fresh random requestId on
// every request and still cached at a 91.8% steady state, so the provider is
// evidently matching parsed structure rather than raw bytes, and JSON key order
// is unlikely to be what decides a hit. See the note on geminiRequest.
//
// It is a struct rather than a map for a concrete reason: encoding/json sorts
// MAP keys alphabetically, which would put "contents" first regardless of what
// the code intends. A map cannot express this ordering at all.
type codeAssistRequest struct {
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	SessionID         string                  `json:"sessionId"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
	Contents          []geminiContent         `json:"contents"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
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
//
// The inner request is a codeAssistRequest struct rather than a map[string]any.
// A map cannot express a deliberate field order at all, because encoding/json
// sorts map keys alphabetically. The envelope itself stays a map: its keys are
// routing metadata that callers assert on by name, and it is marshalled whole.
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
	return map[string]any{
		"model":   geminiCodeAssistUpstream + "-" + tier,
		"project": CodeAssistProject,
		"request": codeAssistRequest{
			SystemInstruction: inner.SystemInstruction,
			Tools:             inner.Tools,
			SessionID:         codeAssistSessionID(req.SessionID, codeAssistFirstUserText(req)),
			ToolConfig:        inner.ToolConfig,
			Contents:          inner.Contents,
			GenerationConfig:  inner.GenerationConfig,
		},
		"requestId": codeAssistUUID(),
		"userAgent": "antigravity",
	}, nil
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
		// Account verification is terminal and account-level: the sandbox
		// endpoint would return the same 403, so surface the link instead of
		// failing over and then dumping the JSON.
		if resp.StatusCode == http.StatusForbidden {
			if validation := codeAssistValidationFromBody(raw); validation != nil {
				return validation
			}
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

// ProbeCodeAssistAccount reports whether the signed-in account still needs
// Google's verification step. It is the post-exchange half of the login: the
// OAuth grant can be valid while Code Assist refuses to serve until the
// account is verified, and claiming success at that point is what confused
// the operator. A nil error with a nil result means the account is usable.
// A probe failure is not fatal to the login; callers log it and keep the
// grant, because the first request will surface the same requirement.
func ProbeCodeAssistAccount(ctx context.Context, tokens CodexTokenSource) (*CodeAssistValidationError, error) {
	return probeCodeAssistAccount(ctx, CodeAssistBaseURL, tokens)
}

func probeCodeAssistAccount(ctx context.Context, baseURL string, tokens CodexTokenSource) (*CodeAssistValidationError, error) {
	if tokens == nil {
		return nil, fmt.Errorf("codeassist: no credential source configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, err := tokens.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideType":    "IDE_UNSPECIFIED",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codeassist: encode account check: %w", err)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/v1internal:loadCodeAssist"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("codeassist: account check request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	for key, value := range codeAssistIdentityHeaders {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codeassist: account check: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codeassist: account check response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if validation := codeAssistValidationFromBody(raw); validation != nil {
			return validation, nil
		}
		return nil, fmt.Errorf("codeassist: account check answered HTTP %d", resp.StatusCode)
	}
	var account struct {
		CurrentTier *struct {
			ID string `json:"id"`
		} `json:"currentTier"`
		IneligibleTiers []struct {
			ReasonCode                  string `json:"reasonCode"`
			ReasonMessage               string `json:"reasonMessage"`
			ValidationURL               string `json:"validationUrl"`
			ValidationErrorMessage      string `json:"validationErrorMessage"`
			ValidationURLLinkText       string `json:"validationUrlLinkText"`
			ValidationLearnMoreURL      string `json:"validationLearnMoreUrl"`
			ValidationLearnMoreLinkText string `json:"validationLearnMoreLinkText"`
		} `json:"ineligibleTiers"`
	}
	if err := json.Unmarshal(raw, &account); err != nil {
		return nil, fmt.Errorf("codeassist: account check response: %w", err)
	}
	if account.CurrentTier != nil {
		return nil, nil
	}
	for _, tier := range account.IneligibleTiers {
		if tier.ReasonCode != "VALIDATION_REQUIRED" {
			continue
		}
		link := httpsURL(tier.ValidationURL)
		if link == "" {
			continue
		}
		message := tier.ValidationErrorMessage
		if message == "" {
			message = tier.ReasonMessage
		}
		return &CodeAssistValidationError{
			Message:       message,
			URL:           link,
			LinkText:      tier.ValidationURLLinkText,
			LearnMoreURL:  httpsURL(tier.ValidationLearnMoreURL),
			LearnMoreText: tier.ValidationLearnMoreLinkText,
		}, nil
	}
	return nil, nil
}

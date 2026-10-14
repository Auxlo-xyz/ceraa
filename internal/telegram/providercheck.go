package telegram

import (
	"bytes"
	"ceraa/harness/llm"
	"ceraa/internal/opencodeauth"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// verifyProvider issues one minimal, non-streaming chat completion so a
// provider profile is only saved after the base URL, model, and key have
// actually been shown to work.
//
// Without this, onboarding accepted anything and the first real task was the
// first thing that ever contacted the provider, so a wrong key or a model the
// endpoint does not serve surfaced as a bare HTTP error mid-run with no
// indication of which of the three values was wrong.
func verifyProvider(ctx context.Context, baseURL, apiKey, model string) error {
	return verifyProviderWithHeaders(ctx, baseURL, apiKey, model, nil)
}

// verifyProviderWithHeaders is verifyProvider for a gateway that expects a
// header contract on every request.
//
// The check has to send what real traffic will send. Verifying Zen with a bare
// Authorization header would be a different request from the one the transport
// makes, so the check could pass or fail for reasons that have nothing to do with
// whether the provider works once configured.
//
// Headers are applied before Content-Type and Authorization, so neither of the
// ones the check owns can be replaced by a header the caller passed.
func verifyProviderWithHeaders(ctx context.Context, baseURL, apiKey, model string, headers map[string]string) error {
	endpoint, err := providerCompletionsURL(baseURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("no model id given")
	}
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 8,
		"stream":     false,
	}
	// The check has to send what real traffic will send. Zen's gateway reads the
	// header contract and the body shape as one thing, so a bare non-streaming
	// ping under the right headers is refused exactly as the real request would
	// be - and would then be reported as a bad key.
	if _, openCodeContract := headers[opencodeauth.HeaderSession]; openCodeContract {
		llm.EnsureOpenCodeTraffic(payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	message := providerErrorText(body)
	// The status and the server's words are not an instruction. Zen refuses
	// three different ways and each has its own fix, none of which is the key
	// that was just typed, so the caller is told what to do rather than what
	// came back.
	if hint := providerFailureHint(body); hint != "" {
		message += "\n\n" + hint
	}
	return fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, message)
}

// providerCompletionsURL normalizes a configured base URL into a completions
// endpoint, tolerating a trailing slash or a base that already names the path.
func providerCompletionsURL(baseURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case trimmed == "":
		return "", fmt.Errorf("provider URL is empty")
	case !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://"):
		return "", fmt.Errorf("provider URL must start with http:// or https://")
	case strings.HasSuffix(trimmed, "/chat/completions"):
		return trimmed, nil
	}
	return trimmed + "/chat/completions", nil
}

// providerErrorText pulls a human-readable message out of an error body.
// Providers disagree on shape: OpenAI-style uses a string, Gemini returns a
// single-element array of objects, and some gateways return plain text.
func providerErrorText(body []byte) string {
	var single struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(body, &single) == nil {
		if single.Message != "" {
			return single.Message
		}
		if single.Detail != "" {
			return single.Detail
		}
		switch typed := single.Error.(type) {
		case string:
			if typed != "" {
				return typed
			}
		case map[string]any:
			if message, ok := typed["message"].(string); ok && message != "" {
				return message
			}
		}
	}
	// Gemini-style: a single-element array of {error:{message}}.
	var many []map[string]any
	if json.Unmarshal(body, &many) == nil {
		for _, entry := range many {
			if inner, ok := entry["error"].(map[string]any); ok {
				if message, ok := inner["message"].(string); ok && message != "" {
					return message
				}
			}
			if message, ok := entry["message"].(string); ok && message != "" {
				return message
			}
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200]
	}
	if text == "" {
		return "(empty response body)"
	}
	return text
}

// providerFailureHint says what to do about a refusal, or returns "" when the
// error is not one this code recognises.
//
// Sweeping Zen's catalog with a live key on 2026-10-01 found exactly three ways
// it says no, and the default model was on the wrong side of one of them: the
// free tier is refused per model rather than per client, so a request that is
// accepted for one free id is refused for the next, and neither the id nor the
// refusal says so.
//
// Re-measured 2026-10-14: the free tier is now closed to third-party clients
// entirely. All eight ids that answered on 2026-10-01 refuse with FreeTierError,
// as do the newer free ids, on both /zen/v1 and /inference/v1 and on both
// chat/completions and /responses. The User-Agent version floor is gone with
// it: 1.17.0 answers 403 rather than 426. The key is not the problem, a paid
// probe reaches the funds check, so the hint must not send anyone to re-check
// it or to another free id.
func providerFailureHint(body []byte) string {
	text := string(body)
	switch {
	case strings.Contains(text, "FreeTierError"), strings.Contains(text, "free tier can only be used"):
		return "OpenCode's free tier is currently closed to third-party clients: every free model id refuses, so no key fixes this. " +
			"To use Zen from here, add funds at opencode.ai and pick a paid model, or use another provider."
	case strings.Contains(text, "Insufficient account funds"):
		return "The key is good, the model is good, and the account has no balance. " +
			"Add funds at opencode.ai."
	case strings.Contains(text, "OpenCode Go subscription"):
		return "That model belongs to OpenCode Go, which needs a $10/month subscription. " +
			"Zen models under Model do not."
	case strings.Contains(text, "ModelProtocolUnsupported"), strings.Contains(text, "does not support this protocol"):
		return "That model does not speak chat/completions, so no key works for it from here. " +
			"Pick another model under Model. Note the free tier is currently closed to third-party clients, so a paid model with a funded account is what works from here."
	case strings.Contains(text, "Cannot find any route") && strings.Contains(text, "chat/completions"):
		return "Zen has no chat/completions route for that model id. " +
			"Pick another model under Model. Note the free tier is currently closed to third-party clients, so a paid model with a funded account is what works from here."
	case strings.Contains(text, "Model is unavailable"):
		return "Zen does not serve that model id. Pick one from Model rather than typing one."
	case strings.Contains(text, "ModelDeprecated"), strings.Contains(text, "has been deprecated"):
		return "That model id is retired upstream. Pick one from Model rather than typing one."
	case strings.Contains(text, "Invalid API key"):
		return "Zen did not accept that key. Copy it again from opencode.ai."
	}
	return ""
}

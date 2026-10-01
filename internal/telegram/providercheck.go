package telegram

import (
	"bytes"
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
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 8,
		"stream":     false,
	})
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
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
	return fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, providerErrorText(body))
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

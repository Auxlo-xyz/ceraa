package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DescribeMedia asks the configured model what it sees in an attachment.
//
// It is a single self-contained request rather than something added to the
// conversation, for two reasons. The model's description is stored as an ordinary
// tool result, so the attachment itself is sent exactly once and the cached
// prefix stays small; and a failure here is a tool failure, not a dead run.
func (c *StreamingClient) DescribeMedia(ctx context.Context, media []*MediaAttachment, prompt string) (string, error) {
	if len(media) == 0 {
		return "", fmt.Errorf("no attachment to view")
	}
	if c.cfg.GeminiNative {
		return c.describeMediaGemini(ctx, media, prompt)
	}
	// Codex serves neither Gemini's native API nor OpenAI chat completions. Routing
	// it to the compatibility path below POSTed to a URL that does not exist with
	// stream:false and max_tokens, both of which the endpoint rejects, so every
	// view_image on Codex failed with a challenge page instead of a description.
	if c.cfg.CodexNative {
		return c.describeMediaCodex(ctx, media, prompt)
	}
	// A Messages provider has to be asked in the Messages shape. It used to fall
	// through to the chat-completions path below, which posts a body the
	// /anthropic host does not accept, so view_image failed on every provider
	// that serves the Messages API rather than reporting the picture.
	if c.cfg.AnthropicNative {
		return c.describeMediaAnthropic(ctx, media, prompt)
	}
	return c.describeMediaOpenAI(ctx, media, prompt)
}

// describeMediaAnthropic asks a Messages provider to look at an image.
//
// Non-streaming on purpose: this is a single short answer to a direct question,
// and the stream reader here would have to re-assemble it for no benefit.
func (c *StreamingClient) describeMediaAnthropic(ctx context.Context, media []*MediaAttachment, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = mediaPrompt
	}
	content := make([]map[string]any, 0, len(media)*2+1)
	for _, item := range media {
		content = append(content, map[string]any{"type": "text", "text": "File: " + item.Describe()})
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": item.MimeType,
				"data":       item.Base64Data(),
			},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})

	body := map[string]any{
		"model":      c.cfg.Model,
		"max_tokens": 1024,
		"messages":   []map[string]any{{"role": "user", "content": content}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode messages media request: %w", err)
	}
	token, err := c.anthropicToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL(c.cfg.BaseURL), bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	if strings.EqualFold(strings.TrimSpace(c.cfg.MessagesAuthStyle), MessagesAuthBearer) {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if strings.EqualFold(strings.TrimSpace(c.cfg.MessagesAuthStyle), MessagesAuthDsh) {
		req.Header.Set("x-dsh-auth-token", token)
	} else {
		req.Header.Set("x-api-key", token)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	for key, value := range c.cfg.ExtraHeaders {
		req.Header.Set(key, value)
	}
	resp, err := c.doRequest(req)
	if err != nil {
		return "", &ProviderError{Status: 0, Body: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &ProviderError{Status: resp.StatusCode, Body: string(raw)}
	}
	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("decode messages media response: %w", err)
	}
	var out []string
	for _, block := range decoded.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			out = append(out, block.Text)
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("the provider returned no text for the image")
	}
	return strings.Join(out, "\n"), nil
}

// mediaPrompt is the instruction used when the caller does not supply one. It
// asks for observation rather than approval, because a model that is asked
// whether something looks right will tend to agree.
const mediaPrompt = "Describe what you see in this image. Be concrete and factual: " +
	"layout, visible text, colours, spacing, alignment, and anything that looks broken, " +
	"cut off, overlapping, or misaligned. Do not say whether it is good."

func (c *StreamingClient) describeMediaGemini(ctx context.Context, media []*MediaAttachment, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = mediaPrompt
	}
	parts := []geminiPart{}
	// A short note about the file, so the model's answer refers to something the
	// user can identify.
	for _, item := range media {
		parts = append(parts, geminiPart{Text: "File: " + item.Describe()})
		parts = append(parts, geminiPart{
			InlineData: &geminiInlineData{MimeType: item.MimeType, Data: item.Base64Data()},
		})
	}
	parts = append(parts, geminiPart{Text: prompt})

	body := geminiRequest{
		Contents: []geminiContent{{Role: "user", Parts: parts}},
		GenerationConfig: &geminiGenerationConfig{
			MaxOutputTokens: 1024,
		},
	}
	return c.streamGeminiOnce(ctx, body)
}

// describeMediaCodex asks the Codex endpoint about an attachment.
//
// The shape mirrors the streaming transport: Responses API `input`, store false,
// stream true, and no max_output_tokens, because the endpoint rejects that field
// outright. It streams rather than asking for a single response, since stream
// must be true and the field is not optional there.
func (c *StreamingClient) describeMediaCodex(ctx context.Context, media []*MediaAttachment, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = mediaPrompt
	}
	content := []map[string]any{}
	for _, item := range media {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": "File: " + item.Describe(),
		})
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": "data:" + item.MimeType + ";base64," + item.Base64Data(),
		})
	}
	content = append(content, map[string]any{"type": "input_text", "text": prompt})

	body := map[string]any{
		"model": c.cfg.Model,
		"input": []map[string]any{{
			"role":    "user",
			"content": content,
		}},
		"store":  false,
		"stream": true,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode codex media request: %w", err)
	}
	token, err := c.codexToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResponsesURL(c.cfg.BaseURL), bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.doRequest(req)
	if err != nil {
		return "", &ProviderError{Status: 0, Body: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &ProviderError{Status: resp.StatusCode, Body: codexErrorMessage(raw)}
	}
	if readErr != nil {
		return "", readErr
	}
	var answer strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		if event.Type == "response.output_text.delta" {
			answer.WriteString(event.Delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(answer.String())
	if trimmed == "" {
		return "", fmt.Errorf("the model returned no description of the attachment")
	}
	return trimmed, nil
}

// describeMediaOpenAI uses the compatibility layer's content-part shape, which
// is how a non-Gemini provider receives an image.
func (c *StreamingClient) describeMediaOpenAI(ctx context.Context, media []*MediaAttachment, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = mediaPrompt
	}
	content := []map[string]any{}
	for _, item := range media {
		content = append(content, map[string]any{
			"type": "text",
			"text": "File: " + item.Describe(),
		})
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + item.MimeType + ";base64," + item.Base64Data()},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})

	body := map[string]any{
		"model":    c.cfg.Model,
		"messages": []map[string]any{{"role": "user", "content": content}},
		"stream":   false,
	}
	if c.cfg.MaxRetries > 0 {
		body["max_tokens"] = 1024
	}
	var answer strings.Builder
	if err := c.postOnce(ctx, body, &answer); err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(answer.String())
	if trimmed == "" {
		return "", fmt.Errorf("the model returned no description of the attachment")
	}
	return trimmed, nil
}

// streamGeminiOnce performs a single non-streaming native Gemini request.
func (c *StreamingClient) streamGeminiOnce(ctx context.Context, body geminiRequest) (string, error) {
	url := strings.Replace(geminiStreamURL(c.cfg.BaseURL, c.cfg.Model), ":streamGenerateContent?alt=sse", ":generateContent", 1)
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	request, err := newAuthorizedRequest(ctx, url, encoded, c.cfg.APIKey, c.cfg.ExtraHeaders)
	if err != nil {
		return "", err
	}
	response, err := c.doRequest(request)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	raw, readErr := readLimited(response.Body)
	if readErr != nil {
		return "", readErr
	}
	if response.StatusCode >= 300 {
		return "", &ProviderError{Status: response.StatusCode, Body: raw}
	}
	return geminiTextFromResponse(raw)
}

// geminiTextFromResponse pulls the answer out of a generateContent response,
// skipping thought parts so the model reports what it saw rather than what it
// considered.
func geminiTextFromResponse(raw string) (string, error) {
	var event struct {
		Candidates []struct {
			Content struct {
				Parts []geminiPart `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return "", fmt.Errorf("decode gemini response: %w", err)
	}
	if len(event.Candidates) == 0 {
		return "", fmt.Errorf("the model returned no candidates")
	}
	var answer strings.Builder
	for _, part := range event.Candidates[0].Content.Parts {
		if part.Thought || part.Text == "" {
			continue
		}
		answer.WriteString(part.Text)
	}
	trimmed := strings.TrimSpace(answer.String())
	if trimmed == "" {
		return "", fmt.Errorf("the model returned no description of the attachment")
	}
	return trimmed, nil
}

// postOnce performs a single non-streaming compatibility request and collects
// the text. It exists so the media path does not have to reproduce the streaming
// state machine for an answer that is a paragraph.
func (c *StreamingClient) postOnce(ctx context.Context, body map[string]any, answer *strings.Builder) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	url := chatCompletionsURL(c.cfg.BaseURL)
	request, err := newAuthorizedRequest(ctx, url, encoded, c.cfg.APIKey, c.cfg.ExtraHeaders)
	if err != nil {
		return err
	}
	response, err := c.doRequest(request)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer response.Body.Close()
	raw, readErr := readLimited(response.Body)
	if readErr != nil {
		return readErr
	}
	if response.StatusCode >= 300 {
		return &ProviderError{Status: response.StatusCode, Body: raw}
	}
	var event struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(event.Choices) == 0 {
		return fmt.Errorf("the model returned no choices")
	}
	answer.WriteString(event.Choices[0].Message.Content)
	return nil
}

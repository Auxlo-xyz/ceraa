package llm

import (
	"context"
	"encoding/json"
	"fmt"
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
	return c.describeMediaOpenAI(ctx, media, prompt)
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
	response, err := c.http.Do(request)
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
	response, err := c.http.Do(request)
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

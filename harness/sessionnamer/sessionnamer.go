package sessionnamer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"sooraa/harness/llm"
	"sooraa/harness/redact"
	"sooraa/harness/sessionstore"
)

const (
	DefaultAPIURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	DefaultModel  = "gemini-3.5-flash-lite"
)

type Config struct {
	APIURL string
	APIKey string
	Model  string
}

func DefaultConfig() Config {
	apiKey := os.Getenv("SOORAA_TITLE_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	apiURL := os.Getenv("SOORAA_TITLE_API_URL")
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	model := os.Getenv("SOORAA_TITLE_MODEL")
	if model == "" {
		model = DefaultModel
	}
	return Config{APIURL: apiURL, APIKey: apiKey, Model: model}
}

func Generate(ctx context.Context, cfg Config, sess *sessionstore.Session) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("session is nil")
	}
	if cfg.APIKey == "" {
		return "", fmt.Errorf("Gemini API key is not configured")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	userText, assistantText := conversationExcerpt(sess)
	if userText == "" {
		return "", fmt.Errorf("session has no user message")
	}
	userPrompt := "User request:\n" + userText
	if assistantText != "" {
		userPrompt += "\n\nAssistant context:\n" + assistantText
	}
	provider := llm.FromProvider(cfg.APIURL, cfg.APIKey, cfg.Model)
	provider.Timeout = 20 * time.Second
	var title strings.Builder
	err := llm.New(provider).Stream(ctx, llm.StreamRequest{
		Messages: []llm.Item{
			{Kind: "text", Role: llm.RoleSystem, Text: "Create a concise, descriptive title for this coding session. Use 2-6 words, no quotation marks, no punctuation at the end, and return only the title."},
			{Kind: "text", Role: llm.RoleUser, Text: userPrompt},
		},
		MaxTokens: 24,
	}, func(response llm.ProviderResponse) {
		if response.Content != "" {
			title.WriteString(response.Content)
		}
	})
	if err != nil {
		return "", err
	}
	result := cleanTitle(title.String())
	if result == "" {
		return "", fmt.Errorf("Gemini returned an empty title")
	}
	return result, nil
}

func HasMeaningfulPrompt(sess *sessionstore.Session) bool {
	user, _ := conversationExcerpt(sess)
	user = strings.TrimSpace(user)
	return len([]rune(user)) >= 12 || len(strings.Fields(user)) >= 3
}

func Fallback(sess *sessionstore.Session) string {
	if sess == nil {
		return "Untitled session"
	}
	return sess.DisplayTitle()
}

func conversationExcerpt(sess *sessionstore.Session) (string, string) {
	var user, assistant strings.Builder
	for _, message := range sess.Messages {
		if user.Len() == 0 && message.Role == llm.RoleUser && strings.TrimSpace(message.Content) != "" {
			user.WriteString(redact.SecretsInText(message.Content))
		}
		if assistant.Len() == 0 && message.Role == llm.RoleAssistant && strings.TrimSpace(message.Content) != "" {
			assistant.WriteString(redact.SecretsInText(message.Content))
		}
		if user.Len() > 0 && assistant.Len() > 0 {
			break
		}
	}
	return truncate(strings.TrimSpace(user.String()), 1200), truncate(strings.TrimSpace(assistant.String()), 700)
}

func cleanTitle(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`\"' ")
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimRight(value, ".,:;")
	return truncate(value, 56)
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ceraa/harness/tool"
)

// telegramMediaSender sends a file to one chat using the bot token.
//
// The token and the chat id live here, on the bridge side, and are never passed
// through a tool argument. The model names a file; where it goes and who
// authenticates it are settled by configuration the model cannot read.
type telegramMediaSender struct {
	token  string
	chatID int64
	client *http.Client
}

func newTelegramMediaSender(token string, chatID int64) tool.MediaSender {
	return &telegramMediaSender{
		token:  token,
		chatID: chatID,
		client: &http.Client{Timeout: 10 * time.Minute},
	}
}

// mediaUploadField is the multipart field each Telegram method expects.
var mediaUploadField = map[string]string{
	"sendPhoto":     "photo",
	"sendVideo":     "video",
	"sendAudio":     "audio",
	"sendAnimation": "animation",
	"sendDocument":  "document",
}

func (s *telegramMediaSender) SendMedia(ctx context.Context, path, caption string, kind tool.MediaKind) (string, error) {
	if s.token == "" || s.chatID == 0 {
		return "", fmt.Errorf("no Telegram destination is configured")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()

	method := tool.MediaEndpointFor(path, kind)
	field, ok := mediaUploadField[method]
	if !ok {
		return "", fmt.Errorf("unsupported media method %q", method)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	// writeField reports an error the first time only; a multipart failure
	// surfaces when the request is sent, so later calls can be ignored.
	writeField := func(name, value string) {
		_ = writer.WriteField(name, value)
	}
	writeField("chat_id", fmt.Sprintf("%d", s.chatID))
	if caption != "" {
		writeField("caption", truncateForTelegram(caption))
	}
	part, err := writer.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return "", fmt.Errorf("prepare upload: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("finish upload: %w", err)
	}

	url := api(method, s.token)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())

	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("upload to Telegram: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if response.StatusCode >= 300 {
		return "", fmt.Errorf("Telegram rejected the upload (%d): %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &result)
	if !result.OK {
		return "", fmt.Errorf("Telegram rejected the upload: %s", result.Description)
	}
	return fmt.Sprintf("sent %s to the user", filepath.Base(path)), nil
}

// truncateForTelegram keeps a caption inside the bot API limit rather than
// having the whole upload rejected for a caption that is slightly too long.
func truncateForTelegram(caption string) string {
	const limit = 1024
	runes := []rune(caption)
	if len(runes) <= limit {
		return caption
	}
	return string(runes[:limit-1]) + "…"
}

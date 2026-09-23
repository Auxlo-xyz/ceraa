package tool

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"soraa-agent/harness/operation"
)

const (
	maxResponseBytesFetch = 5 * 1024 * 1024
	defaultTimeoutSecs    = 30
	maxTimeoutSecs        = 120
)

// WebFetchTool fetches content from HTTP/HTTPS URLs and returns text or markdown.
type WebFetchTool struct{}

func (w *WebFetchTool) Name() string        { return "webfetch" }
func (w *WebFetchTool) Description() string { return "Fetch content from an HTTP or HTTPS URL and return it as text or markdown. Markdown is the default." }

func (w *WebFetchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "description": "The HTTP or HTTPS URL to fetch content from"},
			"format":  map[string]any{"type": "string", "enum": []string{"text", "markdown", "html"}, "description": "The format to return the content in. Defaults to markdown."},
			"timeout": map[string]any{"type": "integer", "description": fmt.Sprintf("Optional timeout in seconds (maximum: %d)", maxTimeoutSecs)},
		},
		"required": []string{"url"},
	}
}

func (w *WebFetchTool) Translate(args map[string]any) (operation.Spec, error) {
	url, ok := args["url"].(string)
	if !ok || url == "" {
		return operation.Spec{}, fmt.Errorf("url is required")
	}
	format := "markdown"
	if f, ok := args["format"].(string); ok && f != "" {
		format = f
	}
	timeout := defaultTimeoutSecs
	if t, ok := args["timeout"].(float64); ok && t > 0 && t <= maxTimeoutSecs {
		timeout = int(t)
	}
	return operation.Spec{Type: "webfetch", Args: map[string]any{
		"url":     url,
		"format":  format,
		"timeout": timeout,
	}}, nil
}

func (w *WebFetchTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// Run executes the fetch.
func (w *WebFetchTool) Run(spec operation.Spec) (string, error) {
	url, _ := spec.Args["url"].(string)
	format, _ := spec.Args["format"].(string)
	timeout, _ := spec.Args["timeout"].(int)

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("URL must use http:// or https://")
	}

	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	// First attempt: browser user agent
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	switch format {
	case "markdown":
		req.Header.Set("Accept", "text/markdown;q=1.0, text/x-markdown;q=0.9, text/plain;q=0.8, text/html;q=0.7, */*;q=0.1")
	case "text":
		req.Header.Set("Accept", "text/plain;q=1.0, text/markdown;q=0.9, text/html;q=0.8, */*;q=0.1")
	case "html":
		req.Header.Set("Accept", "text/html;q=1.0, application/xhtml+xml;q=0.9, text/plain;q=0.8, */*;q=0.1")
	default:
		req.Header.Set("Accept", "text/markdown;q=1.0, */*;q=0.1")
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Cloudflare challenge fallback: retry with soraa-agent user agent
	if resp.StatusCode == 403 && resp.Header.Get("cf-mitigated") == "challenge" {
		req.Header.Set("User-Agent", "soraa-agent/0.1.0 (+go)")
		resp, err = client.Do(req)
		if err != nil {
			return "", fmt.Errorf("fallback request failed: %w", err)
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytesFetch))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	mime := getMime(contentType)

	if strings.HasPrefix(mime, "image/") && mime != "image/svg+xml" {
		return "", fmt.Errorf("unsupported image content type: %s", mime)
	}
	if !isTextualMime(mime) {
		return "", fmt.Errorf("unsupported content type: %s", mime)
	}

	content := string(body)
	return convert(content, contentType, format)
}

func getMime(ct string) string {
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = ct[:idx]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func isTextualMime(mime string) bool {
	if mime == "" {
		return true
	}
	return strings.HasPrefix(mime, "text/") ||
		mime == "application/json" ||
		strings.HasSuffix(mime, "+json") ||
		mime == "application/xml" ||
		strings.HasSuffix(mime, "+xml") ||
		mime == "application/javascript" ||
		mime == "application/x-javascript"
}

func convert(content, contentType, format string) (string, error) {
	mime := getMime(contentType)
	if !strings.Contains(mime, "text/html") {
		return content, nil
	}
	switch format {
	case "text":
		return extractTextFromHTML(content), nil
	case "markdown":
		return extractTextFromHTML(content), nil
	default:
		return content, nil
	}
}

func extractTextFromHTML(html string) string {
	var sb strings.Builder
	inTag := false

	for _, r := range html {
		if inTag {
			if r == '>' {
				inTag = false
			}
			continue
		}
		if r == '<' {
			inTag = true
			continue
		}
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		sb.WriteRune(r)
	}
	result := sb.String()
	result = strings.Join(strings.Fields(result), " ")
	return result
}

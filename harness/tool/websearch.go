package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"soraa-agent/harness/operation"
)

const (
	exaURL          = "https://mcp.exa.ai/mcp"
	parallelURL     = "https://search.parallel.ai/mcp"
	maxNumResults   = 20
	maxContextChars = 50000

	searchTimeoutSecs = 25
)

// WebSearchTool searches the web via Exa or Parallel backends.
type WebSearchTool struct {
	apiKey    string
	provider  string // "exa" or "parallel", empty = auto
}

func (w *WebSearchTool) Name() string        { return "websearch" }
func (w *WebSearchTool) Description() string { return "Search the web using Exa or Parallel. Use this for current information beyond knowledge cutoff." }

func (w *WebSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":              map[string]any{"type": "string", "description": "Web search query"},
			"numResults":         map[string]any{"type": "integer", "description": fmt.Sprintf("Number of results (default: 8, max: %d)", maxNumResults)},
			"livecrawl":          map[string]any{"type": "string", "description": "Live crawl mode: 'fallback' or 'preferred'"},
			"type":               map[string]any{"type": "string", "description": "Search type: 'auto', 'fast', or 'deep'"},
			"contextMaxCharacters": map[string]any{"type": "integer", "description": fmt.Sprintf("Max context chars (default: 10000, max: %d)", maxContextChars)},
		},
		"required": []string{"query"},
	}
}

func (w *WebSearchTool) Translate(args map[string]any) (operation.Spec, error) {
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return operation.Spec{}, fmt.Errorf("query is required")
	}
	numResults := 8
	if n, ok := args["numResults"].(float64); ok {
		numResults = int(n)
	}
	if numResults <= 0 {
		numResults = 8
	}
	if numResults > maxNumResults {
		numResults = maxNumResults
	}

	return operation.Spec{Type: "websearch", Args: map[string]any{
		"query":              query,
		"numResults":         numResults,
		"livecrawl":          args["livecrawl"],
		"type":               args["type"],
		"contextMaxCharacters": args["contextMaxCharacters"],
	}}, nil
}

func (w *WebSearchTool) TranslateResult(op operation.Operation) string {
	return op.Output
}

// Run executes the search.
func (w *WebSearchTool) Run(spec operation.Spec) (string, error) {
	ctx := context.Background()
	query, _ := spec.Args["query"].(string)
	numResults := 8
	if n, ok := spec.Args["numResults"].(float64); ok {
		numResults = int(n)
	}
	provider := w.provider
	if provider == "" {
		provider = "exa" // default to exa, could implement parallel
	}

	if provider == "exa" {
		return w.searchExa(ctx, query, numResults)
	}
	return w.searchParallel(ctx, query, numResults)
}

type exaArgs struct {
	Query              string `json:"query"`
	Type               string `json:"type"`
	NumResults         int    `json:"numResults"`
	Livecrawl          string `json:"livecrawl"`
	ContextMaxChars    *int   `json:"contextMaxCharacters,omitempty"`
}

type mcpRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      int                    `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]any         `json:"params"`
}

type mcpResult struct {
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

func (w *WebSearchTool) searchExa(ctx context.Context, query string, numResults int) (string, error) {
	url := exaURL
	if w.apiKey != "" {
		url = fmt.Sprintf("%s?exaApiKey=%s", exaURL, w.apiKey)
	}

	args := exaArgs{
		Query:      query,
		Type:       "auto",
		NumResults: numResults,
		Livecrawl:  "fallback",
	}

	payload := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      "web_search_exa",
			"arguments": args,
		},
	}

	return callMcp(ctx, url, payload, searchTimeoutSecs, nil)
}

func (w *WebSearchTool) searchParallel(ctx context.Context, query string, numResults int) (string, error) {
	args := map[string]any{
		"objective":     query,
		"search_queries": []string{query},
		"session_id":    ctx.Value(sessionIDKey{}).(string),
	}

	payload := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: map[string]any{
			"name":      "web_search",
			"arguments": args,
		},
	}

	headers := map[string]string{
		"User-Agent": "soraa-agent/0.1.0",
	}
	if w.apiKey != "" {
		headers["Authorization"] = fmt.Sprintf("Bearer %s", w.apiKey)
	}

	return callMcp(ctx, parallelURL, payload, searchTimeoutSecs, headers)
}

type sessionIDKey struct{}

func callMcp(ctx context.Context, url string, payload mcpRequest, timeoutSecs int, headers map[string]string) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", err
	}

	return parseMcpResponse(string(bodyBytes))
}

func parseMcpResponse(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "No search results found. Please try a different query.", nil
	}

	// Try direct JSON first
	if strings.HasPrefix(trimmed, "{") {
		var result mcpResult
		if err := json.Unmarshal([]byte(trimmed), &result); err == nil {
			for _, c := range result.Result.Content {
				if c.Text != "" {
					return c.Text, nil
				}
			}
		}
	}

	// Try SSE lines
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var result mcpResult
		if err := json.Unmarshal([]byte(data), &result); err == nil {
			for _, c := range result.Result.Content {
				if c.Text != "" {
					return c.Text, nil
				}
			}
		}
	}

	return "No search results found. Please try a different query.", nil
}

func NewWebSearchTool(apiKey, provider string) *WebSearchTool {
	return &WebSearchTool{apiKey: apiKey, provider: provider}
}

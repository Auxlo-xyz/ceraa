package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type codexVisionServer struct {
	mu     sync.Mutex
	body   map[string]any
	path   string
	auth   string
	called int
}

func (c *codexVisionServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.body = body
		c.path = r.URL.Path
		c.auth = r.Header.Get("Authorization")
		c.called++
		c.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"a red square\"}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n")
	}
}

// The media path used to be routed to the chat-completions adapter for every
// non-Gemini provider. On Codex that meant a URL that does not exist plus two
// fields the endpoint rejects, so view_image never worked there.
func TestCodexMediaUsesTheResponsesEndpoint(t *testing.T) {
	server := &codexVisionServer{}
	httpServer := httptest.NewServer(server.handler(t))
	defer httpServer.Close()

	client := New(ProviderConfig{
		BaseURL:     httpServer.URL + "/backend-api/codex",
		Model:       DefaultCodexModel,
		CodexNative: true,
		CodexTokens: &fakeCodexTokens{token: "tok"},
	})
	media := &MediaAttachment{OriginalName: "x.png", MimeType: "image/png", Data: []byte{1, 2, 3}}

	answer, err := client.DescribeMedia(context.Background(), []*MediaAttachment{media}, "")
	if err != nil {
		t.Fatalf("DescribeMedia: %v", err)
	}
	if answer != "a red square" {
		t.Errorf("answer = %q", answer)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if server.path != "/backend-api/codex/responses" {
		t.Errorf("path = %q, want the responses endpoint, not /chat/completions", server.path)
	}
	if server.auth != "Bearer tok" {
		t.Errorf("Authorization = %q", server.auth)
	}
	body := server.body
	if body["store"] != false {
		t.Errorf("store = %v, want false", body["store"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true; the endpoint rejects false", body["stream"])
	}
	// max_tokens is rejected by this endpoint, and it belongs to chat completions.
	if _, present := body["max_tokens"]; present {
		t.Error("media request carries max_tokens, which this endpoint rejects")
	}
	if _, present := body["max_output_tokens"]; present {
		t.Error("media request carries max_output_tokens, which this endpoint rejects")
	}
	if _, present := body["messages"]; present {
		t.Error("media request uses chat-completions messages instead of input")
	}
	input, _ := body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", body["input"])
	}
	parts, _ := input[0].(map[string]any)["content"].([]any)
	var sawImage bool
	for _, part := range parts {
		p, _ := part.(map[string]any)
		if p["type"] == "input_image" {
			sawImage = true
			url, _ := p["image_url"].(string)
			if !strings.HasPrefix(url, "data:image/png;base64,") {
				t.Errorf("image_url = %.40q, want a data URL", url)
			}
		}
	}
	if !sawImage {
		t.Error("no input_image part in the request")
	}
}

// A non-Codex provider must keep using the compatibility path it always used.
func TestNonCodexMediaStillUsesChatCompletions(t *testing.T) {
	server := &codexVisionServer{}
	httpServer := httptest.NewServer(server.handler(t))
	defer httpServer.Close()

	client := New(ProviderConfig{
		BaseURL:    httpServer.URL + "/v1",
		Model:      "some/model",
		APIKey:     "k",
		MaxRetries: 1,
	})
	media := &MediaAttachment{OriginalName: "x.png", MimeType: "image/png", Data: []byte{1}}
	// The stub returns an event stream, which the chat path will not parse into an
	// answer; what matters is which URL it chose.
	_, _ = client.DescribeMedia(context.Background(), []*MediaAttachment{media}, "")
	server.mu.Lock()
	defer server.mu.Unlock()
	if !strings.HasSuffix(server.path, "/chat/completions") {
		t.Errorf("path = %q, want the chat-completions endpoint preserved for non-Codex", server.path)
	}
}

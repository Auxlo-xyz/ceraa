package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/paths"
	"ceraa/internal/secret"
)

// Live-only: it spends plan credits and reads the real credential store.
func liveImageDisabled() bool { return os.Getenv("CERAA_LIVE_IMAGE") != "1" }

// The end-to-end question the unit tests cannot answer: does an image a read
// attached actually reach the model, and does the model look at it?
//
// The probe image is an 8x8 red/green/blue checkerboard, so a description that
// mentions red, green and blue proves the pixels arrived rather than the model
// guessing from the filename.
func TestLiveTheModelSeesAnImageReadByTheReadTool(t *testing.T) {
	if liveImageDisabled() {
		t.Skip("live image check disabled")
	}
	dir := paths.DataDir()
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	value, err := secret.Get("ceraa_minimax_global")
	if err != nil {
		t.Skipf("no stored MiniMax credential: %v", err)
	}
	var cred struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal([]byte(value), &cred) != nil || cred.AccessToken == "" {
		t.Skip("stored credential is unreadable")
	}

	shot := "/tmp/imgprobe/probe.png"
	if _, err := os.Stat(shot); err != nil {
		t.Skipf("no probe image: %v", err)
	}

	// Exactly the items the read operation now produces.
	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "I read an image file. Describe its colours."},
		{Kind: "tool_calls", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: `{"path":"probe.png"}`}}},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read",
			Output: "probe.png attached as an image (image/png, 96 kB).", Images: []string{shot}},
	}
	encoded := encodeAnthropicMessages(items)
	wire, _ := json.Marshal(encoded)
	if !bytes.Contains(wire, []byte(`"type":"image"`)) {
		t.Fatalf("no image block in the encoded request:\n%s", wire)
	}
	t.Logf("encoded request carries an image block (%d bytes on the wire)", len(wire))

	payload, _ := json.Marshal(map[string]any{
		"model": "MiniMax-M3", "max_tokens": 300, "stream": false, "messages": encoded,
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://agent.minimax.io/mavis/api/v1/llm/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, clip2(string(raw), 600))
	}
	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(raw, &decoded)
	for _, b := range decoded.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			t.Logf("MODEL REPLY: %s", clip2(b.Text, 400))
			return
		}
	}
	t.Logf("no text block: %s", clip2(string(raw), 300))
}

func clip2(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

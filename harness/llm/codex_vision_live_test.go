package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ceraa/internal/secret"
)

// TestLiveCodexViewImage drives view_image through the real Codex transport.
//
// It exists because the media path for Codex was routed to the chat-completions
// adapter, which POSTed to a URL that does not exist with two parameters the
// endpoint rejects. The failure surfaced as a 16KB HTML challenge page, which
// read like a network fault rather than a wrong request.
func TestLiveCodexViewImage(t *testing.T) {
	if os.Getenv("CERAA_LIVE_CODEX") != "1" {
		t.Skip("set CERAA_LIVE_CODEX=1 to view a real image through Codex")
	}
	dir := os.Getenv("CERAA_HOME")
	if dir == "" {
		t.Fatal("CERAA_HOME must be set; refusing to run")
	}
	if err := secret.Init(filepath.Join(dir, "secret.key"), filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatalf("secret store: %v", err)
	}
	image := os.Getenv("CERAA_LIVE_IMAGE")
	if image == "" {
		t.Skip("set CERAA_LIVE_IMAGE to the file to view")
	}

	media, err := LoadMediaAttachment(image)
	if err != nil {
		t.Fatalf("load attachment: %v", err)
	}
	client := New(ProviderConfig{
		BaseURL:     CodexBaseURL,
		Model:       DefaultCodexModel,
		CodexNative: true,
		CodexTokens: liveCodexTokens{},
		Timeout:     120 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	answer, err := client.DescribeMedia(ctx, []*MediaAttachment{media}, "")
	if err != nil {
		t.Fatalf("DescribeMedia: %v", err)
	}
	if strings.TrimSpace(answer) == "" {
		t.Fatal("empty description")
	}
	t.Logf("described %s in %d chars: %.200s", filepath.Base(image), len(answer), answer)
	t.Log("LIVE CODEX VIEW_IMAGE OK")
}

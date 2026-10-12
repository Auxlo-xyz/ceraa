package llm

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ceraa/internal/paths"
)

// imageBounds decodes far enough to read an image's dimensions.
func imageBounds(raw []byte) image.Rectangle {
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return image.Rectangle{}
	}
	return decoded.Bounds()
}

func writePNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMediaIsDownscaledBeforeSending is the caching and cost guarantee: a large
// screenshot is re-encoded small, so a turn does not carry megabytes of base64
// and the cached prefix does not grow.
func TestMediaIsDownscaledBeforeSending(t *testing.T) {
	dir := t.TempDir()
	big := writePNG(t, dir, "big.png", 3000, 2000)
	attachment, err := LoadMediaAttachment(big)
	if err != nil {
		t.Fatal(err)
	}
	if !attachment.Resized {
		t.Fatal("a 3000x2000 png should have been downscaled")
	}
	if attachment.MimeType != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", attachment.MimeType)
	}
	// The pixel count is what providers price, so that is the invariant, not the
	// byte count: a noisy source can encode a few KB larger once downscaled.
	if !fitsWithin(imageBounds(attachment.Data), MaxMediaEdge) {
		t.Fatalf("downscaled image still exceeds the edge cap")
	}
	if !strings.Contains(attachment.Describe(), "downscaled") {
		t.Fatalf("describe should say it was prepared: %q", attachment.Describe())
	}
	if strings.Contains(attachment.Describe(), big) {
		t.Fatal("describe should not need the full path")
	}
}

// TestSmallImageIsLeftAlone pins that a small PNG is not needlessly re-encoded,
// so text stays lossless where that is cheap.
func TestSmallImageIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	small := writePNG(t, dir, "small.png", 200, 120)
	attachment, err := LoadMediaAttachment(small)
	if err != nil {
		t.Fatal(err)
	}
	if attachment.Resized {
		t.Fatal("a 200x120 png is already small enough")
	}
	if attachment.MimeType != "image/png" {
		t.Fatalf("mime = %q, want the original", attachment.MimeType)
	}
}

// TestUnknownExtensionIsRefused pins that a type is never guessed at, because a
// wrong declared type is rejected by the provider.
func TestUnknownExtensionIsRefused(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "notes.xyz")
	if err := os.WriteFile(odd, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMediaAttachment(odd); err == nil {
		t.Fatal("an unknown extension should be refused")
	}
	if _, _, ok := MimeTypeForFile("a.mp4"); !ok {
		t.Fatal("video should be recognised")
	}
}

// TestOversizedMediaIsRefused pins that a file too big to inline is a clear
// error rather than a request the provider will reject.
func TestOversizedMediaIsRefused(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(video, make([]byte, MaxMediaBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMediaAttachment(video); err == nil {
		t.Fatal("an oversized file should be refused")
	}
}

// TestMediaRefusalIsRemembered pins the behaviour the user asked for: a model
// that cannot see is not asked again, and the reason is kept.
func TestMediaRefusalIsRemembered(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	resetMediaSupportForTest()

	model := "some-text-only-model"
	if supported, _ := ModelSupportsMedia(model); !supported {
		t.Fatal("an unlisted model should be attempted once")
	}
	RecordMediaRefusal(model, "does not support image input")
	supported, reason := ModelSupportsMedia(model)
	if supported {
		t.Fatal("the refusal must be remembered")
	}
	if reason == "" {
		t.Fatal("the reason should be kept")
	}
	// It must survive a reload, which is the point of persisting it.
	resetMediaSupportForTest()
	if supported, _ := ModelSupportsMedia(model); supported {
		t.Fatal("a refusal must survive a restart")
	}
	if ForgetMediaRefusals() == 0 {
		t.Fatal("there was something to forget")
	}
	if supported, _ := ModelSupportsMedia(model); !supported {
		t.Fatal("after forgetting, the model should be tried again")
	}
}

// TestOnlyMediaRefusalsAreRemembered pins that a timeout does not permanently
// cost a model its vision.
func TestOnlyMediaRefusalsAreRemembered(t *testing.T) {
	if IsMediaRefusal("context deadline exceeded") {
		t.Fatal("a timeout is not a media refusal")
	}
	if IsMediaRefusal("429 too many requests") {
		t.Fatal("rate limiting is not a media refusal")
	}
	if !IsMediaRefusal("Invalid MIME type: image/webp is not supported") {
		t.Fatal("a mime rejection is a media refusal")
	}
	if !IsMediaRefusal("This model does not support image input") {
		t.Fatal("an explicit statement is a media refusal")
	}
}

// TestMediaSupportIsKeyedByHost pins that a refusal is remembered per provider,
// since the same model name behind two endpoints can accept different things.
func TestMediaSupportIsKeyedByHost(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	resetMediaSupportForTest()
	SetMediaSupportHostResolver(func(model string) string {
		if model == "shared-name" {
			return "generativelanguage.googleapis.com"
		}
		return "other.example.com"
	})
	RecordMediaRefusal("shared-name", "does not support image input")
	if supported, _ := ModelSupportsMedia("shared-name"); supported {
		t.Fatal("the refusal should apply to the host that refused")
	}
	// A different host for the same name is a different deployment.
	if supported, _ := ModelSupportsMedia("unrelated-name"); !supported {
		t.Fatal("an unrelated model should be unaffected")
	}
}

func TestDataDirIsUnderCeraaHome(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	if !strings.HasPrefix(mediaSupportFile(), paths.DataDir()) {
		t.Fatalf("media support file %q should live under %q", mediaSupportFile(), paths.DataDir())
	}
}

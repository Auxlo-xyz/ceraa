package tool

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ceraa/harness/llm"
	"ceraa/harness/operation"
)

// fakeVision records what a media view sent, so the tool can be tested without a
// model.
type fakeVision struct {
	mu          sync.Mutex
	Model       string
	description string
	fail        error
	sentBytes   []int
	calls       int
}

func (f *fakeVision) ModelName() string { return f.Model }
func (f *fakeVision) MediaHost() string { return "" }

func (f *fakeVision) DescribeMedia(ctx context.Context, media []*llm.MediaAttachment, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	for _, item := range media {
		f.sentBytes = append(f.sentBytes, len(item.Data))
	}
	if f.fail != nil {
		return "", f.fail
	}
	return f.description, nil
}

func newFakeVision() (*ViewImageTool, *fakeVision) {
	client := &fakeVision{Model: "gemini-3.5-flash-lite", description: "A dark hero section."}
	return &ViewImageTool{Client: client}, client
}

func writeTestPNG(t *testing.T, dir, name string) string {
	t.Helper()
	return writeSizedPNG(t, dir, name, 20, 20)
}

func writeSizedPNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 90, A: 255})
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

// TestViewImageReportsWhatItSaw pins the change from a stub: the tool now returns
// the model's description rather than a placeholder, which is what stops an
// agent claiming to have checked something it never saw.
func TestViewImageReportsWhatItSaw(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	tool, client := newFakeVision()
	dir := t.TempDir()
	shot := writeTestPNG(t, dir, "shot.png")

	spec, err := tool.Translate(map[string]any{"path": shot})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = dir
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if out != "A dark hero section." {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "stub") || strings.Contains(out, "view via Telegram") {
		t.Fatal("the placeholder must be gone")
	}
	if client.calls != 1 {
		t.Fatalf("model called %d times", client.calls)
	}
}

// TestViewImageForwardsThePrompt pins that a specific question reaches the model.
func TestViewImageForwardsThePrompt(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	tool, _ := newFakeVision()
	dir := t.TempDir()
	shot := writeTestPNG(t, dir, "shot.png")
	spec, err := tool.Translate(map[string]any{"path": shot, "prompt": "is the navbar overlapping?"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Args["prompt"] != "is the navbar overlapping?" {
		t.Fatalf("prompt = %v", spec.Args["prompt"])
	}
	spec.Args["workdir"] = dir
	if _, err := tool.RunContext(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}

// TestViewImageWithoutAClientSaysSo pins that a missing model is a configuration
// fact rather than a crash or a fake success.
func TestViewImageWithoutAClientSaysSo(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	tool := &ViewImageTool{}
	dir := t.TempDir()
	shot := writeTestPNG(t, dir, "shot.png")
	spec, err := tool.Translate(map[string]any{"path": shot})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = dir
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Fatal("expected an error when no model is configured")
	} else if !strings.Contains(err.Error(), "no model is configured") {
		t.Fatalf("error = %v", err)
	}
}

// TestViewImageMissingFileNamesWhereItLooked pins the error quality rule that
// already paid once: a wrong path must be correctable from the message.
func TestViewImageMissingFileNamesWhereItLooked(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	tool, _ := newFakeVision()
	workdir := t.TempDir()
	spec, err := tool.Translate(map[string]any{"path": "missing.png"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = workdir
	_, err = tool.RunContext(context.Background(), spec)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), workdir) {
		t.Fatalf("error should name the directory searched, got %v", err)
	}
}

// TestViewImageIsNotRegardedAsSequential pins that a media view does not
// serialise a turn. It is a single self-contained request that shares no state
// with anything else, so it should run alongside other tools.
func TestViewImageIsNotRegardedAsSequential(t *testing.T) {
	var candidate any = &ViewImageTool{}
	if _, sequential := candidate.(SequentialTool); sequential {
		t.Fatal("a media view holds no shared state and should not be sequential")
	}
}

// TestViewImageTranslateRejectsBadArguments pins the cheap checks.
func TestViewImageTranslateRejectsBadArguments(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	tool, _ := newFakeVision()
	for _, args := range []map[string]any{{}, {"path": ""}, {"path": 5}} {
		if _, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) should have failed", args)
		}
	}
}

// TestViewImageTranslateResultNeverEchoesAFile pins that a failed view reads as a
// failure rather than an empty success, so an agent does not read silence as
// "nothing to report".
func TestViewImageTranslateResultNeverEchoesAFile(t *testing.T) {
	tool := &ViewImageTool{}
	got := tool.TranslateResult(operationWithError("boom"))
	if !strings.Contains(got, "Error:") {
		t.Fatalf("result = %q", got)
	}
}

func operationWithError(message string) operation.Operation {
	return operation.Operation{Error: message}
}

// TestViewImageSendsTheBytesExactlyOnce is the caching guarantee stated as a
// test: the tool returns the description, never the attachment, so a later turn
// carries text rather than the image again.
func TestViewImageSendsTheBytesExactlyOnce(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	llm.ForgetMediaRefusals()
	dir := t.TempDir()
	shot := writeSizedPNG(t, dir, "shot.png", 400, 300)

	tool, client := newFakeVision()
	spec, err := tool.Translate(map[string]any{"path": shot})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = dir
	out, err := tool.RunContext(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if out != client.description {
		t.Fatalf("output = %q, want the description", out)
	}
	if strings.Contains(out, "base64") || strings.Contains(out, "data:image") {
		t.Fatal("the attachment must never be echoed into the conversation")
	}
	if len(client.sentBytes) == 0 {
		t.Fatal("the image should have been sent to the model")
	}
	if len(client.sentBytes) >= 512*1024 {
		t.Fatalf("a 400x300 png should not be sent as %d bytes", len(client.sentBytes))
	}
}

// TestViewImageRefusesAKnownBlindModel pins that a model which already refused is
// not asked again, so a turn is not spent rediscovering it.
func TestViewImageRefusesAKnownBlindModel(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	llm.ForgetMediaRefusals()
	llm.RecordMediaRefusal("blind-model", "does not support image input")

	tool, client := newFakeVision()
	client.Model = "blind-model"
	dir := t.TempDir()
	shot := writeSizedPNG(t, dir, "shot.png", 20, 20)

	_, err := tool.Translate(map[string]any{"path": shot})
	if err == nil {
		t.Fatal("translate should refuse a model known not to see")
	}
	if !strings.Contains(err.Error(), "cannot view images") {
		t.Fatalf("error should say why: %v", err)
	}
	if client.calls != 0 {
		t.Fatal("a refused model must not be called")
	}
}

// TestViewImageRecordsARefusalOnce pins the discovery path: the first attempt
// fails, the refusal is remembered, and the second attempt never reaches the
// model.
func TestViewImageRecordsARefusalOnce(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	llm.ForgetMediaRefusals()
	dir := t.TempDir()
	shot := writeSizedPNG(t, dir, "shot.png", 20, 20)

	tool, client := newFakeVision()
	client.fail = &llm.ProviderError{Status: 400, Body: `{"error":{"message":"Invalid MIME type"}}`}

	spec, err := tool.Translate(map[string]any{"path": shot})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = dir
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Fatal("expected the provider refusal to surface")
	}
	if supported, _ := llm.ModelSupportsMedia(client.Model); supported {
		t.Fatal("the refusal should have been recorded")
	}

	// Second attempt: refused before the model is called.
	client.fail = nil
	if _, err := tool.Translate(map[string]any{"path": shot}); err == nil {
		t.Fatal("the second attempt should be refused up front")
	}
	if client.calls != 1 {
		t.Fatalf("model was called %d times, want 1", client.calls)
	}
}

// TestViewImageDoesNotRecordATransientFailure pins that a timeout is not a
// permanent loss of vision.
func TestViewImageDoesNotRecordATransientFailure(t *testing.T) {
	t.Setenv("CERAA_HOME", t.TempDir())
	llm.ForgetMediaRefusals()
	dir := t.TempDir()
	shot := writeSizedPNG(t, dir, "shot.png", 20, 20)

	tool, client := newFakeVision()
	client.fail = &llm.ProviderError{Status: 503, Body: "service unavailable"}
	spec, _ := tool.Translate(map[string]any{"path": shot})
	spec.Args["workdir"] = dir
	if _, err := tool.RunContext(context.Background(), spec); err == nil {
		t.Fatal("expected an error")
	}
	if supported, _ := llm.ModelSupportsMedia(client.Model); !supported {
		t.Fatal("a transient failure must not disable vision permanently")
	}
}

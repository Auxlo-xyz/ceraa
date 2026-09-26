package tool

import (
	"context"
	"os"
	"testing"
)

type recordingSender struct {
	paths   []string
	caption []string
	kind    []MediaKind
	fail    error
}

func (r *recordingSender) SendMedia(ctx context.Context, path, caption string, kind MediaKind) (string, error) {
	r.paths = append(r.paths, path)
	r.caption = append(r.caption, caption)
	r.kind = append(r.kind, kind)
	if r.fail != nil {
		return "", r.fail
	}
	return "sent", nil
}

// TestSendMediaHoldsNoDestination pins the property that matters most here: the
// tool carries no token and no chat id, and refuses an argument that looks like
// an attempt to supply one. The model can only name a file.
func TestSendMediaHoldsNoDestination(t *testing.T) {
	dir := t.TempDir()
	shot := dir + "/shot.png"
	if err := os.WriteFile(shot, []byte("\x89PNG data"), 0o644); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	tool := &SendMediaTool{Sender: sender}

	if _, err := tool.Translate(map[string]any{"path": shot}); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []map[string]any{
		{"path": shot, "chat_id": 1},
		{"path": shot, "token": "abc"},
		{"path": shot, "bot_token": "abc"},
		{"path": shot, "recipient": "someone"},
	} {
		if _, err := tool.Translate(attempt); err == nil {
			t.Errorf("Translate(%v) should have been refused", attempt)
		}
	}
	if len(sender.paths) != 0 {
		t.Fatal("a refused call must not reach the sender")
	}
}

// TestSendMediaValidatesTheFile pins that a file is checked before an upload is
// attempted, so a missing or empty path fails as a mistake rather than a
// Telegram error.
func TestSendMediaValidatesTheFile(t *testing.T) {
	dir := t.TempDir()
	empty := dir + "/empty.txt"
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &SendMediaTool{Sender: &recordingSender{}}
	cases := []map[string]any{
		{},
		{"path": dir},
		{"path": dir + "/missing.png"},
		{"path": empty},
		{"path": dir + "/ok.png", "kind": "spreadsheet"},
	}
	for _, args := range cases {
		if _, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) should have failed", args)
		}
	}
}

// TestSendMediaPicksTheRightEndpoint pins that a screenshot is sent as a
// viewable photo rather than a document the user has to tap open.
func TestSendMediaPicksTheRightEndpoint(t *testing.T) {
	cases := map[string]string{
		"/tmp/shot.png":       "sendPhoto",
		"/tmp/clip.mp4":       "sendVideo",
		"/tmp/voice.m4a":      "sendAudio",
		"/tmp/build.log":      "sendDocument",
		"/tmp/archive.tar.gz": "sendDocument",
		"/tmp/loop.gif":       "sendPhoto",
	}
	for path, want := range cases {
		if got := MediaEndpointFor(path, MediaAuto); got != want {
			t.Errorf("MediaEndpointFor(%q) = %q, want %q", path, got, want)
		}
	}
	// An explicit kind wins, so a mislabelled extension can still be corrected.
	if got := MediaEndpointFor("/tmp/data.log", MediaPhoto); got != "sendPhoto" {
		t.Errorf("explicit kind = %q", got)
	}
}

// TestSendMediaReportsSenderFailure pins that an upload failure is a tool error.
func TestSendMediaReportsSenderFailure(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/a.txt"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &SendMediaTool{Sender: &recordingSender{fail: context.DeadlineExceeded}}
	spec, err := tool.Translate(map[string]any{"path": file})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(spec); err == nil {
		t.Fatal("expected the sender failure to surface")
	}
}

// TestSendMediaUnavailableWithoutSender pins that the tool refuses rather than
// pretending to send when there is nowhere to send to.
func TestSendMediaUnavailableWithoutSender(t *testing.T) {
	tool := &SendMediaTool{}
	dir := t.TempDir()
	file := dir + "/a.txt"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Translate(map[string]any{"path": file}); err == nil {
		t.Fatal("send_media must not be usable without a destination")
	}
}

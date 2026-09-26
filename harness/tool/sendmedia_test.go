package tool

import (
	"context"
	"os"
	"strings"
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

// TestSendMediaRejectsBadArgumentsAtTranslate pins what can be known before the
// workdir is: the argument shape. Whether the file exists is checked at run time
// instead, because a relative path cannot be resolved until then.
func TestSendMediaRejectsBadArgumentsAtTranslate(t *testing.T) {
	tool := &SendMediaTool{Sender: &recordingSender{}}
	for _, args := range []map[string]any{
		{},
		{"path": ""},
		{"path": 42},
		{"path": dirWithFile(t, "a.png"), "kind": "spreadsheet"},
	} {
		if _, err := tool.Translate(args); err == nil {
			t.Errorf("Translate(%v) should have failed", args)
		}
	}
}

func dirWithFile(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/"+name, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir + "/" + name
}

// TestSendMediaResolvesRelativePathAgainstWorkdir pins the bug that made the
// tool report a file missing while it sat in the directory the agent's own shell
// was using.
//
// A relative path from the model means "where my commands run". A shell runs in
// the session workdir, which is not the Ceraa process directory, so resolving
// against the process directory made a file the model had just created look
// absent.
func TestSendMediaResolvesRelativePathAgainstWorkdir(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(workdir+"/screenshot.png", []byte("\x89PNG data"), 0o644); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	tool := &SendMediaTool{Sender: sender}

	spec, err := tool.Translate(map[string]any{"path": "screenshot.png"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = workdir
	if _, err := tool.Run(spec); err != nil {
		t.Fatalf("a relative path must resolve against the workdir: %v", err)
	}
	if len(sender.paths) != 1 || sender.paths[0] != workdir+"/screenshot.png" {
		t.Fatalf("sent %v, want %s", sender.paths, workdir+"/screenshot.png")
	}
}

// TestSendMediaSaysWhereItLooked pins that a missing file names the resolved
// path, so the model can correct itself instead of retrying the same guess.
func TestSendMediaSaysWhereItLooked(t *testing.T) {
	workdir := t.TempDir()
	tool := &SendMediaTool{Sender: &recordingSender{}}
	spec, err := tool.Translate(map[string]any{"path": "gone.png"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Args["workdir"] = workdir
	_, err = tool.Run(spec)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), workdir+"/gone.png") {
		t.Fatalf("error should name the resolved path, got %q", err)
	}
	if !strings.Contains(err.Error(), workdir) {
		t.Fatalf("error should name the base it resolved against, got %q", err)
	}
}

// TestSendMediaChecksTheFileAtRunTime pins that a missing, empty, or directory
// target is rejected as a mistake rather than becoming a Telegram error.
func TestSendMediaChecksTheFileAtRunTime(t *testing.T) {
	dir := t.TempDir()
	empty := dir + "/empty.txt"
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	real := dir + "/ok.png"
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	tool := &SendMediaTool{Sender: sender}
	for _, path := range []string{dir, dir + "/missing.png", empty} {
		spec, err := tool.Translate(map[string]any{"path": path})
		if err != nil {
			continue
		}
		spec.Args["workdir"] = dir
		if _, err := tool.Run(spec); err == nil {
			t.Errorf("Run(%q) should have failed", path)
		}
	}
	if len(sender.paths) != 0 {
		t.Fatal("a rejected file must not reach the sender")
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

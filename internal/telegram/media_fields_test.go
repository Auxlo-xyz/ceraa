package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A music file arrives on message.audio, not on message.document. Until the
// audio, video and animation fields were parsed, such a message unmarshalled
// with every field empty and fell into the empty-text case that drops it: no
// download, no save notice, nothing on disk. That is how a two-minute mp3
// failed to reach the workspace without a word.
func TestSentMediaFieldsSurviveUnmarshal(t *testing.T) {
	raw := `{"update_id":1,"message":{"message_id":2,"chat":{"id":3},"from":{"id":3},` +
		`"audio":{"file_id":"AUDIO_ID","file_name":"The Quiet Decision.mp3","title":"The Quiet Decision"},` +
		`"video":{"file_id":"VIDEO_ID","file_name":"clip.mp4"},` +
		`"animation":{"file_id":"GIF_ID","file_name":"loop.gif"}}}`

	var u update
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	if u.Message == nil {
		t.Fatal("message did not parse")
	}
	if u.Message.Audio == nil || u.Message.Audio.FileID != "AUDIO_ID" ||
		u.Message.Audio.FileName != "The Quiet Decision.mp3" || u.Message.Audio.Title != "The Quiet Decision" {
		t.Fatalf("audio = %+v, want the file id, name and title", u.Message.Audio)
	}
	if u.Message.Video == nil || u.Message.Video.FileID != "VIDEO_ID" {
		t.Fatalf("video = %+v, want the file id", u.Message.Video)
	}
	if u.Message.Animation == nil || u.Message.Animation.FileID != "GIF_ID" {
		t.Fatalf("animation = %+v, want the file id", u.Message.Animation)
	}
	if u.Message.Text != "" {
		t.Errorf("text = %q, want empty for a file-only message", u.Message.Text)
	}
}

// The filename comes from the other end. A name that tries to climb out of the
// files directory must not become a path, and one that is not a name at all must
// be refused rather than passed through.
func TestSenderFileNameStaysUnderTheFilesDirectory(t *testing.T) {
	for name, want := range map[string]string{
		"song.mp3":         "song.mp3",
		"../../etc/passwd": "passwd",
		"a/b/c.mp3":        "c.mp3",
		"line\nbreak.txt":  "linebreak.txt",
		"..":               "",
		"":                 "",
	} {
		if got := senderFileName(name); got != want {
			t.Errorf("senderFileName(%q) = %q, want %q", name, got, want)
		}
	}
}

// Telegram hands the download a file_NNN handle with no extension worth
// trusting. The sender's name keeps the extension that auto-kind on send_media
// and ffmpeg both read, and a second send of the same track must not overwrite
// the first.
func TestSaveWithSenderNameKeepsTheExtensionAndBothUploads(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "file_1")
	if err := os.WriteFile(first, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := saveWithSenderName(first, "The Quiet Decision.mp3")
	if want := filepath.Join(dir, "The Quiet Decision.mp3"); got != want {
		t.Fatalf("saveWithSenderName = %q, want %q", got, want)
	}

	second := filepath.Join(dir, "file_2")
	if err := os.WriteFile(second, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = saveWithSenderName(second, "The Quiet Decision.mp3")
	if want := filepath.Join(dir, "The Quiet Decision (1).mp3"); got != want {
		t.Fatalf("saveWithSenderName = %q, want a counter so both uploads survive as %q", got, want)
	}
}

// The run is told what it can and cannot do with the file, so it does not read
// audio or video as bytes and answer with mojibake.
func TestUploadContextWarnsThatAudioAndVideoCannotBeRead(t *testing.T) {
	defaultState.noteUpload(4242, "audio", "/files/The Quiet Decision.mp3", "The Quiet Decision.mp3")
	defaultState.noteUpload(4242, "video", "/files/clip.mp4", "clip.mp4")
	context := uploadContext(4242)
	for _, want := range []string{
		"/files/The Quiet Decision.mp3",
		"transcription is not available",
		"/files/clip.mp4",
		"cannot watch it",
	} {
		if !strings.Contains(context, want) {
			t.Errorf("upload context missing %q:\n%s", want, context)
		}
	}
	if leftover := defaultState.takeUploads(4242); len(leftover) != 0 {
		t.Fatalf("upload context did not consume the notes: %+v", leftover)
	}
}

package whatsapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"ceraa/harness/tool"
)

// fakeMediaClient records what the sender asked for, so the send path is
// exercised without a logged-in device.
type fakeMediaClient struct {
	uploads   []fakeUpload
	sent      []*waE2E.Message
	sentTo    []types.JID
	uploadErr error
	sendErr   error
}

type fakeUpload struct {
	data []byte
	kind whatsmeow.MediaType
}

func (f *fakeMediaClient) Upload(_ context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	if f.uploadErr != nil {
		return whatsmeow.UploadResponse{}, f.uploadErr
	}
	f.uploads = append(f.uploads, fakeUpload{data: plaintext, kind: appInfo})
	return whatsmeow.UploadResponse{
		URL:           "https://mmg.whatsapp.net/fake",
		DirectPath:    "/v/fake",
		MediaKey:      []byte("key-32-bytes-0123456789abcdefghij"),
		FileEncSHA256: []byte("enc"),
		FileSHA256:    []byte("sha"),
		FileLength:    uint64(len(plaintext)),
	}, nil
}

func (f *fakeMediaClient) SendMessage(_ context.Context, to types.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	if f.sendErr != nil {
		return whatsmeow.SendResponse{}, f.sendErr
	}
	f.sent = append(f.sent, message)
	f.sentTo = append(f.sentTo, to)
	return whatsmeow.SendResponse{ID: types.MessageID("FAKE")}, nil
}

const testChatJID = "2349073100938@s.whatsapp.net"

// writeMediaFixture lays a file down and returns its path.
func writeMediaFixture(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// A sender delivers to the chat it was bound to, and nothing else. The
// destination is never read from the tool's arguments.
func TestWhatsAppMediaSenderDeliversToTheBoundChat(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "shot.png", []byte("png-bytes"))

	result, err := sender.SendMedia(t.Context(), path, "here you go", tool.MediaPhoto)
	if err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	if !strings.Contains(result, "shot.png") {
		t.Errorf("result = %q, want it to name the file", result)
	}
	if len(client.sentTo) != 1 || client.sentTo[0].String() != testChatJID {
		t.Errorf("sent to %v, want %s", client.sentTo, testChatJID)
	}
}

// With no destination bound the send is refused by name. A silent success
// here would deliver a file nowhere and report a delivery.
func TestWhatsAppMediaSenderRefusesWithoutAChat(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: "   "}
	path := writeMediaFixture(t, "shot.png", []byte("png-bytes"))

	_, err := sender.SendMedia(t.Context(), path, "", tool.MediaPhoto)
	if err == nil {
		t.Fatal("SendMedia with no destination succeeded, want the refusal")
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Errorf("error = %q, want it to name the missing destination", err)
	}
	if len(client.sent) != 0 {
		t.Errorf("sent %d messages with no destination", len(client.sent))
	}
}

// A transport that is not up returns a nil sender rather than a broken
// one, so the tool is never offered and then refused.
func TestWhatsAppMediaSenderIsNilWithoutATransport(t *testing.T) {
	if got := (*ChatTransport)(nil).MediaSender(testChatJID); got != nil {
		t.Errorf("MediaSender on a nil transport = %v, want nil", got)
	}
}

// Auto kind follows the extension, which is what the model is choosing
// from when it leaves the kind out.
func TestWhatsAppMediaSenderPicksAChannelByExtension(t *testing.T) {
	cases := []struct {
		file    string
		appInfo whatsmeow.MediaType
		message func(*waE2E.Message) bool
	}{
		{"shot.png", whatsmeow.MediaImage, func(m *waE2E.Message) bool { return m.ImageMessage != nil }},
		{"clip.mp4", whatsmeow.MediaVideo, func(m *waE2E.Message) bool { return m.VideoMessage != nil }},
		{"note.mp3", whatsmeow.MediaAudio, func(m *waE2E.Message) bool { return m.AudioMessage != nil }},
		{"report.pdf", whatsmeow.MediaDocument, func(m *waE2E.Message) bool { return m.DocumentMessage != nil }},
	}
	for _, tc := range cases {
		client := &fakeMediaClient{}
		sender := &whatsappMediaSender{client: client, chat: testChatJID}
		path := writeMediaFixture(t, tc.file, []byte("content"))

		if _, err := sender.SendMedia(t.Context(), path, "", tool.MediaAuto); err != nil {
			t.Fatalf("%s: SendMedia: %v", tc.file, err)
		}
		if got := client.uploads[0].kind; got != tc.appInfo {
			t.Errorf("%s: uploaded as %q, want %q", tc.file, got, tc.appInfo)
		}
		if !tc.message(client.sent[0]) {
			t.Errorf("%s: wrong message type for auto kind", tc.file)
		}
	}
}

// An explicit kind wins over the extension: a .txt sent as a photo has to
// leave as an image, or WhatsApp renders it as a document the user never
// asked for.
func TestWhatsAppMediaSenderHonoursAnExplicitKind(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "notes.txt", []byte("plain text"))

	if _, err := sender.SendMedia(t.Context(), path, "", tool.MediaPhoto); err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	if got := client.uploads[0].kind; got != whatsmeow.MediaImage {
		t.Errorf("uploaded as %q, want image from the explicit kind", got)
	}
	if client.sent[0].ImageMessage == nil {
		t.Error("message is not an image, want ImageMessage from MediaPhoto")
	}
}

// The upload's keys are what let WhatsApp decrypt the file again. Copying
// the URL alone produces a message that arrives as an undownloadable stub.
func TestWhatsAppMediaSenderCarriesTheUploadKeys(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "shot.png", []byte("png-bytes"))

	if _, err := sender.SendMedia(t.Context(), path, "", tool.MediaPhoto); err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	image := client.sent[0].ImageMessage
	if image == nil {
		t.Fatal("no image message")
	}
	if image.GetURL() != "https://mmg.whatsapp.net/fake" {
		t.Errorf("URL = %q, want the uploaded one", image.GetURL())
	}
	if image.GetDirectPath() != "/v/fake" {
		t.Errorf("DirectPath = %q, want the uploaded one", image.GetDirectPath())
	}
	if string(image.GetMediaKey()) != "key-32-bytes-0123456789abcdefghij" {
		t.Errorf("MediaKey = %q, want the uploaded one", image.GetMediaKey())
	}
	if string(image.GetFileSHA256()) != "sha" || string(image.GetFileEncSHA256()) != "enc" {
		t.Errorf("checksums = %q/%q, want the uploaded ones", image.GetFileSHA256(), image.GetFileEncSHA256())
	}
	if image.GetFileLength() != uint64(len([]byte("png-bytes"))) {
		t.Errorf("FileLength = %d, want %d", image.GetFileLength(), len("png-bytes"))
	}
	if image.GetMimetype() != "image/png" {
		t.Errorf("Mimetype = %q, want image/png", image.GetMimetype())
	}
}

// A caption past WhatsApp's limit is truncated, not sent and rejected: the
// upload has already happened by then and the file would be stranded.
func TestWhatsAppMediaSenderTruncatesACaption(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "shot.png", []byte("png-bytes"))
	long := strings.Repeat("a", maxWhatsAppCaption+50)

	if _, err := sender.SendMedia(t.Context(), path, long, tool.MediaPhoto); err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	caption := client.sent[0].ImageMessage.GetCaption()
	if len([]rune(caption)) > maxWhatsAppCaption {
		t.Errorf("caption is %d runes, want at most %d", len([]rune(caption)), maxWhatsAppCaption)
	}
	if !strings.HasSuffix(caption, "...") {
		t.Errorf("caption = %q, want an ellipsis on the cut", caption[len(caption)-4:])
	}
}

// An audio message has no caption field, so one is dropped rather than
// attached to a field that does not exist.
func TestWhatsAppMediaSenderDropsTheCaptionOnAudio(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "note.mp3", []byte("audio"))

	if _, err := sender.SendMedia(t.Context(), path, "listen to this", tool.MediaAuto); err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	audio := client.sent[0].AudioMessage
	if audio == nil {
		t.Fatal("no audio message")
	}
	if audio.GetMimetype() != "audio/mpeg" {
		t.Errorf("Mimetype = %q, want audio/mpeg", audio.GetMimetype())
	}
}

// A document keeps its file name, or the recipient downloads a hash.
func TestWhatsAppMediaSenderNamesTheDocument(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "report.pdf", []byte("%PDF"))

	if _, err := sender.SendMedia(t.Context(), path, "", tool.MediaAuto); err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	document := client.sent[0].DocumentMessage
	if document == nil {
		t.Fatal("no document message")
	}
	if document.GetFileName() != "report.pdf" {
		t.Errorf("FileName = %q, want report.pdf", document.GetFileName())
	}
	if document.GetMimetype() != "application/pdf" {
		t.Errorf("Mimetype = %q, want application/pdf", document.GetMimetype())
	}
}

// An oversized file is refused before the read, so the cap is a memory
// bound and not just a network one.
func TestWhatsAppMediaSenderRefusesAnOversizedFile(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "huge.bin", []byte("x"))
	// Truncate grows the file without spending the bytes.
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if err := file.Truncate(maxOutboundMediaBytes + 1); err != nil {
		t.Fatalf("grow fixture: %v", err)
	}
	file.Close()

	_, err = sender.SendMedia(t.Context(), path, "", tool.MediaDocument)
	if err == nil {
		t.Fatal("SendMedia of an oversized file succeeded, want the refusal")
	}
	if !strings.Contains(err.Error(), "above the") {
		t.Errorf("error = %q, want it to name the limit", err)
	}
	if len(client.uploads) != 0 {
		t.Error("uploaded an oversized file")
	}
}

// A missing file reports the name, so a retry can correct itself.
func TestWhatsAppMediaSenderReportsAMissingFile(t *testing.T) {
	client := &fakeMediaClient{}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}

	_, err := sender.SendMedia(t.Context(), filepath.Join(t.TempDir(), "gone.png"), "", tool.MediaPhoto)
	if err == nil {
		t.Fatal("SendMedia of a missing file succeeded, want the error")
	}
	if !strings.Contains(err.Error(), "gone.png") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

// An upload failure is reported rather than swallowed: the tool has not
// delivered anything and saying so is the whole contract.
func TestWhatsAppMediaSenderReportsAnUploadFailure(t *testing.T) {
	client := &fakeMediaClient{uploadErr: errors.New("offline")}
	sender := &whatsappMediaSender{client: client, chat: testChatJID}
	path := writeMediaFixture(t, "shot.png", []byte("png-bytes"))

	_, err := sender.SendMedia(t.Context(), path, "", tool.MediaPhoto)
	if err == nil {
		t.Fatal("SendMedia with a failing upload succeeded, want the error")
	}
	if len(client.sent) != 0 {
		t.Error("sent a message despite the upload failing")
	}
}

// Mimes Go's mime package is missing on a minimal host fall to a table, so
// a WhatsApp-common format still previews.
func TestMimeForExtFallsBackToTheTable(t *testing.T) {
	cases := map[string]string{
		"shot.webp": "image/webp",
		"clip.mkv":  "video/x-matroska",
		"note.m4a":  "audio/mp4",
	}
	for name, want := range cases {
		if got := mimeForExt(name, "application/octet-stream"); got != want {
			t.Errorf("mimeForExt(%q) = %q, want %q", name, got, want)
		}
	}
}

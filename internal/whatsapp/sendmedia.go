// Outbound media: the file half of a run, delivered to the chat the run
// already belongs to. The destination is bound in configuration and is
// never a tool argument, matching the Telegram side, so a model can name
// a file and nothing else.
package whatsapp

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"ceraa/harness/tool"
)

// maxOutboundMediaBytes caps one sent file. WhatsApp's own ceiling is far
// higher for documents, but the upload path reads the file into memory, so
// this bounds that rather than the network. Telegram's 45MB is Telegram's.
const maxOutboundMediaBytes = 32 << 20

// maxWhatsAppCaption is the caption limit on a media message. A longer one
// is truncated rather than failing an upload that is otherwise fine.
const maxWhatsAppCaption = 1024

// mediaClient is the slice of the whatsmeow client this sender needs, as an
// interface so the send path is testable without a logged-in device.
type mediaClient interface {
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// whatsappMediaSender delivers one file to one already-bound chat.
type whatsappMediaSender struct {
	client mediaClient
	chat   string
}

// compileCheck pins the sender to the seam the tool holds.
var _ tool.MediaSender = (*whatsappMediaSender)(nil)

// MediaSender returns a sender bound to one chat. The chat is the address
// the run already belongs to, read from the bridge, so a model cannot aim a
// file at a conversation it is not in.
func (t *ChatTransport) MediaSender(chat string) tool.MediaSender {
	if t == nil || t.client == nil {
		return nil
	}
	return &whatsappMediaSender{client: t.client, chat: chat}
}

func (s *whatsappMediaSender) SendMedia(ctx context.Context, path, caption string, kind tool.MediaKind) (string, error) {
	if s.client == nil {
		return "", fmt.Errorf("ceraa whatsapp: media client is not connected")
	}
	if strings.TrimSpace(s.chat) == "" {
		return "", fmt.Errorf("no WhatsApp destination is configured")
	}
	to, err := parseRecipient(s.chat)
	if err != nil {
		return "", fmt.Errorf("no WhatsApp destination is configured")
	}

	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; name a file", filepath.Base(path))
	}
	if info.Size() > maxOutboundMediaBytes {
		return "", fmt.Errorf("%s is %dMB, above the %dMB WhatsApp send limit",
			filepath.Base(path), info.Size()/(1024*1024), maxOutboundMediaBytes/(1024*1024))
	}
	plaintext, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}

	appInfo, mimeType := mediaTypeFor(path, kind)
	resp, err := s.client.Upload(ctx, plaintext, appInfo)
	if err != nil {
		return "", fmt.Errorf("upload to WhatsApp: %w", err)
	}
	// The upload response carries the keys WhatsApp needs to decrypt the
	// file again; the fields are copied verbatim, as the client documents.
	message := mediaMessage(appInfo, resp, mimeType, filepath.Base(path), caption)
	if _, err := s.client.SendMessage(ctx, to, message); err != nil {
		return "", fmt.Errorf("send to WhatsApp: %w", err)
	}
	return fmt.Sprintf("sent %s to the user", filepath.Base(path)), nil
}

// mediaMessage assembles the one message kind the upload was for. WhatsApp
// has no caption on audio, so a caption on one is dropped rather than
// attached to the wrong message type.
func mediaMessage(appInfo whatsmeow.MediaType, resp whatsmeow.UploadResponse, mimeType, fileName, caption string) *waE2E.Message {
	message := &waE2E.Message{}
	switch appInfo {
	case whatsmeow.MediaImage:
		image := &waE2E.ImageMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(resp.URL),
			DirectPath:    proto.String(resp.DirectPath),
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    proto.Uint64(resp.FileLength),
		}
		if caption = truncateCaption(caption); caption != "" {
			image.Caption = proto.String(caption)
		}
		message.ImageMessage = image
	case whatsmeow.MediaVideo:
		video := &waE2E.VideoMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(resp.URL),
			DirectPath:    proto.String(resp.DirectPath),
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    proto.Uint64(resp.FileLength),
		}
		if caption = truncateCaption(caption); caption != "" {
			video.Caption = proto.String(caption)
		}
		message.VideoMessage = video
	case whatsmeow.MediaAudio:
		// AudioMessage carries no caption field.
		message.AudioMessage = &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           proto.String(resp.URL),
			DirectPath:    proto.String(resp.DirectPath),
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    proto.Uint64(resp.FileLength),
		}
	default:
		document := &waE2E.DocumentMessage{
			Mimetype:      proto.String(mimeType),
			FileName:      proto.String(fileName),
			URL:           proto.String(resp.URL),
			DirectPath:    proto.String(resp.DirectPath),
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    proto.Uint64(resp.FileLength),
		}
		if caption = truncateCaption(caption); caption != "" {
			document.Caption = proto.String(caption)
		}
		message.DocumentMessage = document
	}
	return message
}

// truncateCaption keeps a caption inside WhatsApp's limit rather than
// having the send rejected for one that is slightly too long. The ellipsis
// is counted, so the result is the limit and no more.
func truncateCaption(caption string) string {
	const ellipsis = "..."
	runes := []rune(strings.TrimSpace(caption))
	if len(runes) <= maxWhatsAppCaption {
		return string(runes)
	}
	keep := maxWhatsAppCaption - len(ellipsis)
	return string(runes[:keep]) + ellipsis
}

// mediaTypeFor resolves which WhatsApp media channel a file goes through. An
// explicit kind wins; auto falls to the extension, which is what the model
// is choosing from when it leaves the kind out.
func mediaTypeFor(path string, kind tool.MediaKind) (whatsmeow.MediaType, string) {
	switch kind {
	case tool.MediaPhoto:
		return whatsmeow.MediaImage, mimeForExt(path, "image/png")
	case tool.MediaVideo:
		return whatsmeow.MediaVideo, mimeForExt(path, "video/mp4")
	case tool.MediaAudio:
		return whatsmeow.MediaAudio, mimeForExt(path, "audio/mpeg")
	case tool.MediaDocument:
		return whatsmeow.MediaDocument, mimeForExt(path, "application/octet-stream")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return whatsmeow.MediaImage, mimeForExt(path, "image/png")
	case ".mp4", ".mov", ".webm", ".mkv", ".avi":
		return whatsmeow.MediaVideo, mimeForExt(path, "video/mp4")
	case ".mp3", ".m4a", ".ogg", ".oga", ".wav", ".flac":
		return whatsmeow.MediaAudio, mimeForExt(path, "audio/mpeg")
	}
	return whatsmeow.MediaDocument, mimeForExt(path, "application/octet-stream")
}

// mimeForExt answers the mime type for a path. mime.TypeByExtension reads the
// host mime.types, which is missing several formats WhatsApp deals in daily, so
// a short table fills those and the fallback stands in when the file is known
// by neither.
func mimeForExt(path, fallback string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if known := knownMime[ext]; known != "" {
		return known
	}
	if guessed := mime.TypeByExtension(ext); guessed != "" {
		return strings.TrimSpace(strings.SplitN(guessed, ";", 2)[0])
	}
	return fallback
}

// knownMime covers the extensions Go's mime package does not carry on a
// minimal host, and the ones WhatsApp clients expect to preview correctly.
var knownMime = map[string]string{
	".webp": "image/webp",
	".gif":  "image/gif",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".m4a":  "audio/mp4",
	".oga":  "audio/ogg",
	".flac": "audio/flac",
}

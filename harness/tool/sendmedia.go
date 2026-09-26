package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ceraa/harness/operation"
)

// MediaKind selects which Telegram endpoint a file is sent through, so a video
// is not uploaded as a document and shown as an unplayable attachment.
type MediaKind string

const (
	MediaAuto     MediaKind = "auto"
	MediaPhoto    MediaKind = "photo"
	MediaDocument MediaKind = "document"
	MediaVideo    MediaKind = "video"
	MediaAudio    MediaKind = "audio"
)

// maxMediaBytes is Telegram's bot API upload ceiling for ordinary files.
const maxMediaBytes = 45 * 1024 * 1024

// MediaSender delivers a file to the user. It is injected rather than
// constructed from configuration so the tool holds no credentials: the model
// can only name a file, never a destination or a token.
type MediaSender interface {
	SendMedia(ctx context.Context, path, caption string, kind MediaKind) (string, error)
}

// SendMediaTool sends a file the agent produced to the user over Telegram.
//
// The destination and the bot token come from the already-configured account.
// They are deliberately not arguments, so the model cannot address a different
// chat, and cannot read a credential to try.
type SendMediaTool struct {
	Sender MediaSender
}

func (s *SendMediaTool) Name() string { return "send_media" }

func (s *SendMediaTool) Description() string {
	return "Send a file to the user on Telegram: an image, a document, a video, or audio. " +
		"Use this to deliver something you produced, such as a screenshot of a page you built, " +
		"rather than only describing it or leaving a path. The destination is the chat you are " +
		"already talking in, so name only the file."
}

func (s *SendMediaTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute path of the file to send. Must already exist.",
			},
			"caption": map[string]any{
				"type":        "string",
				"description": "Short line shown with the file.",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"auto", "photo", "document", "video", "audio"},
				"description": "How to send it. Defaults to auto, which picks from the file extension.",
			},
		},
		"required": []string{"path"},
	}
}

// reservedMediaArgs are the names a model might reach for when trying to
// address a chat or supply credentials. Rejecting them makes the boundary
// explicit instead of silently ignoring an attempt.
var reservedMediaArgs = map[string]struct{}{
	"chat_id": {}, "chatid": {}, "token": {}, "bot_token": {}, "user_id": {}, "to": {}, "recipient": {},
}

func (s *SendMediaTool) Translate(args map[string]any) (operation.Spec, error) {
	if s.Sender == nil {
		return operation.Spec{}, fmt.Errorf("send_media is not available in this context")
	}
	for key := range args {
		if _, reserved := reservedMediaArgs[strings.ToLower(key)]; reserved {
			return operation.Spec{}, fmt.Errorf("send_media does not accept %q; the destination is the current chat", key)
		}
	}
	raw, ok := args["path"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return operation.Spec{}, fmt.Errorf("path is required")
	}
	// The file is not stat-ed here: a relative path can only be resolved once the
	// workdir is known, and that arrives with the spec at run time.
	if len(raw) > 4096 || strings.ContainsRune(raw, 0) {
		return operation.Spec{}, fmt.Errorf("path is not a usable file path")
	}
	kind := MediaAuto
	if requested, ok := args["kind"].(string); ok && requested != "" {
		kind = MediaKind(strings.ToLower(requested))
		switch kind {
		case MediaAuto, MediaPhoto, MediaDocument, MediaVideo, MediaAudio:
		default:
			return operation.Spec{}, fmt.Errorf("kind must be one of auto, photo, document, video, audio")
		}
	}
	spec := operation.Spec{Type: "send_media", Args: map[string]any{
		"path":    strings.TrimSpace(raw),
		"caption": strings.TrimSpace(stringArg(args, "caption")),
		"kind":    string(kind),
	}}
	if v, ok := args["session_id"]; ok {
		spec.Args["session_id"] = v
	}
	return spec, nil
}

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func (s *SendMediaTool) TranslateResult(op operation.Operation) string {
	if op.Output == "" {
		if op.Error != "" {
			return "Error: " + op.Error
		}
		return ""
	}
	return op.Output
}

func (s *SendMediaTool) Run(spec operation.Spec) (string, error) {
	return s.RunContext(context.Background(), spec)
}

func (s *SendMediaTool) RunContext(ctx context.Context, spec operation.Spec) (string, error) {
	if s.Sender == nil {
		return "", fmt.Errorf("send_media is not available in this context")
	}
	base, _ := spec.Args["workdir"].(string)
	raw, _ := spec.Args["path"].(string)
	path, err := ResolvePathFrom(base, raw)
	if err != nil {
		return "", fmt.Errorf("send_media: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Naming both the resolved path and the base makes a relative-path
			// mistake obvious, instead of leaving the model to guess where it
			// searched.
			return "", fmt.Errorf("send_media: no file at %s (relative paths resolve against %s)", path, displayBase(base))
		}
		return "", fmt.Errorf("send_media: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("send_media: %s is a directory; name a file", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("send_media: %s is empty", path)
	}
	caption, _ := spec.Args["caption"].(string)
	kind := MediaKind(stringArg(spec.Args, "kind"))
	if kind == "" {
		kind = MediaAuto
	}
	return s.Sender.SendMedia(ctx, path, caption, kind)
}

// displayBase names the directory a relative path was resolved against, for an
// error message. An empty workdir means the process directory was used.
func displayBase(base string) string {
	if trimmed := strings.TrimSpace(base); trimmed != "" {
		return trimmed
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd + " (the Ceraa process directory)"
	}
	return "the Ceraa process directory"
}

// Sequential reports that a send must not race another one. Telegram accepts
// them concurrently, but the results are returned in one message, so ordering
// them keeps the transcript readable.
func (s *SendMediaTool) Sequential() bool { return true }

// MediaEndpointFor chooses the Telegram method for a file, so an image arrives as
// a viewable photo rather than a document with a preview the user must tap.
func MediaEndpointFor(path string, kind MediaKind) string {
	if kind != MediaAuto {
		switch kind {
		case MediaPhoto:
			return "sendPhoto"
		case MediaVideo:
			return "sendVideo"
		case MediaAudio:
			return "sendAudio"
		default:
			return "sendDocument"
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return "sendPhoto"
	case ".mp4", ".mov", ".webm", ".mkv", ".avi":
		return "sendVideo"
	case ".mp3", ".m4a", ".ogg", ".oga", ".wav", ".flac":
		return "sendAudio"
	case ".tgs":
		return "sendAnimation"
	}
	return "sendDocument"
}

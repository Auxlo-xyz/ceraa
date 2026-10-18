// Inbound media landing: files dir, never /tmp. Outbound media sending
// lives in sendmedia.go.
package whatsapp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ceraa/internal/paths"
)

// MaxInboundMediaBytes caps one saved attachment at 16MB.
const MaxInboundMediaBytes = 16 << 20

// SaveInboundMedia copies an inbound attachment under filesDir and returns
// the saved path. An empty filesDir resolves via paths.FilesDir, which
// honors CERAA_FILES_DIR. The copy is size-capped and the name is
// sanitized so traversal in the source name cannot escape filesDir.
func SaveInboundMedia(msg InboundMessage, filesDir string) (string, error) {
	if strings.TrimSpace(msg.MediaPath) == "" {
		return "", fmt.Errorf("ceraa whatsapp: message has no attachment")
	}
	if filesDir == "" {
		filesDir = paths.FilesDir()
	}
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return "", fmt.Errorf("ceraa whatsapp: files dir: %w", err)
	}
	stem := sanitizeName(msg.ID)
	if stem == "" {
		return "", fmt.Errorf("ceraa whatsapp: attachment id is empty")
	}
	ext := sanitizeExt(filepath.Ext(filepath.Base(msg.MediaPath)))
	dest := filepath.Join(filesDir, "wa-"+stem+ext)
	if err := refuseEscape(filesDir, dest); err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(msg.MediaPath); err == nil {
		if destAbs, err := filepath.Abs(dest); err == nil && abs == destAbs {
			return dest, nil
		}
	}
	src, err := os.Open(msg.MediaPath)
	if err != nil {
		return "", fmt.Errorf("ceraa whatsapp: open attachment: %w", err)
	}
	defer src.Close()
	if fi, err := src.Stat(); err == nil && fi.Size() > MaxInboundMediaBytes {
		return "", fmt.Errorf("ceraa whatsapp: attachment larger than %d bytes", MaxInboundMediaBytes)
	}
	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("ceraa whatsapp: save attachment: %w", err)
	}
	written, copyErr := io.Copy(dst, io.LimitReader(src, MaxInboundMediaBytes+1))
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("ceraa whatsapp: save attachment: %w", copyErr)
	}
	if written > MaxInboundMediaBytes {
		_ = os.Remove(dest)
		return "", fmt.Errorf("ceraa whatsapp: attachment larger than %d bytes", MaxInboundMediaBytes)
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("ceraa whatsapp: save attachment: %w", closeErr)
	}
	return dest, nil
}

// sanitizeName keeps a stable file stem from an id.
func sanitizeName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "._")
}

// sanitizeExt keeps a short lowercase extension or drops it.
func sanitizeExt(ext string) string {
	ext = strings.ToLower(ext)
	if ext == "" || ext[0] != '.' {
		return ""
	}
	if len(ext) > 12 {
		return ""
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return ext
}

// refuseEscape rejects a dest that escapes filesDir.
func refuseEscape(filesDir, dest string) error {
	base, err := filepath.Abs(filesDir)
	if err != nil {
		return fmt.Errorf("ceraa whatsapp: files dir: %w", err)
	}
	target, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("ceraa whatsapp: dest path: %w", err)
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("ceraa whatsapp: attachment name escapes the files dir")
	}
	return nil
}

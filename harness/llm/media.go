package llm

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	// Decoders only: the common web formats must be readable, but everything
	// that gets sent is re-encoded as JPEG below, so no other encoder is needed.
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// Media encoding for providers that accept images or video inline.
//
// Every provider prices and caches on the bytes actually sent, so an image is
// downscaled and re-encoded before it leaves the machine. A screenshot of a web
// page is several megabytes as a PNG and around a tenth of that as a JPEG,
// which is the difference between a usable turn and an unusable one.

const (
	// MaxMediaEdge is the longest edge kept when resizing. Large enough that UI
	// text stays legible, small enough that the payload is sane.
	MaxMediaEdge = 1568
	// MaxMediaBytes is the encoded ceiling for an inline attachment.
	MaxMediaBytes = 4 * 1024 * 1024
	// JPEGSignalQuality is high enough to keep text readable in a screenshot.
	JPEGSignalQuality = 82
)

// MediaKind distinguishes what an attachment is, because providers validate the
// declared type and an image sent as video is rejected.
type MediaType string

const (
	MediaTypeImage MediaType = "image"
	MediaTypeVideo MediaType = "video"
	MediaTypeAudio MediaType = "audio"
)

// MediaAttachment is one inline file ready to be placed in a request.
type MediaAttachment struct {
	Type     MediaType
	MimeType string
	// Data holds the encoded bytes. It is never logged.
	Data []byte
	// OriginalName is kept for the model and for error messages.
	OriginalName string
	// OriginalBytes records the size before any resizing, so a caller can report
	// what it saved.
	OriginalBytes int
	// Resized reports whether the bytes were re-encoded.
	Resized bool
}

var extensionMimeTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".ogg":  "audio/ogg",
	".wav":  "audio/wav",
	".flac": "audio/flac",
}

// MimeTypeForFile guesses a type from the extension. A wrong type is rejected by
// the provider, so an unknown extension is refused rather than guessed at.
func MimeTypeForFile(path string) (string, MediaType, bool) {
	mime, ok := extensionMimeTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return "", "", false
	}
	switch {
	case strings.HasPrefix(mime, "image/"):
		return mime, MediaTypeImage, true
	case strings.HasPrefix(mime, "video/"):
		return mime, MediaTypeVideo, true
	case strings.HasPrefix(mime, "audio/"):
		return mime, MediaTypeAudio, true
	}
	return mime, "", false
}

// LoadMediaAttachment reads a file and prepares it for an inline request.
//
// Images are downscaled and re-encoded as JPEG unless they are already small,
// which keeps a screenshot cheap to send. Video and audio are passed through
// untouched, because re-encoding them is not something to attempt here.
func LoadMediaAttachment(path string) (*MediaAttachment, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; name a file", filepath.Base(path))
	}
	mime, kind, ok := MimeTypeForFile(path)
	if !ok {
		return nil, fmt.Errorf("cannot tell the type of %s from its extension", filepath.Base(path))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	attachment := &MediaAttachment{
		Type:          kind,
		MimeType:      mime,
		OriginalName:  filepath.Base(path),
		OriginalBytes: len(raw),
	}
	if kind == MediaTypeImage {
		prepared, mime, resized, err := prepareImage(raw)
		if err == nil {
			attachment.Data = prepared
			attachment.MimeType = mime
			attachment.Resized = resized
		} else {
			// A file that claims to be an image but will not decode is still
			// passed through: some providers accept it, and refusing here would
			// be a guess about a file we have not been asked to interpret.
			attachment.Data = raw
		}
	} else {
		attachment.Data = raw
	}
	if len(attachment.Data) > MaxMediaBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte inline limit", attachment.OriginalName, len(attachment.Data), MaxMediaBytes)
	}
	return attachment, nil
}

// prepareImage decodes, optionally resizes, and re-encodes an image as JPEG,
// reporting the type of what it actually produced.
//
// An oversized image is always downscaled, even when the re-encoded file comes
// out a little larger in bytes. Providers price an image by its dimensions
// rather than its length on the wire, so a 3000x2000 screenshot sent as 128KB of
// PNG costs far more than the same page as 150KB of 1568px JPEG. Pixels are the
// expensive axis; bytes are the cheap one.
func prepareImage(raw []byte) (data []byte, mime string, resized bool, err error) {
	decoded, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", false, err
	}
	originalMime := "image/" + format
	if format == "jpeg" {
		originalMime = "image/jpeg"
	}
	// Already small enough: leave it alone, so a PNG that is not oversized keeps
	// its lossless text rather than becoming a generational JPEG.
	if fitsWithin(decoded.Bounds(), MaxMediaEdge) && len(raw) <= 512*1024 {
		return raw, originalMime, false, nil
	}
	scaled := resizeToFit(decoded, MaxMediaEdge)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: JPEGSignalQuality}); err != nil {
		return nil, "", false, err
	}
	// Encoding failed to help, so send the original rather than a larger file.
	if out.Len() >= len(raw) && fitsWithin(decoded.Bounds(), MaxMediaEdge) {
		return raw, originalMime, false, nil
	}
	return out.Bytes(), "image/jpeg", true, nil
}

func fitsWithin(bounds image.Rectangle, maxEdge int) bool {
	return bounds.Dx() <= maxEdge && bounds.Dy() <= maxEdge
}

// resizeToFit scales an image down so its longest edge is at most maxEdge,
// preserving aspect ratio. A bilinear filter is deliberate: this runs inside a
// tool call, and a better kernel is not worth the time.
func resizeToFit(src image.Image, maxEdge int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= maxEdge && height <= maxEdge {
		return src
	}
	scale := float64(maxEdge) / float64(max(width, height))
	targetWidth := int(float64(width) * scale)
	targetHeight := int(float64(height) * scale)
	if targetWidth < 1 {
		targetWidth = 1
	}
	if targetHeight < 1 {
		targetHeight = 1
	}
	destination := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		sourceY := bounds.Min.Y + y*height/targetHeight
		for x := 0; x < targetWidth; x++ {
			sourceX := bounds.Min.X + x*width/targetWidth
			destination.Set(x, y, src.At(sourceX, sourceY))
		}
	}
	return destination
}

// Base64Data renders the attachment for a provider field that expects a base64
// string, which is the shape both the Gemini native API and the OpenAI
// compatibility layer use.
func (m *MediaAttachment) Base64Data() string {
	return base64.StdEncoding.EncodeToString(m.Data)
}

// Describe renders the attachment for a model, naming what it is and how it was
// prepared so the model is not guessing at the file it was given.
func (m *MediaAttachment) Describe() string {
	note := fmt.Sprintf("%s (%s, %s)", m.OriginalName, m.MimeType, humanBytes(len(m.Data)))
	if m.Resized {
		note += fmt.Sprintf(", downscaled for sending from %s", humanBytes(m.OriginalBytes))
	}
	return note
}

// HumanBytes is the readable size used in a tool result that describes an
// attachment, so the model is told the payload it is about to receive.
func HumanBytes(n int) string { return humanBytes(n) }

func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.0fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

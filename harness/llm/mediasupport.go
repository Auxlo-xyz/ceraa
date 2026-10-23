package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ceraa/internal/paths"
)

// Multimodal support is discovered, not assumed.
//
// A model that rejects an attachment fails the whole turn, so a failure is
// remembered: the same model is not offered media again, and the reason is kept
// so the behaviour is documented rather than mysterious.

// visionFamilies are model families known to accept images. They are a hint, not
// a gate: an unlisted model is still attempted, because refusing preemptively
// would be the wrong guess about a model nobody has heard of.
var visionFamilies = []string{
	"gemini", "gemma", "gpt-4o", "gpt-4.1", "gpt-4.5", "gpt-5", "o3", "o4",
	"claude-3", "claude-4", "claude-sonnet", "claude-opus", "claude-haiku",
	"llama-3.2-vision", "llama-4", "pixtral", "qwen-vl", "qwen2-vl",
	"internvl", "mistral-small", "ministral", "phi-3", "phi-4",
	"grok-2-vision", "grok-vision", "deepseek-vl", "nova-lite", "nova-pro",
}

var (
	mediaSupportMu        sync.Mutex
	mediaSupportLoaded    bool
	mediaSupportRefusals  map[string]string
	mediaSupportHostOfKey = func(model string) string { return "" }
)

// SetMediaSupportHostResolver supplies the host for a model, so a refusal is
// recorded per provider rather than per name: the same model name behind two
// providers can accept different things.
func SetMediaSupportHostResolver(resolver func(model string) string) {
	mediaSupportMu.Lock()
	defer mediaSupportMu.Unlock()
	mediaSupportHostOfKey = resolver
	mediaSupportLoaded = false
	mediaSupportRefusals = nil
}

// mediaSupportFile is where learned refusals are kept, so they survive a
// restart. A refusal that is forgotten is a refusal that will be paid for again.
func mediaSupportFile() string {
	return filepath.Join(paths.DataDir(), "media-support.json")
}

func loadMediaSupportLocked() {
	if mediaSupportLoaded {
		return
	}
	mediaSupportLoaded = true
	mediaSupportRefusals = map[string]string{}
	raw, err := os.ReadFile(mediaSupportFile())
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &mediaSupportRefusals)
}

func saveMediaSupportLocked() {
	encoded, err := json.MarshalIndent(mediaSupportRefusals, "", "  ")
	if err != nil {
		return
	}
	path := mediaSupportFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// A failure here costs one repeated attempt later, which is not worth
	// failing a turn over.
	_ = os.WriteFile(path, encoded, 0o600)
}

func mediaSupportKeyLocked(model string) string {
	host := mediaSupportHostOfKey(model)
	if host == "" {
		host = "unknown"
	}
	return strings.ToLower(strings.TrimSpace(model)) + "@" + host
}

// ModelSupportsMedia reports whether an attempt is worth making, and why not.
func ModelSupportsMedia(model string) (bool, string) {
	mediaSupportMu.Lock()
	defer mediaSupportMu.Unlock()
	loadMediaSupportLocked()
	if reason, refused := mediaSupportRefusals[mediaSupportKeyLocked(model)]; refused {
		return false, reason
	}
	return true, ""
}

// RecordMediaRefusal remembers that a model rejected an attachment.
func RecordMediaRefusal(model, reason string) {
	mediaSupportMu.Lock()
	defer mediaSupportMu.Unlock()
	loadMediaSupportLocked()
	key := mediaSupportKeyLocked(model)
	if _, exists := mediaSupportRefusals[key]; exists {
		return
	}
	mediaSupportRefusals[key] = reason
	saveMediaSupportLocked()
}

// ForgetMediaRefusals clears what has been learned, for a model whose support may
// have arrived since. Returns how many entries were dropped.
func ForgetMediaRefusals() int {
	mediaSupportMu.Lock()
	defer mediaSupportMu.Unlock()
	loadMediaSupportLocked()
	count := len(mediaSupportRefusals)
	mediaSupportRefusals = map[string]string{}
	saveMediaSupportLocked()
	return count
}

// LearnedMediaRefusals returns what has been learned, for display and for the
// documentation the user asked to be kept.
func LearnedMediaRefusals() map[string]string {
	mediaSupportMu.Lock()
	defer mediaSupportMu.Unlock()
	loadMediaSupportLocked()
	copied := make(map[string]string, len(mediaSupportRefusals))
	for key, reason := range mediaSupportRefusals {
		copied[key] = reason
	}
	return copied
}

// mediaRefusalMarkers identify a provider declining media, as opposed to any
// other failure. Only these are remembered, so a timeout never permanently
// disables a model's vision.
var mediaRefusalMarkers = []string{
	"unsupported mime type", "unsupported media", "unsupported file type",
	"invalid mime type", "unsupported modality",
	"does not support image", "not support vision", "vision is not supported",
	"image input", "image_url", "multimodal", "inline_data", "content type",
	"text input only",
}

// IsMediaRefusal reports whether an error is the provider declining media.
func IsMediaRefusal(message string) bool {
	lowered := strings.ToLower(message)
	for _, marker := range mediaRefusalMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// MediaRefusalMessage is what the model is told when it cannot see, phrased so it
// stops trying rather than retrying the same call.
func MediaRefusalMessage(model, reason string) string {
	return fmt.Sprintf("This model (%s) cannot view images: %s. That is recorded, so it will not be "+
		"asked again. Use send_media to hand a file to the user, or describe what needs inspecting.",
		model, reason)
}

// RefusalReason extracts the provider's own explanation, so a recorded refusal
// says what the provider objected to rather than only that it objected.
func RefusalReason(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		// The body is a JSON envelope; the message inside it is the useful part.
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(providerErr.Body), &envelope) == nil && envelope.Error.Message != "" {
			return truncateMediaReason(envelope.Error.Message)
		}
	}
	return truncateMediaReason(message)
}

func truncateMediaReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	const limit = 240
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit-1] + "…"
}

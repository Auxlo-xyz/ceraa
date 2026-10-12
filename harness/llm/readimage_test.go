package llm

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wire format under test: with view_image being deleted, the Messages
// conversion is the only route by which a read's image can reach the model. An
// Item carrying an image path must become a native image block, and if it ever
// degrades to text the model sees mojibake instead of the picture.

// testImagePNG writes a small valid PNG and returns its path. The bytes come
// from the helper already in this package (testPNG), so the fixture is
// generated rather than checked in.
func testImagePNG(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, testPNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An Item with Images becomes a real content block with a base64 source, not
// text.
func TestAnImageItemBecomesANativeImageBlock(t *testing.T) {
	shot := testImagePNG(t)

	items := []Item{
		{Kind: "text", Role: RoleUser, Text: "what is in this picture?"},
		{Kind: "tool_call", CallID: "c1", Name: "read", Args: `{"path":"shot.png"}`},
		{Kind: "text", Role: RoleTool, CallID: "c1", Name: "read",
			Output: "shot.png attached as an image (image/png, 1.2 kB).", Images: []string{shot}},
	}
	messages := encodeAnthropicMessages(items)
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("the request does not encode: %v", err)
	}
	body := string(raw)

	// Locate the block structurally rather than by substring, so this cannot
	// pass on a string that merely mentions an image.
	var found int
	var check func(v any)
	check = func(v any) {
		switch node := v.(type) {
		case []any:
			for _, e := range node {
				check(e)
			}
		case map[string]any:
			if node["type"] == "image" {
				found++
				source, ok := node["source"].(map[string]any)
				if !ok {
					t.Errorf("image block has no source object: %v", node)
					return
				}
				if source["type"] != "base64" {
					t.Errorf("source type = %v, want base64", source["type"])
				}
				mediaType, _ := source["media_type"].(string)
				if !strings.HasPrefix(mediaType, "image/") {
					t.Errorf("media_type = %q, want an image type", mediaType)
				}
				data, _ := source["data"].(string)
				decoded, err := base64.StdEncoding.DecodeString(data)
				if err != nil {
					t.Errorf("source data is not base64: %v (%d chars)", err, len(data))
					return
				}
				if len(decoded) == 0 {
					t.Error("the image block carries no bytes")
				}
			}
			for _, e := range node {
				check(e)
			}
		}
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the encoded request is not JSON: %v", err)
	}
	check(parsed)
	if found != 1 {
		t.Fatalf("found %d image blocks, want exactly 1:\n%s", found, body)
	}

	// The bytes never appear as text, which is what "transcribed" looks like.
	if strings.Contains(body, "\x89PNG") || strings.Contains(body, "PNG") {
		t.Error("raw PNG bytes reached the request as text")
	}
	// The result text still describes the attachment beside the image.
	if !strings.Contains(body, "attached as an image") {
		t.Error("the attachment note is missing from the request")
	}
}

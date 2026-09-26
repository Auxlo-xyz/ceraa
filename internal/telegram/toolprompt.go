package telegram

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// describeToolRequest renders a tool call for a human deciding whether to allow
// it.
//
// Rendering the arguments with fmt is unusable in practice: arguments arrive as
// a map, so the default formatting produces Go's map syntax, and a single
// content field then dumps an entire file into the chat. The point of the prompt
// is to let someone judge the call at a glance, so only the identifying fields
// are shown, and every value is bounded.
func describeToolRequest(tool string, args any) string {
	fields := toolArgFields(args)
	if len(fields) == 0 {
		if value := strings.TrimSpace(scalarText(args)); value != "" {
			return value
		}
		return "no arguments"
	}

	// A few tools have one field that fully identifies the call, and showing
	// anything else is noise.
	if primary, ok := primaryToolArg(tool); ok {
		if value := strings.TrimSpace(fields[primary]); value != "" {
			if size, isSize := payloadSizeField(tool); isSize {
				return fmt.Sprintf("%s\n%s (%s)", primary, truncateRunes(value, 400), sizeLabel(fields[size]))
			}
			return primary + ": " + truncateRunes(value, 400)
		}
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s: %s", key, truncateRunes(fields[key], 200)))
	}
	return strings.Join(lines, "\n")
}

// payloadSizeFields are the keys whose value is a payload rather than an
// identifier. Their size is reported instead of their content.
var payloadSizeFields = map[string]string{
	"write":    "content",
	"edit":     "new_text",
	"bash":     "",
	"webfetch": "",
}

// primaryToolArg names the one argument that identifies a call for a tool, and
// whether that tool also carries a payload worth sizing.
func primaryToolArg(tool string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "bash":
		return "command", true
	case "read", "write", "edit", "glob", "ls", "view_image", "apply_patch":
		return "path", true
	case "grep", "fts_search":
		return "pattern", true
	case "webfetch":
		return "url", false
	case "browser":
		return "action", false
	}
	return "", false
}

// payloadSizeField reports the payload argument for a tool, if it has one.
func payloadSizeField(tool string) (string, bool) {
	field, ok := payloadSizeFields[strings.ToLower(strings.TrimSpace(tool))]
	return field, ok && field != ""
}

func sizeLabel(content string) string {
	length := utf8.RuneCountInString(content)
	if length == 0 {
		return "empty"
	}
	unit := "chars"
	return fmt.Sprintf("%d %s", length, unit)
}

// toolArgFields flattens arguments into string form, accepting both the map form
// used by tool calls and a raw string.
func toolArgFields(args any) map[string]string {
	switch typed := args.(type) {
	case nil:
		return nil
	case map[string]any:
		fields := make(map[string]string, len(typed))
		for key, value := range typed {
			fields[key] = scalarText(value)
		}
		return fields
	case map[string]string:
		fields := make(map[string]string, len(typed))
		for key, value := range typed {
			fields[key] = value
		}
		return fields
	case string:
		return map[string]string{"input": typed}
	}
	if text := scalarText(args); text != "" {
		return map[string]string{"input": text}
	}
	return nil
}

// scalarText renders one argument value as a single line. Structured values fall
// back to compact JSON so a nested object is still legible.
func scalarText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return fmt.Sprintf("%t", typed)
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", typed), "0"), ".")
	case int, int64, float32:
		return fmt.Sprintf("%d", typed)
	case []any, map[string]any:
		if encoded, err := json.Marshal(typed); err == nil {
			return string(encoded)
		}
		return fmt.Sprintf("%v", typed)
	}
	return fmt.Sprintf("%v", value)
}

// truncateRunes bounds a rendered value on a rune boundary so multi-byte text is
// never cut mid-character.
func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

// summariseToolRequest wraps the rendered arguments for a Markdown message,
// quoting each line so a path or command is not reflowed by the chat client.
func summariseToolRequest(tool string, args any) string {
	described := describeToolRequest(tool, args)
	lines := strings.Split(described, "\n")
	for i, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines[i] = "`" + trimmed + "`"
		}
	}
	return strings.Join(lines, "\n")
}

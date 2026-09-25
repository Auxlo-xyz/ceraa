package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ReasonLevel controls how much internal reasoning the model emits.
type ReasonLevel string

const (
	ReasonOff     ReasonLevel = "off"
	ReasonMinimal ReasonLevel = "minimal"
	ReasonLow     ReasonLevel = "low"
	ReasonMedium  ReasonLevel = "medium"
	ReasonHigh    ReasonLevel = "high"
	ReasonXHigh   ReasonLevel = "xhigh"
	ReasonMax     ReasonLevel = "max"
)

func (r ReasonLevel) Valid() bool {
	switch r {
	case ReasonOff, ReasonMinimal, ReasonLow, ReasonMedium, ReasonHigh, ReasonXHigh, ReasonMax:
		return true
	}
	return false
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Args             string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

var textToolCallPattern = regexp.MustCompile(`(?is)<?tool_call>\s*([a-zA-Z0-9_.-]+)(.*?)</?tool_call>`)
var textToolArgumentPattern = regexp.MustCompile(`(?is)<arg_key>\s*(.*?)\s*</arg_key>\s*<arg_value>\s*(.*?)\s*</arg_value>`)
var textArgValuePattern = regexp.MustCompile(`(?is)<arg_value>\s*(.*?)\s*</arg_value>`)

func normalizeTextToolCall(value string) string {
	return strings.NewReplacer("\u200b", "", "\u2060", "").Replace(value)
}

func ParseTextToolCall(text string) (StreamedToolCall, bool) {
	text = strings.TrimSpace(normalizeTextToolCall(text))
	match := textToolCallPattern.FindStringSubmatch(text)
	if len(match) != 3 || strings.TrimSpace(match[0]) != text || strings.TrimSpace(match[1]) != "run" {
		return StreamedToolCall{}, false
	}
	values := make(map[string]string)
	for _, pair := range textToolArgumentPattern.FindAllStringSubmatch(match[2], -1) {
		if len(pair) == 3 {
			values[strings.TrimSpace(pair[1])] = strings.TrimSpace(pair[2])
		}
	}
	toolName := values["tool"]
	rawArguments := values["arguments"]
	if toolName == "" || rawArguments == "" {
		return StreamedToolCall{}, false
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(rawArguments), &arguments); err != nil {
		return StreamedToolCall{}, false
	}
	encoded, err := json.Marshal(map[string]any{"tool": toolName, "arguments": arguments})
	if err != nil {
		return StreamedToolCall{}, false
	}
	return StreamedToolCall{Name: "run", Arguments: string(encoded)}, true
}

func ContainsTextToolCall(text string) bool {
	return textToolCallPattern.MatchString(normalizeTextToolCall(text))
}

func ExtractSingleArgValue(text string) (string, bool) {
	matches := textArgValuePattern.FindAllStringSubmatch(normalizeTextToolCall(text), -1)
	if len(matches) != 1 || len(matches[0]) != 2 {
		return "", false
	}
	value := strings.TrimSpace(matches[0][1])
	return value, value != ""
}

type Item struct {
	Kind             string          `json:"type"` // text, tool_call, tool_calls, tool_result, reasoning
	Role             Role            `json:"role,omitempty"`
	Text             string          `json:"text,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Args             string          `json:"arguments,omitempty"`
	ThoughtSignature string          `json:"thought_signature,omitempty"`
	Output           string          `json:"output,omitempty"`
	Reason           string          `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
}

func MergeReasoningDetails(parts []json.RawMessage) json.RawMessage {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		return append(json.RawMessage(nil), parts[0]...)
	}
	data, err := json.Marshal(parts)
	if err != nil {
		return nil
	}
	return data
}

func RepairToolArgumentsJSON(value string) (string, bool) {
	if json.Valid([]byte(value)) {
		return value, false
	}
	var repaired strings.Builder
	repaired.Grow(len(value) + 8)
	stack := make([]byte, 0, 4)
	inString := false
	escaped := false
	for i := 0; i < len(value); i++ {
		char := value[i]
		if inString {
			if escaped {
				repaired.WriteByte(char)
				escaped = false
				continue
			}
			switch char {
			case '\\':
				repaired.WriteByte(char)
				escaped = true
			case '"':
				if i+1 == len(value) || nextJSONDelimiter(value, i+1) != 0 {
					repaired.WriteByte('"')
					inString = false
				} else {
					repaired.WriteString("\\\"")
				}
			case '}', ']':
				repaired.WriteByte('"')
				inString = false
				if len(stack) > 0 {
					repaired.WriteByte(char)
					stack = stack[:len(stack)-1]
					continue
				}
				repaired.WriteByte(char)
			default:
				repaired.WriteByte(char)
			}
			continue
		}
		repaired.WriteByte(char)
		switch char {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, char)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if inString {
		if escaped {
			repaired.WriteByte('\\')
		}
		repaired.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			repaired.WriteByte('}')
		} else {
			repaired.WriteByte(']')
		}
	}
	result := repaired.String()
	if !json.Valid([]byte(result)) {
		return value, false
	}
	return result, result != value
}

func nextJSONDelimiter(value string, start int) byte {
	for i := start; i < len(value); i++ {
		switch value[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case ',', '}', ']', ':':
			return value[i]
		default:
			return 0
		}
	}
	return 0
}

func (i Item) String() string {
	switch i.Kind {
	case "text":
		return fmt.Sprintf("%s: %s", i.Role, i.Text)
	case "tool_call":
		return fmt.Sprintf("tool_call: %s(%s)", i.Name, i.Args)
	case "tool_calls":
		return fmt.Sprintf("tool_calls: %d", len(i.ToolCalls))
	case "tool_result":
		return fmt.Sprintf("tool_result[%s]: %s", i.CallID, i.Output)
	case "reasoning":
		return i.Reason
	}
	return ""
}

func EnsureToolCallIDs(calls []StreamedToolCall, prefix string) []StreamedToolCall {
	used := make(map[string]struct{}, len(calls))
	for i := range calls {
		id := calls[i].ID
		if id == "" {
			id = fmt.Sprintf("%s_%d", prefix, i)
		}
		if _, exists := used[id]; exists {
			id = fmt.Sprintf("%s_%d", prefix, i)
		}
		calls[i].ID = id
		used[id] = struct{}{}
	}
	return calls
}

func MergeStreamedToolCalls(dst, incoming []StreamedToolCall) []StreamedToolCall {
	positions := make(map[int]int, len(dst)+len(incoming))
	for i, call := range dst {
		positions[call.Index] = i
	}
	for _, call := range incoming {
		position, ok := positions[call.Index]
		if !ok {
			positions[call.Index] = len(dst)
			dst = append(dst, call)
			continue
		}
		current := dst[position]
		if call.ID != "" {
			current.ID = call.ID
		}
		if call.Name != "" {
			current.Name = call.Name
		}
		if call.ThoughtSignature != "" {
			current.ThoughtSignature = call.ThoughtSignature
		}
		if call.Arguments != "" {
			if current.Arguments == "" || strings.HasPrefix(call.Arguments, current.Arguments) {
				current.Arguments = call.Arguments
			} else {
				current.Arguments += call.Arguments
			}
		}
		dst[position] = current
	}
	return dst
}

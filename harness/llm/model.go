package llm

import (
	"bytes"
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
	Kind             string `json:"type"` // text, tool_call, tool_calls, tool_result, reasoning
	Role             Role   `json:"role,omitempty"`
	Text             string `json:"text,omitempty"`
	CallID           string `json:"call_id,omitempty"`
	Name             string `json:"name,omitempty"`
	Args             string `json:"arguments,omitempty"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
	// Images are file paths attached to this item, not inline bytes.
	//
	// A path rather than base64 because a prepared screenshot is a few hundred
	// kilobytes and a session holds every past message: inlining would put the
	// same image in the file once per request it appears in. The encoder loads
	// and resizes at send time, which is also where the format belongs, and the
	// resize is deterministic so a repeated request still matches the cache.
	Images           []string        `json:"images,omitempty"`
	Output           string          `json:"output,omitempty"`
	Reason           string          `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
}

func MergeReasoningDetails(parts []json.RawMessage) json.RawMessage {
	flat := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		flat = append(flat, flattenReasoningDetails(part)...)
	}
	flat = coalesceReasoningDetails(flat)
	if len(flat) == 0 {
		return nil
	}
	data, err := json.Marshal(flat)
	if err != nil {
		return nil
	}
	return data
}

// NormalizeReasoningDetails returns the detail list as the flat array of
// objects the chat-completions transports send.
//
// The shape matters on the wire. Every streaming chunk carries its own
// reasoning_details array, and merging them by concatenating the raw values
// produced an array of arrays: [[{...}],[{...}]]. The Cline gateway forwards
// that to the model provider as-is and the provider refuses the whole request
// with "expected object, received array" at messages.N.reasoning_details.0, so
// the turn came back empty instead of answering.
//
// Flattening happens at both ends on purpose: the merge no longer creates the
// nested shape, and this repairs history written before it stopped. Sessions
// live on disk across upgrades, and a stored array of arrays would keep
// poisoning every turn that replayed it.
func NormalizeReasoningDetails(raw json.RawMessage) json.RawMessage {
	flat := coalesceReasoningDetails(flattenReasoningDetails(raw))
	if len(flat) == 0 {
		return nil
	}
	data, err := json.Marshal(flat)
	if err != nil {
		return nil
	}
	return data
}

// coalesceReasoningDetails joins the consecutive text blocks one streamed turn
// produces into a single block.
//
// Reasoning arrives as a delta per chunk, so a long turn stored one entry per
// chunk: measured on a live audit session, 1,608 entries and 163k characters of
// JSON for 26k characters of thinking. The wrapper repeats "type", "format" and
// "index" on every entry, so 84 percent of the stored bytes were that repetition,
// and the same text sits again in the message's reasoning field.
//
// That is not just disk. Every estimate of the request charges the stored JSON,
// and the trim measures candidates with it, so the repetition inflated a 204k
// request to 1.1M on paper and the first overage cut the whole conversation. One
// block per turn keeps the text intact and the measurement honest.
//
// Only text blocks with the same type and format are joined, in order. Signed or
// encrypted blocks are left as they are: concatenating those changes a value the
// provider validates.
func coalesceReasoningDetails(flat []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(flat))
	for _, entry := range flat {
		var incoming reasoningTextBlock
		if json.Unmarshal(entry, &incoming) != nil || !incoming.mergeable() {
			out = append(out, entry)
			continue
		}
		if len(out) > 0 {
			var previous struct {
				reasoningTextBlock
				Index int `json:"index"`
			}
			if json.Unmarshal(out[len(out)-1], &previous) == nil && previous.mergeable() &&
				previous.Type == incoming.Type && previous.Format == incoming.Format {
				previous.Text += incoming.Text
				if merged, err := json.Marshal(previous); err == nil {
					out[len(out)-1] = merged
					continue
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

// reasoningTextBlock is the shape of one text delta in a reasoning stream.
type reasoningTextBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Format    string `json:"format"`
	Signature string `json:"signature"`
}

// mergeable names the blocks that are prose and only prose. A signature or a
// ciphertext is a value the provider validates, so it keeps its own entry and
// one entry per delta even when the type name says "text".
func (b reasoningTextBlock) mergeable() bool {
	if b.Text == "" || b.Signature != "" {
		return false
	}
	switch b.Type {
	case "reasoning.text", "text":
		return true
	}
	return false
}

// flattenReasoningDetails collects the object entries of a detail blob,
// descending one array level at a time. Anything that is not an object is
// dropped: a string or a number is not a reasoning block, and forwarding it is
// what the provider rejects.
func flattenReasoningDetails(raw json.RawMessage) []json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] != '[' {
		var probe map[string]any
		if json.Unmarshal(trimmed, &probe) != nil {
			return nil
		}
		return []json.RawMessage{trimmed}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 || bytes.Equal(item, []byte("null")) {
			continue
		}
		if item[0] == '[' {
			out = append(out, flattenReasoningDetails(item)...)
			continue
		}
		var probe map[string]any
		if json.Unmarshal(item, &probe) != nil {
			continue
		}
		out = append(out, item)
	}
	return out
}

// RepairToolArgumentsJSON makes a tool call's arguments usable.
//
// Three failures, in the order they are tried. A value that already parses is
// returned untouched. A value that parses after quotes are escaped and
// unbalanced braces and strings are closed is repaired. A value carrying more
// than one JSON object, which is what a fused content block leaves behind, keeps
// the first object that carries anything: {"command":"ls"}{"limit":40} became a
// call for a bash command renamed read, and that is the value the endpoint and
// the executor both want to see rather than a request they cannot parse.
//
// The last case used to live only in the Anthropic encoder, so it was repaired on
// one transport and reached the executor intact on every other.
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
	if json.Valid([]byte(result)) {
		return result, result != value
	}
	// Still not one value, which means it is two values rather than one broken
	// one. Keep the first that carries something.
	if recovered := FirstMeaningfulJSONValue(value); recovered != "" {
		return recovered, recovered != value
	}
	return value, false
}

// SanitizeToolCallNameCandidates splits a tool call name the model garbled
// into the tool names it may have meant, in order of likelihood.
//
// Models sometimes fuse two calls into one name and leak their framing tags
// into it: get_goal</tool_call:6124c78e><tool_call:6124c78e>bash is one call
// slot carrying get_goal and bash plus two raw tags. Every <...> span becomes
// a separator (an unclosed < runs to the end, which is what a truncated reply
// leaves behind), each part is trimmed, and a spaced part also yields its
// underscored twin, because the renderer shows get_goal as "get goal" and the
// model echoes that spelling back. A clean name returns exactly itself, so
// well-formed calls pass through untouched.
func SanitizeToolCallNameCandidates(name string) []string {
	var parts []string
	current := strings.Builder{}
	flush := func() {
		part := strings.TrimSpace(current.String())
		current.Reset()
		if part != "" {
			parts = append(parts, part)
		}
	}
	i := 0
	for i < len(name) {
		if name[i] == '<' {
			flush()
			end := strings.IndexByte(name[i:], '>')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		current.WriteByte(name[i])
		i++
	}
	flush()
	var candidates []string
	seen := map[string]bool{}
	for _, part := range parts {
		for _, variant := range []string{part, strings.ReplaceAll(part, " ", "_")} {
			if variant == "" || seen[variant] {
				continue
			}
			seen[variant] = true
			candidates = append(candidates, variant)
		}
	}
	return candidates
}

// carries something, skipping empty objects and arrays. It returns "" when s
// holds no JSON at all.
func FirstMeaningfulJSONValue(s string) string {
	decoder := json.NewDecoder(strings.NewReader(s))
	fallback := ""
	for {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			break
		}
		trimmed := strings.TrimSpace(string(value))
		if trimmed == "" {
			continue
		}
		if fallback == "" {
			fallback = trimmed
		}
		if trimmed != "{}" && trimmed != "[]" {
			return trimmed
		}
	}
	return fallback
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

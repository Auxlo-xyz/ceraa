package llm

import (
	"fmt"
	"strings"
)

// ReasonLevel controls how much internal reasoning the model emits.
type ReasonLevel string

const (
	ReasonLow    ReasonLevel = "low"
	ReasonMedium ReasonLevel = "medium"
	ReasonHigh   ReasonLevel = "high"
	ReasonXHigh  ReasonLevel = "xhigh"
	ReasonMax    ReasonLevel = "max"
)

func (r ReasonLevel) Valid() bool {
	switch r {
	case ReasonLow, ReasonMedium, ReasonHigh, ReasonXHigh, ReasonMax:
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

type Item struct {
	Kind             string     `json:"type"` // text, tool_call, tool_calls, tool_result, reasoning
	Role             Role       `json:"role,omitempty"`
	Text             string     `json:"text,omitempty"`
	CallID           string     `json:"call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	Args             string     `json:"arguments,omitempty"`
	ThoughtSignature string     `json:"thought_signature,omitempty"`
	Output           string     `json:"output,omitempty"`
	Reason           string     `json:"reasoning,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
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

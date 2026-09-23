package llm

import "fmt"

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

type Item struct {
	Kind   string `json:"type"` // text, tool_call, tool_result, reasoning
	Role   Role   `json:"role,omitempty"`
	Text   string `json:"text,omitempty"`
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Args   string `json:"arguments,omitempty"`
	Output string `json:"output,omitempty"`
	Reason string `json:"reasoning,omitempty"`
}

func (i Item) String() string {
	switch i.Kind {
	case "text":
		return fmt.Sprintf("%s: %s", i.Role, i.Text)
	case "tool_call":
		return fmt.Sprintf("tool_call: %s(%s)", i.Name, i.Args)
	case "tool_result":
		return fmt.Sprintf("tool_result[%s]: %s", i.CallID, i.Output)
	case "reasoning":
		return i.Reason
	}
	return ""
}

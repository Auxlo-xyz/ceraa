package contextbuilder

import (
	"fmt"
	"strings"

	"sooraa/harness/llm"
	"sooraa/harness/sessionstore"
)

const maxTotalTokens = 236000 // 256k - safety margin

// PruneResult reports what was cut when building context.
type PruneResult struct {
	OmittedMessages []int // indices dropped from the front
	TruncatedText   bool  // whether the current message was truncated
	TokensUsed      int   // estimated tokens used
	TokensRemaining int   // tokens left for the model
}

// Request is the prepared payload for the LLM provider.
type Request struct {
	Messages []llm.Item
	Usage    PruneResult
}

// Builder incrementally assembles messages from a session.
type Builder struct {
	sess   *sessionstore.Session
	prompt string // system prompt
	pinned []llm.Item
}

func New(sess *sessionstore.Session, systemPrompt string) *Builder {
	return &Builder{
		sess:   sess,
		prompt: systemPrompt,
	}
}

func (b *Builder) Pinned(items ...llm.Item) {
	b.pinned = append(b.pinned, items...)
}

// Build returns the final Request with a system message prepended.
func (b *Builder) Build() Request {
	var req Request
	req.Messages = make([]llm.Item, 0, len(b.sess.Messages)+2)

	// system
	req.Messages = append(req.Messages, llm.Item{
		Kind: "text",
		Role: llm.RoleSystem,
		Text: b.prompt,
	})

	// pinned additions (e.g. skills XML, references)
	req.Messages = append(req.Messages, b.pinned...)

	// conversation history
	for _, m := range b.sess.Messages {
		role := m.Role
		if role == "" {
			role = llm.RoleUser
		}
		item := llm.Item{
			Kind: "text",
			Role: role,
			Text: m.Content,
		}
		if m.ToolCall != nil {
			item.Kind = "tool_call"
			item.CallID = m.ToolCall.ID
			item.Name = m.ToolCall.Name
			item.Args = m.ToolCall.Args
		}
		if m.ToolResult != "" {
			item.Kind = "text"
			item.Role = llm.RoleTool
			item.CallID = m.ToolCallID
			if m.ToolCall != nil {
				item.CallID = m.ToolCall.ID
			}
			item.Output = m.ToolResult
		}
		if m.Reasoning != "" {
			req.Messages = append(req.Messages, llm.Item{
				Kind:   "reasoning",
				Reason: m.Reasoning,
			})
		}
		req.Messages = append(req.Messages, item)
	}

	req.Usage = PruneResult{TokensRemaining: maxTotalTokens}
	return req
}

// Truncate cuts a string to maxChars, preserving complete lines if possible.
// Returns the truncated string and the byte length cut.
func Truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	head := int(float64(maxChars) * 0.75)
	tail := maxChars - head
	if head > 0 && tail > 0 {
		return s[:head] + "...\n[truncated]\n..." + s[len(s)-tail:]
	}
	return s[:maxChars]
}

// EstimateTokens approximates tokens via 4 chars/token.
func EstimateTokens(s string) int {
	return len(s) / 4
}

// EstimateMessages calculates approximate token count.
func EstimateMessages(items []llm.Item) int {
	total := 0
	for _, item := range items {
		total += EstimateTokens(item.Text) + EstimateTokens(item.Args) + EstimateTokens(item.Output) + EstimateTokens(item.Reason)
		total += 10 // overhead per item
	}
	return total
}

// Compact summarizes a slice of messages into a single summary item,
// reducing context when near the token limit.
func Compact(items []llm.Item, summary string) []llm.Item {
	summaryItem := llm.Item{
		Kind: "text",
		Role: llm.RoleSystem,
		Text: fmt.Sprintf("Earlier conversation summary:\n%s", Truncate(summary, 1000)),
	}
	return append([]llm.Item{summaryItem}, items[len(items)-5:]...)
}

// FormatMessagesIntoPrompt joins message texts with role labels.
func FormatMessagesIntoPrompt(items []llm.Item) string {
	var sb strings.Builder
	for _, item := range items {
		switch item.Role {
		case llm.RoleSystem:
			sb.WriteString(fmt.Sprintf("SYSTEM: %s\n", item.Text))
		case llm.RoleUser:
			sb.WriteString(fmt.Sprintf("USER: %s\n", item.Text))
		case llm.RoleAssistant:
			sb.WriteString(fmt.Sprintf("ASSISTANT: %s\n", item.Text))
		case llm.RoleTool:
			sb.WriteString(fmt.Sprintf("TOOL[%s]: %s\n", item.CallID, Truncate(item.Output, 2000)))
		default:
			if item.Kind == "tool_call" {
				sb.WriteString(fmt.Sprintf("ASSISTANT(tool_call %s): %s(%s)\n", item.Name, item.CallID, item.Args))
			}
		}
	}
	return sb.String()
}

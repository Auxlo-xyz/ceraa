package contextbuilder

import (
	"fmt"
	"strings"

	"ceraa/harness/compaction"
	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
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
	return b.BuildWithLimit(maxTotalTokens)
}

func (b *Builder) BuildWithLimit(limit int) Request {
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

	// conversation history - handle compaction messages as synthetic checkpoints
	// Mirrors soraacode agent/core/src/session/runner/to-llm-message.ts
	toolNames := make(map[string]string)
	for _, m := range b.sess.Messages {
		for _, call := range m.AllToolCalls() {
			if call.Name != "" {
				toolNames[call.ID] = call.Name
			}
		}
	}
	for _, m := range b.sess.Messages {
		if m.Compaction != nil {
			// Render compaction as synthetic checkpoint -> maps to soraacode to-llm-message.ts compaction case
			req.Messages = append(req.Messages, llm.Item{
				Kind: "text",
				Role: llm.RoleUser,
				Text: fmt.Sprintf("<conversation-checkpoint>\n<summary>%s</summary>\n</conversation-checkpoint>", m.Compaction.Summary),
			})
			continue
		}
		role := m.Role
		if role == "" {
			role = llm.RoleUser
		}
		item := llm.Item{
			Kind: "text",
			Role: role,
			Text: m.Content,
		}
		if calls := m.AllToolCalls(); len(calls) > 0 {
			if len(calls) == 1 {
				args := calls[0].Args
				if repaired, changed := llm.RepairToolArgumentsJSON(args); changed {
					args = repaired
				}
				item.Kind = "tool_call"
				item.CallID = calls[0].ID
				item.Name = calls[0].Name
				item.Args = args
				item.ThoughtSignature = calls[0].ThoughtSignature
			} else {
				item.Kind = "tool_calls"
				item.ToolCalls = make([]llm.ToolCall, 0, len(calls))
				for _, call := range calls {
					args := call.Args
					if repaired, changed := llm.RepairToolArgumentsJSON(args); changed {
						args = repaired
					}
					item.ToolCalls = append(item.ToolCalls, llm.ToolCall{ID: call.ID, Name: call.Name, Args: args, ThoughtSignature: call.ThoughtSignature})
				}
			}
		}
		if m.ToolResultPresent || m.ToolResult != "" {
			item.Kind = "text"
			item.Role = llm.RoleTool
			item.CallID = m.ToolCallID
			item.Name = m.ToolName
			if item.Name == "" {
				item.Name = toolNames[m.ToolCallID]
			}
			// Truncated here, at render time, rather than in the store. Truncating
			// m.ToolResult rewrites a message that has already been sent and cached,
			// which invalidates the provider prefix from that point onward, forever:
			// the bytes are persisted, so the break can never be undone. Rendering
			// the cut keeps the session append-only and the cache prefix monotone,
			// and leaves the full result on disk for /messages and for compaction.
			item.Output = compaction.TruncateToolOutput(m.ToolResult)
		}
		if item.Role == llm.RoleAssistant && (m.Reasoning != "" || len(m.ReasoningDetails) > 0) {
			item.Reason = m.Reasoning
			item.ReasoningDetails = m.ReasoningDetails
		}
		req.Messages = append(req.Messages, item)
	}

	if limit > 0 && EstimateMessages(req.Messages) > limit {
		req.Messages = trimCompleteTurns(req.Messages, limit)
	}
	req.Usage = PruneResult{TokensRemaining: max(0, limit-EstimateMessages(req.Messages))}
	return req
}

func trimCompleteTurns(items []llm.Item, limit int) []llm.Item {
	if limit <= 0 || EstimateMessages(items) <= limit {
		return items
	}
	firstUser := len(items)
	for i, item := range items {
		if item.Kind == "text" && item.Role == llm.RoleUser {
			firstUser = i
			break
		}
	}
	if firstUser == len(items) {
		return items[:0]
	}
	prefix := append([]llm.Item(nil), items[:firstUser]...)
	var groups [][]llm.Item
	var current []llm.Item
	for _, item := range items[firstUser:] {
		if item.Kind == "text" && item.Role == llm.RoleUser && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, item)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	total := EstimateMessages(prefix)
	keepFrom := len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		groupTokens := EstimateMessages(groups[i])
		if total+groupTokens > limit {
			if keepFrom == len(groups) {
				return prefix
			}
			break
		}
		total += groupTokens
		keepFrom = i
	}
	result := append([]llm.Item(nil), prefix...)
	for i := keepFrom; i < len(groups); i++ {
		result = append(result, groups[i]...)
	}
	return result
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
		total += EstimateTokens(item.Text) + EstimateTokens(item.Args) + EstimateTokens(item.Output) + EstimateTokens(item.Reason) + EstimateTokens(string(item.ReasoningDetails))
		total += EstimateTokens(item.CallID) + EstimateTokens(item.Name)
		for _, call := range item.ToolCalls {
			total += EstimateTokens(call.ID) + EstimateTokens(call.Name) + EstimateTokens(call.Args)
		}
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

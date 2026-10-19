package contextbuilder

import (
	"fmt"
	"strings"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

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
	// replayReasoning is whether the transport that will carry this request sends
	// stored reasoning back to the provider. It decides what the context budget
	// is measured against, not what the model sees.
	replayReasoning bool
}

// ReasoningReplayed records whether the transport replays stored reasoning, so
// the context budget is measured against the bytes that actually go on the wire.
//
// The Codex transport takes text, tool calls and tool results and has no case
// for reasoning at all, so a stored reasoning block is never sent. Counting it
// anyway made the builder think a request was about a fifth larger than it was:
// on the live provider reasoning measured 20% of the request, and the budget is
// compared against the window, so the builder trimmed roughly that much real
// history out of every request to stay inside a limit the provider was never
// approaching. Trimming is not free either, because what it drops has to be
// re-read.
//
// Reasoning continuity is the reason a chat-completions provider does replay
// it, and dropping those items is a known quality cost. This only makes the
// arithmetic match the transport, which is the opposite trade.
func (b *Builder) ReasoningReplayed(replayed bool) *Builder {
	b.replayReasoning = replayed
	return b
}

func New(sess *sessionstore.Session, systemPrompt string) *Builder {
	return &Builder{
		sess:            sess,
		prompt:          systemPrompt,
		replayReasoning: true,
	}
}

func (b *Builder) Pinned(items ...llm.Item) {
	b.pinned = append(b.pinned, items...)
}

// staleResultChars caps a tool result that sits behind the session's newest
// checkpoint. The full text stays in the session and, when it was spooled, under
// the path its own notice names; what travels on later requests is the head of
// it plus a pointer back.
const staleResultChars = 600

// pointerizeEpochBoundary reports how many leading messages render their tool
// results as pointers: one past the newest checkpoint, or zero when the session
// has none.
//
// It is a pure function of the stored messages and deliberately does not track
// the newest user message, which is what it used to do. A boundary that follows
// the conversation moves on every turn: the results of the turn just finished
// flip from whole to pointer, so the bytes of an already-sent prefix change and
// the provider's cache stops matching from that point. Live runs measured that
// flip as the dominant per-turn invalidation - inside a run the prefix was
// otherwise append-only, and every run start re-billed the previous exchange
// because of it.
//
// A checkpoint only moves when a fold rewrites the session, and a fold is
// already paying for a prefix rewrite. Tying the boundary to it buys one
// invariant for free: within an epoch nothing that has been sent changes again,
// so every request is the previous request with a suffix, and the cache reads
// all of it. The cost is that results in the current epoch travel whole until
// the next fold. That is bounded by the epoch, which the compaction trigger
// already bounds, and whole results are what the model is asked to reason over
// anyway.
func pointerizeEpochBoundary(messages []sessionstore.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Compaction != nil {
			return i + 1
		}
	}
	return 0
}

// pointerizeStaleResults replaces the bodies of tool results ahead of the
// session's newest checkpoint with pointers to themselves.
//
// Past the boundary is the current epoch: those turns are too recent to have
// been summarized, and their results render whole so the model still has what
// it just read. Behind it the summary carries what the range taught, or a
// re-read is one tool call away. Either way re-sending the bodies on every
// request is paying rent on a house already owned.
//
// The copy is shallow and the replaced strings are fresh, so the session's
// stored messages are never mutated and the archive keeps what it kept.
// Thinking blocks are untouched throughout, because a thinking model must
// replay them verbatim and a pointer is not a block.
func pointerizeStaleResults(messages []sessionstore.Message) []sessionstore.Message {
	live := pointerizeEpochBoundary(messages)
	if live == 0 {
		return messages
	}
	out := append([]sessionstore.Message(nil), messages...)
	for i := 0; i < live; i++ {
		if out[i].ToolResult == "" {
			continue
		}
		out[i].ToolResult = staleResultPointer(out[i].ToolResult)
	}
	return out
}

func staleResultPointer(result string) string {
	trimmed := strings.TrimSpace(result)
	if len(trimmed) <= staleResultChars {
		return result
	}
	head := trimmed[:staleResultChars]
	if i := strings.LastIndex(head, "\n"); i > 0 {
		head = head[:i]
	}
	return head + "\n[older output - the full text is in the session transcript and, when spooled, under the path its notice names; re-read it if you need more than this]"
}

// Build returns the final Request with a system message prepended.
//
// The fallback here is the shared conservative default rather than a
// model-specific constant. A builder used without an explicit limit has no idea
// which model it is assembling for, and guessing a large window is how a small
// model ends up handed a prompt it cannot accept.
func (b *Builder) Build() Request {
	return b.BuildWithLimit(llm.FallbackContextWindow)
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
	//
	// Results behind the immediate exchange travel as pointers, not bodies: see
	// pointerizeStaleResults. The loop below renders from the copied slice, so
	// the session's stored messages are never mutated by it.
	toolNames := make(map[string]string)
	for _, m := range b.sess.Messages {
		for _, call := range m.AllToolCalls() {
			if call.Name != "" {
				toolNames[call.ID] = call.Name
			}
		}
	}
	messages := pointerizeStaleResults(b.sess.Messages)
	for _, m := range messages {
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
			// Sent verbatim. Capping a tool result is formatOutput's job, applied once
			// when the result is captured, which keeps the stored value stable
			// thereafter. Trimming here instead would re-cap an already-capped
			// result on every render, keep the head rather than the tail where
			// errors actually appear, and cost content for no cache benefit:
			// a pure function of the stored text is already byte-stable.
			item.Output = m.ToolResult
			item.Images = append([]string(nil), m.ToolImages...)
		}
		if item.Role == llm.RoleAssistant && (m.Reasoning != "" || len(m.ReasoningDetails) > 0) {
			item.Reason = m.Reasoning
			item.ReasoningDetails = m.ReasoningDetails
		}
		req.Messages = append(req.Messages, item)
	}

	if limit > 0 && requestSize(b.sess, req.Messages, b.replayReasoning) > limit {
		req.Messages = trimCompleteTurns(req.Messages, limit, b.replayReasoning)
	}
	req.Usage = PruneResult{TokensRemaining: max(0, limit-b.estimate(req.Messages))}
	return req
}

// requestSize measures the request about to be sent on the scale the provider
// itself counts, because that is the only scale a trim decision can be made on.
//
// The local estimate over the rendered items charges stored reasoning at char/4,
// and on the live opencode path that reasoning measured about 80% of the
// estimate while the provider counted none of it: one request the provider saw
// as 69k tokens estimated at 191k. Trimming on the local figure therefore fired
// at about a third of the real window, and when a single group of turns crossed
// the local figure the trim dropped everything but the system prompt - 42
// items=1 collapses on one session, each one the model losing its entire
// history mid-run.
//
// The provider has already reported the true size of the last request it
// accepted, on the assistant message that produced it. That count plus a local
// estimate of only the messages appended since - tool results and the new user
// turn, which really do travel on the wire - is the size of the next request.
// The provider's count includes the tool schemas, and the limit the runner
// passes has already had them subtracted, so the comparison errs conservative by
// the size of the catalogue: a few thousand tokens, on the safe side.
//
// A session with no reported usage yet has nothing to read, so it falls back to
// the local estimate. That is the old behaviour, and it can only apply to the
// first exchange, where there is no history to lose.
func requestSize(sess *sessionstore.Session, rendered []llm.Item, replayReasoning bool) int {
	last := 0
	lastAssistant := -1
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		message := sess.Messages[i]
		if message.Role != llm.RoleAssistant {
			continue
		}
		if size := message.Usage.ContextTokens(); size > 0 {
			last = size
			lastAssistant = i
			break
		}
	}
	if last == 0 {
		return EstimateMessagesFor(rendered, replayReasoning)
	}
	trailing := 0
	for _, message := range sess.Messages[lastAssistant+1:] {
		trailing += estimateStored(message, replayReasoning)
	}
	return last + trailing
}

// estimateStored measures one stored message the way compaction.estimateMessage
// does. It is duplicated rather than imported because the builder is a leaf
// package and the compactor sits above it; twelve lines is the cheaper side of
// that seam. If the two drift, the trim and the trigger disagree about the size
// of the same session.
func estimateStored(m sessionstore.Message, replayReasoning bool) int {
	n := EstimateTokens(m.Content) + EstimateTokens(m.ToolResult)
	if replayReasoning {
		n += EstimateTokens(m.Reasoning) + EstimateTokens(string(m.ReasoningDetails))
	}
	for _, call := range m.AllToolCalls() {
		n += EstimateTokens(call.Args) + EstimateTokens(call.Name) + 10
	}
	if m.Compaction != nil {
		n += EstimateTokens(m.Compaction.Summary) + EstimateTokens(m.Compaction.Recent)
	}
	return n + 10
}

// estimate measures the request the way the receiving transport will.
func (b *Builder) estimate(items []llm.Item) int {
	if b.replayReasoning {
		return EstimateMessages(items)
	}
	return EstimateMessagesWithoutReasoning(items)
}

// trimCompleteTurns drops whole exchanges from the front until the request fits,
// never splitting a tool call from its result.
func trimCompleteTurns(items []llm.Item, limit int, replayReasoning bool) []llm.Item {
	if limit <= 0 || EstimateMessagesFor(items, replayReasoning) <= limit {
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
	total := EstimateMessagesFor(prefix, replayReasoning)
	keepFrom := len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		groupTokens := EstimateMessagesFor(groups[i], replayReasoning)
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
	return EstimateMessagesFor(items, true)
}

// EstimateMessagesFor estimates the request as the named transport will carry
// it. With replayReasoning false, stored reasoning is excluded because that
// transport has no case for it and never sends it, so counting it would charge
// the window for bytes that do not exist on the wire.
func EstimateMessagesFor(items []llm.Item, replayReasoning bool) int {
	total := 0
	for _, item := range items {
		total += EstimateTokens(item.Text) + EstimateTokens(item.Args) + EstimateTokens(item.Output)
		if replayReasoning {
			total += EstimateTokens(item.Reason) + EstimateTokens(string(item.ReasoningDetails))
		}
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

// EstimateMessagesWithoutReasoning measures a request for a transport that
// does not replay stored reasoning.
func EstimateMessagesWithoutReasoning(items []llm.Item) int {
	return EstimateMessagesFor(items, false)
}

package compaction

import (
	"context"
	"fmt"
	"strings"

	"sooraa/harness/llm"
	"sooraa/harness/sessionstore"
)

// Constants from soraacode agent/core/src/session/compaction.ts
const (
	DefaultBuffer         = 20000
	DefaultKeepTokens     = 8000
	ToolOutputMaxChars    = 2000
	SummaryOutputTokens   = 4096
	MaxTotalTokens        = 236000 // 256k - margin, matches contextbuilder
)

// Settings mirrors soraacode Config.Document.info.compaction
type Settings struct {
	Auto   bool `json:"auto"`
	Buffer int  `json:"buffer"`
	Keep   struct {
		Tokens int `json:"tokens"`
	} `json:"keep"`
}

func DefaultSettings() Settings {
	s := Settings{Auto: true, Buffer: DefaultBuffer}
	s.Keep.Tokens = DefaultKeepTokens
	return s
}

// Token estimation via char/4, same as soraacode agent/core/src/util/token.ts
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n == 0 {
		return 1
	}
	return n
}

func estimateMessage(m sessionstore.Message) int {
	n := EstimateTokens(m.Content) + EstimateTokens(m.Reasoning) + EstimateTokens(m.ToolResult)
	if m.ToolCall != nil {
		n += EstimateTokens(m.ToolCall.Args) + EstimateTokens(m.ToolCall.Name) + 10
	}
	if m.Compaction != nil {
		n += EstimateTokens(m.Compaction.Summary) + EstimateTokens(m.Compaction.Recent)
	}
	return n + 10 // overhead
}

func estimateSession(sess *sessionstore.Session) int {
	total := 0
	for _, m := range sess.Messages {
		total += estimateMessage(m)
	}
	return total
}

// Summary template from soraacode
const summaryTemplate = `## Objective
Summarize the main goal and current task.
## Progress
What has been completed so far (terse bullets).
## Relevant Files
List files touched and their purpose.
## Decisions
Key technical decisions made.
## Next Steps
What remains to be done.
## Design Lessons Learnt
Patterns to preserve.

Rules: keep every section, terse bullets, never mention compaction, no preamble.`

const summaryUpdateInstructions = `Combine both summaries: Preserve objectives from <prior-summary> even if absent from <conversation> - <conversation> is newer and wins conflicts. Anything omitted is permanently lost.`

func serializeMessage(m sessionstore.Message) string {
	if m.Compaction != nil {
		return "" // filtered - compaction messages not serialized into head
	}
	switch m.Role {
	case llm.RoleUser:
		s := fmt.Sprintf("[User]: %s", m.Content)
		if m.ToolResult != "" {
			s += fmt.Sprintf("\n[Tool result]: %s", truncate(m.ToolResult, ToolOutputMaxChars))
		}
		return s
	case llm.RoleAssistant:
		var parts []string
		if m.Content != "" {
			parts = append(parts, fmt.Sprintf("[Assistant]: %s", m.Content))
		}
		if m.Reasoning != "" {
			parts = append(parts, fmt.Sprintf("[Assistant reasoning]: %s", truncate(m.Reasoning, ToolOutputMaxChars)))
		}
		if m.ToolCall != nil {
			parts = append(parts, fmt.Sprintf("[Assistant tool call]: %s(%s)", m.ToolCall.Name, truncate(m.ToolCall.Args, ToolOutputMaxChars)))
		}
		if m.ToolResult != "" {
			parts = append(parts, fmt.Sprintf("[Tool result]: %s", truncate(m.ToolResult, ToolOutputMaxChars)))
		}
		return strings.Join(parts, "\n")
	case llm.RoleTool:
		return fmt.Sprintf("[Tool result]: %s", truncate(m.ToolResult, ToolOutputMaxChars))
	case llm.RoleSystem:
		return fmt.Sprintf("[System]: %s", m.Content)
	default:
		if m.Content != "" {
			return fmt.Sprintf("[%s]: %s", m.Role, m.Content)
		}
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// Select splits messages into head (to summarize) and recent (to keep verbatim)
// Mirrors soraacode select() with keepTokens budget from the tail.
func Select(sess *sessionstore.Session, keepTokens int) (head string, recent string) {
	// Filter out compaction messages for selection
	var conv []string
	for _, m := range sess.Messages {
		if s := serializeMessage(m); s != "" {
			conv = append(conv, s)
		}
	}
	if len(conv) == 0 {
		return "", ""
	}
	total := 0
	split := len(conv)
	for i := len(conv) - 1; i >= 0; i-- {
		next := total + EstimateTokens(conv[i])
		if next > keepTokens {
			break
		}
		total = next
		split = i
	}
	head = strings.Join(conv[:split], "\n\n")
	recent = strings.Join(conv[split:], "\n\n")
	return head, recent
}

func buildPrompt(previousSummary string, head string, recent string) string {
	var contextParts []string
	if head != "" {
		contextParts = append(contextParts, head)
	}
	context := strings.Join(contextParts, "\n\n")
	conversation := fmt.Sprintf("Current active conversation:\n\n<conversation>\n%s\n</conversation>", context)
	if recent != "" {
		conversation += fmt.Sprintf("\n\nRecent context to preserve verbatim:\n%s", recent)
	}
	if previousSummary == "" {
		return strings.Join([]string{
			conversation,
			"Create a new anchored summary of the conversation so far.",
			summaryTemplate,
		}, "\n\n")
	}
	return strings.Join([]string{
		conversation,
		fmt.Sprintf("Here is the prior summary:\n<prior-summary>\n%s\n</prior-summary>", previousSummary),
		summaryUpdateInstructions,
		summaryTemplate,
	}, "\n\n")
}

// Compactor performs LLM-based summarization.
type Compactor struct {
	LLM      llm.ProviderConfig
	Settings Settings
}

// New creates a compactor with default settings.
func New(cfg llm.ProviderConfig) *Compactor {
	return &Compactor{LLM: cfg, Settings: DefaultSettings()}
}

// NeedsCompaction checks pre-flight like soraacode compactIfNeeded
func (c *Compactor) NeedsCompaction(sess *sessionstore.Session) bool {
	if !c.Settings.Auto {
		return false
	}
	used := estimateSession(sess)
	// Estimate request size including system prompt overhead
	contextLimit := MaxTotalTokens
	buffer := c.Settings.Buffer
	if buffer == 0 {
		buffer = DefaultBuffer
	}
	// If used + buffer exceeds context, need compaction
	return used > (contextLimit - buffer)
}

// IsOverflow checks like soraacode overflow.ts isOverflow
func (c *Compactor) IsOverflow(sess *sessionstore.Session) bool {
	if !c.Settings.Auto {
		return false
	}
	used := estimateSession(sess)
	usable := MaxTotalTokens - 20000 // reserved output
	return used >= usable
}

// CompactAfterOverflow performs summarization and rewrites session
func (c *Compactor) Compact(ctx context.Context, sess *sessionstore.Session) (bool, error) {
	keepTokens := c.Settings.Keep.Tokens
	if keepTokens == 0 {
		keepTokens = DefaultKeepTokens
	}
	// Find previous compaction summary
	var previousSummary string
	for _, m := range sess.Messages {
		if m.Compaction != nil && m.Compaction.Summary != "" {
			previousSummary = m.Compaction.Summary
		}
	}
	head, recent := Select(sess, keepTokens)
	if head == "" && previousSummary == "" {
		return false, nil
	}
	prompt := buildPrompt(previousSummary, head, recent)
	// Guard: prompt itself must fit in context - summaryOutput
	if EstimateTokens(prompt) > MaxTotalTokens-SummaryOutputTokens {
		return false, fmt.Errorf("prompt too large for compaction")
	}
	client := llm.New(c.LLM)
	req := llm.StreamRequest{
		Messages: []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: prompt}},
		Tools:    nil,
	}
	var summary strings.Builder
	err := client.Stream(ctx, req, func(delta llm.ProviderResponse) {
		if delta.Content != "" {
			summary.WriteString(delta.Content)
		}
	})
	if err != nil {
		return false, err
	}
	text := strings.TrimSpace(summary.String())
	if text == "" {
		return false, fmt.Errorf("empty summary from LLM")
	}
	// Rewrite session: keep only recent messages + new compaction message
	// Find split point: messages that contributed to recent
	recentParts := strings.Split(recent, "\n\n")
	// Simple: keep last N messages that fit keepTokens (re-derive)
	var keepCount int
	total := 0
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		if sess.Messages[i].Compaction != nil {
			continue
		}
		s := serializeMessage(sess.Messages[i])
		if s == "" {
			continue
		}
		tok := EstimateTokens(s)
		if total+tok > keepTokens && keepCount > 0 {
			break
		}
		total += tok
		keepCount++
	}
	// Build new messages: [compaction] + last keepCount
	newMessages := make([]sessionstore.Message, 0, keepCount+1)
	compMsg := sessionstore.Message{
		Role:    llm.RoleSystem,
		Content: fmt.Sprintf("<conversation-checkpoint>\n<summary>%s</summary>\n<recent-context>%s</recent-context>\n</conversation-checkpoint>", text, recent),
		Compaction: &sessionstore.CompactionInfo{
			Summary: text,
			Recent:  recent,
			Seq:     len(sess.Messages),
		},
	}
	// Use recentParts count to slice - fallback to keepCount
	_ = recentParts
	start := len(sess.Messages) - keepCount
	if start < 0 {
		start = 0
	}
	newMessages = append(newMessages, compMsg)
	for _, m := range sess.Messages[start:] {
		if m.Compaction != nil {
			continue
		}
		newMessages = append(newMessages, m)
	}
	sess.Messages = newMessages
	return true, nil
}

// Prune tool outputs like soraacode prune: keep 40k protected, prune >20k beyond
func Prune(sess *sessionstore.Session) int {
	const protect = 40000
	const minimum = 20000
	var total, pruned int
	var toPrune []*sessionstore.Message
	// Walk backwards, protect recent 40k of tool outputs
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		m := &sess.Messages[i]
		if m.ToolResult == "" {
			continue
		}
		tok := EstimateTokens(m.ToolResult)
		total += tok
		if total <= protect {
			continue
		}
		pruned += tok
		toPrune = append(toPrune, m)
	}
	if pruned <= minimum {
		return 0
	}
	for _, m := range toPrune {
		if len(m.ToolResult) > ToolOutputMaxChars {
			m.ToolResult = m.ToolResult[:ToolOutputMaxChars] + "...[pruned]"
		}
	}
	return len(toPrune)
}

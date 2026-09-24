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
	DefaultBuffer       = 20000
	DefaultKeepTokens   = 8000
	ToolOutputMaxChars  = 2000
	SummaryOutputTokens = 4096
	MaxTotalTokens      = 236000 // 256k - margin, matches contextbuilder
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
	for _, call := range m.AllToolCalls() {
		n += EstimateTokens(call.Args) + EstimateTokens(call.Name) + 10
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

type messageUnit struct {
	messages []sessionstore.Message
	tokens   int
}

func selectMessageUnits(messages []sessionstore.Message, keepTokens int) (head []sessionstore.Message, recent []sessionstore.Message) {
	var units []messageUnit
	var current []sessionstore.Message
	flush := func() {
		if len(current) == 0 {
			return
		}
		tokens := 0
		for _, message := range current {
			tokens += estimateMessage(message)
		}
		units = append(units, messageUnit{messages: current, tokens: tokens})
		current = nil
	}
	for _, message := range messages {
		if message.Compaction != nil || message.Role == llm.RoleSystem {
			flush()
			continue
		}
		if message.Role == llm.RoleUser && len(current) > 0 {
			flush()
		}
		current = append(current, message)
	}
	flush()

	split := len(units)
	total := 0
	for i := len(units) - 1; i >= 0; i-- {
		if total+units[i].tokens > keepTokens {
			if split == len(units) {
				return flattenMessages(units), nil
			}
			break
		}
		total += units[i].tokens
		split = i
	}
	for i := 0; i < split; i++ {
		head = append(head, units[i].messages...)
	}
	for i := split; i < len(units); i++ {
		recent = append(recent, units[i].messages...)
	}
	return head, recent
}

func flattenMessages(units []messageUnit) []sessionstore.Message {
	var messages []sessionstore.Message
	for _, unit := range units {
		messages = append(messages, unit.messages...)
	}
	return messages
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
		for _, call := range m.AllToolCalls() {
			parts = append(parts, fmt.Sprintf("[Assistant tool call]: %s(%s)", call.Name, truncate(call.Args, ToolOutputMaxChars)))
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

func serializeMessages(messages []sessionstore.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if serialized := serializeMessage(message); serialized != "" {
			parts = append(parts, serialized)
		}
	}
	return strings.Join(parts, "\n\n")
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
	headMessages, recentMessages := selectMessageUnits(sess.Messages, keepTokens)
	return serializeMessages(headMessages), serializeMessages(recentMessages)
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
	LLM              llm.ProviderConfig
	Settings         Settings
	ContextLimit     int
	MaxOutputTokens  int
	LastPromptTokens int
	OnUsage          func(llm.UsageInfo)
}

// New creates a compactor with default settings.
func New(cfg llm.ProviderConfig) *Compactor {
	return &Compactor{LLM: cfg, Settings: DefaultSettings(), ContextLimit: MaxTotalTokens, MaxOutputTokens: 20000}
}

func (c *Compactor) contextLimit() int {
	if c.ContextLimit > 0 {
		return c.ContextLimit
	}
	return MaxTotalTokens
}

func (c *Compactor) outputReserve() int {
	if c.MaxOutputTokens > 0 {
		return c.MaxOutputTokens
	}
	return 20000
}

// NeedsCompaction checks pre-flight like soraacode compactIfNeeded
func (c *Compactor) NeedsCompaction(sess *sessionstore.Session) bool {
	if !c.Settings.Auto {
		return false
	}
	used := estimateSession(sess)
	// Estimate request size including system prompt overhead
	contextLimit := c.contextLimit()
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
	usable := c.contextLimit() - c.outputReserve()
	return used >= usable
}

// CompactAfterOverflow performs summarization and rewrites session
func (c *Compactor) Compact(ctx context.Context, sess *sessionstore.Session) (bool, error) {
	c.LastPromptTokens = 0
	keepTokens := c.Settings.Keep.Tokens
	if keepTokens == 0 {
		keepTokens = DefaultKeepTokens
	}
	var previousSummary string
	latestCompaction := -1
	var systemMessage *sessionstore.Message
	for i, message := range sess.Messages {
		if message.Compaction != nil {
			previousSummary = message.Compaction.Summary
			latestCompaction = i
		}
		if systemMessage == nil && message.Compaction == nil && message.Role == llm.RoleSystem {
			copy := message
			systemMessage = &copy
		}
	}
	candidate := sess.Messages
	if latestCompaction >= 0 {
		candidate = sess.Messages[latestCompaction+1:]
	}
	headMessages, recentMessages := selectMessageUnits(candidate, keepTokens)
	head := serializeMessages(headMessages)
	recent := serializeMessages(recentMessages)
	if head == "" && previousSummary == "" {
		return false, nil
	}
	prompt := buildPrompt(previousSummary, head, recent)
	summaryOutput := SummaryOutputTokens
	if c.MaxOutputTokens > 0 && c.MaxOutputTokens < summaryOutput {
		summaryOutput = c.MaxOutputTokens
	}
	if EstimateTokens(prompt) > c.contextLimit()-summaryOutput {
		return false, fmt.Errorf("prompt too large for compaction")
	}
	c.LastPromptTokens = EstimateTokens(prompt)
	client := llm.New(c.LLM)
	req := llm.StreamRequest{
		Messages:  []llm.Item{{Kind: "text", Role: llm.RoleUser, Text: prompt}},
		Tools:     nil,
		MaxTokens: summaryOutput,
	}
	var summary strings.Builder
	err := client.Stream(ctx, req, func(delta llm.ProviderResponse) {
		if delta.Content != "" {
			summary.WriteString(delta.Content)
		}
		if delta.Usage != nil && c.OnUsage != nil {
			c.OnUsage(*delta.Usage)
		}
	})
	if err != nil {
		return false, err
	}
	text := strings.TrimSpace(summary.String())
	if text == "" {
		return false, fmt.Errorf("empty summary from LLM")
	}
	newMessages := make([]sessionstore.Message, 0, len(recentMessages)+2)
	if systemMessage != nil {
		newMessages = append(newMessages, *systemMessage)
	}
	newMessages = append(newMessages, sessionstore.Message{
		Role:    llm.RoleSystem,
		Content: "<conversation-checkpoint>",
		Compaction: &sessionstore.CompactionInfo{
			Summary: text,
			Recent:  recent,
			Seq:     len(sess.Messages),
		},
	})
	newMessages = append(newMessages, recentMessages...)
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

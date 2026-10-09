package compaction

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/sessionstore"
)

// Constants from soraacode agent/core/src/session/compaction.ts
const (
	DefaultBuffer       = 20000
	DefaultKeepTokens   = 8000
	SummaryOutputTokens = 4096
	// MaxTotalTokens is the fallback budget when no model-specific limit has been
	// supplied. It is the shared conservative default, not a per-model figure: a
	// real limit arrives from the catalog via the runner, and DefaultContextLimit
	// exists only so an unconfigured compactor still refuses to overrun.
	MaxTotalTokens = llm.FallbackContextWindow

	// ReserveContextTokens is the headroom kept beyond the conversation: room for
	// the model's own reply, the tool definitions, and token estimation error.
	//
	// Absolute rather than proportional, because none of those three shrink with
	// the window. A 1M-token model needs about this much; a 32k one needs it just
	// as much and cannot spare a percentage of itself instead.
	ReserveContextTokens = 16000

	// CompactAtWindowPercent is the fraction of the window compaction waits for
	// before starting, on any window large enough to afford the absolute reserve.
	//
	// Ninety percent rather than something smaller, because the reserve is already
	// accounted for separately: holding back another slice of a large window just
	// discards headroom the model could have used.
	CompactAtWindowPercent = 90
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
	n := EstimateTokens(m.Content) + EstimateTokens(m.Reasoning) + EstimateTokens(string(m.ReasoningDetails)) + EstimateTokens(m.ToolResult)
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

func estimatedContext(sess *sessionstore.Session) int {
	lastUsage := 0
	lastAssistant := -1
	for i, message := range sess.Messages {
		if message.Role == llm.RoleAssistant && message.Usage.TotalTokens > 0 {
			lastUsage = message.Usage.TotalTokens
			lastAssistant = i
		}
	}
	if lastAssistant < 0 || lastUsage == 0 {
		return estimateSession(sess)
	}
	trailing := 0
	for _, message := range sess.Messages[lastAssistant+1:] {
		trailing += estimateMessage(message)
	}
	return lastUsage + trailing
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

	// Structure first: the open exchange and the one before it. A token budget
	// can only say "this much"; it cannot say "this matters". Two exchanges
	// back the transcript is either resolved, in which case the summary carries
	// it, or still open, in which case the handoff carries it.
	first := len(units) - maxRecentExchanges
	if first < 0 {
		first = 0
	}
	// Budget second: drop whole exchanges from the front while the tail exceeds
	// it, but never the last exchange. A cap that can delete the present is
	// worse than no cap.
	for first < len(units)-1 && tailTokens(units[first:]) > keepTokens {
		first++
	}
	return flattenMessages(units[:first]), flattenMessages(units[first:])
}

func tailTokens(units []messageUnit) int {
	total := 0
	for _, unit := range units {
		total += unit.tokens
	}
	return total
}

// maxRecentExchanges bounds the verbatim tail by structure: the open exchange
// and the one before it. See selectMessageUnits for why a budget cannot do
// this job.
const maxRecentExchanges = 2

func flattenMessages(units []messageUnit) []sessionstore.Message {
	var messages []sessionstore.Message
	for _, unit := range units {
		messages = append(messages, unit.messages...)
	}
	return messages
}

// Summary template from soraacode.
//
// In-flight & Blocked and Errors & Fixes are ours. The template had nowhere to
// say that work was still running, paused or held up, or that something had
// failed and what the error read, which is exactly what the turn after a
// compaction needs to know: a reader told only "Next Steps" cannot tell a task
// that was finished from one that was abandoned half-way with a failing build.
// opencode answers this with a Work State of Completed, Active and Blocked;
// Hermes with Blocked and Errors & Fixes. Both shapes carry it here.
const summaryTemplate = `## Objective
Summarize the main goal and current task.
## Progress
What has been completed so far (terse bullets).
## In-flight & Blocked
What is running now, what is paused, and what is held up. Name the blocker for anything waiting on something else.
## Errors & Fixes
What failed and the error as it read, what was tried, and what worked. Carry forward anything still unresolved.
## Relevant Files
List files touched and their purpose.
## Decisions
Key technical decisions made.
## Next Steps
What remains to be done.
## Handoff
The next action as one command, the current position as file:line, the open blocker in one line. Written as instructions to the reader, not as history.
## Design Lessons Learnt
Patterns to preserve.

Rules: keep every section, terse bullets, never mention compaction, no preamble. Keep by value: the user's words closest to verbatim, then lasting effects and failures, then findings, tool steps last. Copy names, numbers, ids, paths and errors exactly. Never make anything look further along than it was.`

// summaryBoundaryRules is what stops a summariser from doing the work instead.
//
// The instruction asks a model that has been working to stop working, and a
// model with nothing in front of it but a transcript is the one case where
// that costs a turn rather than a mistaken edit.
const summaryBoundaryRules = `Reply with the summary and nothing else: do not call a tool, do not continue the work, do not invent a result.`

// compactionInstruction is the user turn an in-band compaction appends.
//
// There is nothing to embed: the conversation is the request, so the model has
// already been sent everything above. What it has not been sent is a reason to
// stop, and it needs one, because the instruction arrives at the end of a
// transcript whose every other user turn asked for work.
//
// It is a user turn and never a session message. Appending it to the session,
// the way the runner's final-answer nudge does, would leave a permanent
// "summarise now" as the last thing the model reads on every subsequent
// request.
func compactionInstruction(attempt int) string {
	instruction := strings.Join([]string{
		"Record what you need to remember from everything above. You will continue this same work with only this note and the recent tail; write it as yourself, to yourself. If the conversation holds a prior checkpoint, carry its unresolved items forward: anything omitted is permanently lost.",
		summaryTemplate,
		summaryBoundaryRules,
	}, "\n\n")
	if attempt > 1 {
		instruction += "\n\nYour previous reply was not in the required format. Reply with only the summary, using every heading above in order."
	}
	return instruction
}

// requiredSummaryHeadings reads the template's own headings rather than
// restating them, so a section added to the template becomes required on its
// own. A second list is the kind that goes stale quietly.
func requiredSummaryHeadings() []string {
	var headings []string
	for _, line := range strings.Split(summaryTemplate, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			headings = append(headings, strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
		}
	}
	return headings
}

// minSummaryChars is a floor, not a quality bar. It rejects the refusal and the
// one-line stub, both of which are shorter than any summary that carries every
// section at all.
const minSummaryChars = 200

// validateSummary checks that a reply is a checkpoint.
//
// The rule it replaces was "not empty", so a refusal, or a single line, was
// written as the summary of everything thrown away and the session continued on
// it. Every heading must appear, in the template's order, and carry a body.
//
// What it cannot check is truth: a summary that claims nothing is in flight
// while a failing build sits in the transcript passes every check here. The
// section exists to make that visible to the reader, not to prove it.
func validateSummary(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return fmt.Errorf("the summariser returned nothing")
	}
	if length := len([]rune(trimmed)); length < minSummaryChars {
		return fmt.Errorf("the summary is %d chars, want at least %d", length, minSummaryChars)
	}
	lines := strings.Split(trimmed, "\n")
	position := 0
	for _, heading := range requiredSummaryHeadings() {
		index := indexOfSummaryHeading(lines, heading, position)
		if index < 0 {
			return fmt.Errorf("the summary is missing the %q section", heading)
		}
		if summarySectionBody(lines, index) == "" {
			return fmt.Errorf("the summary has an empty %q section", heading)
		}
		position = index + 1
	}
	return nil
}

// indexOfSummaryHeading finds "## heading" at or after from. The order is part
// of the check: a summary that answered the sections out of order is a reply
// that was not reading the template.
func indexOfSummaryHeading(lines []string, heading string, from int) int {
	want := "## " + heading
	for i := from; i < len(lines); i++ {
		if strings.EqualFold(strings.TrimSpace(lines[i]), want) {
			return i
		}
	}
	return -1
}

func summarySectionBody(lines []string, from int) string {
	var body strings.Builder
	for i := from + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			break
		}
		body.WriteString(lines[i])
	}
	return strings.TrimSpace(body.String())
}

// Compactor performs LLM-based summarization.
type Compactor struct {
	LLM              llm.ProviderConfig
	Settings         Settings
	ContextLimit     int
	MaxOutputTokens  int
	ResolveModel     func(context.Context) (llm.ProviderConfig, error)
	LastPromptTokens int
	OnUsage          func(llm.UsageInfo)
	// ArchiveDir is where the discarded head is kept for auditing. Empty disables
	// archiving, so a caller that has nowhere to put it is unaffected.
	//
	// This is a diagnostic and never an input: nothing read back from here reaches
	// the model, so turning it on cannot change what the agent does.
	ArchiveDir string
	// Learn, if set, is asked to mine the discarded head for durable preferences
	// immediately after the head is archived. It receives the raw discarded
	// messages and the summary that was produced from them, never the compacted
	// checkpoint, because the summary is not raw enough to mine.
	//
	// It is optional and runs off the hot path: compaction is the only trigger,
	// so the cost per turn is zero. A hook that fails must not fail the turnover;
	// the implementation logs and returns.
	Learn func(ctx context.Context, sessionID string, discarded []sessionstore.Message, summary string)

	// LiveRequest, when set, returns the request the running agent would send
	// for this session: the same system prompt, the same pinned context, the
	// whole history and the tool catalogue. An in-band compaction appends its
	// instruction to that and lets the agent summarise itself.
	//
	// It wins on two things at once. The prefix is byte-identical to the turn
	// that just ran, so the whole conversation is a cache read rather than a
	// re-sent copy. And the model sees the conversation as it actually saw it,
	// rather than a serialization of it where every tool result, reasoning
	// trace and call's arguments are truncated a second time - which is the
	// difference between an Errors & Fixes section naming the error and one
	// guessing at it.
	//
	// Nil disables it, and every caller that has a runner sets it. The cold
	// summariser remains for the overflow trigger, which runs with a history the
	// provider would refuse, and for a cold prompt is smaller than the
	// conversation it stands for.
	LiveRequest func(context.Context, *sessionstore.Session) llm.StreamRequest

	// failureMu guards lastFailure, which the cooldown below reads.
	failureMu   sync.Mutex
	lastFailure time.Time
}

// summaryAttempts is how many in-band summarisation requests one compaction
// makes before falling back. Two, because the first failure is usually format
// rather than refusal and a re-ask is cheaper than the cold path, and because
// the request is a cache read the model has just paid to have open.
const summaryAttempts = 2

// summaryCooldown stops a summariser that keeps failing from being asked again
// on every turn.
//
// Without it, a model that never produces a valid summary leaves the session
// over the trigger forever, so each subsequent turn pays for compaction again
// and the only thing compaction buys it is the bill. The forced path - the
// Mini App's button - never waits, because an operator pressing it is entitled
// to the answer.
var summaryCooldown = 5 * time.Minute

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
// compactAt is the context size at which compaction begins.
//
// The earlier form subtracted a flat buffer from the limit, which combined with
// the margin the resolver also subtracted to trigger absurdly early on small
// windows: 12% of a 32k window, 80% of a 200k one. Neither figure was chosen;
// both were the same flat reservation counted twice.
//
// The trigger is now the lesser of a 90% fill and the point where the absolute
// reserve would be consumed. A large window therefore uses 90% of itself, and a
// window too small to hold the reserve still stops before the model's reply no
// longer fits.
func (c *Compactor) compactAt() int {
	window := c.contextLimit()
	if window <= 0 {
		return 0
	}
	proportional := window * CompactAtWindowPercent / 100
	absolute := window - c.reserve()
	if absolute < proportional {
		// A window smaller than the reserve cannot satisfy it. Triggering at the
		// raw difference would be negative, and every turn would then exceed it and
		// compact, forever, with nothing gained. Fall back to half the window,
		// which at least leaves the turn room to happen.
		if floor := window / 2; absolute < floor {
			return floor
		}
		return absolute
	}
	return proportional
}

// reserve is the headroom to keep, never less than the reply itself needs.
func (c *Compactor) reserve() int {
	reserve := c.Settings.Buffer
	if reserve < ReserveContextTokens {
		reserve = ReserveContextTokens
	}
	if output := c.outputReserve(); reserve < output {
		reserve = output
	}
	return reserve
}

func (c *Compactor) NeedsCompaction(sess *sessionstore.Session) bool {
	if !c.Settings.Auto {
		return false
	}
	// Estimate request size including system prompt overhead
	return estimatedContext(sess) > c.compactAt()
}

// IsOverflow checks like soraacode overflow.ts isOverflow
func (c *Compactor) IsOverflow(sess *sessionstore.Session) bool {
	if !c.Settings.Auto {
		return false
	}
	used := estimatedContext(sess)
	usable := c.contextLimit() - c.outputReserve()
	return used >= usable
}

// CompactAfterOverflow performs summarization and rewrites session
func (c *Compactor) Compact(ctx context.Context, sess *sessionstore.Session) (bool, error) {
	c.LastPromptTokens = 0
	if !c.attemptAllowed() {
		return false, nil
	}
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
	if len(headMessages) == 0 && previousSummary == "" {
		return false, nil
	}
	summaryOutput := c.summaryOutputTokens()
	text, err := c.summarise(ctx, sess, summaryOutput)
	if err != nil {
		c.noteSummaryFailure()
		return false, err
	}
	if text == "" {
		return false, nil
	}
	// Archive before the rewrite, while the head is still in hand. The summary is
	// the only surviving account of this stretch, and without the head there is no
	// way to check that account later.
	c.writeArchive(sess.ID, len(sess.Messages), text, headMessages)
	if c.Learn != nil {
		c.Learn(ctx, sess.ID, headMessages, text)
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
			Seq:     len(sess.Messages),
		},
	})
	newMessages = append(newMessages, recentMessages...)
	sess.Messages = newMessages
	return true, nil
}

// summarise produces a validated checkpoint from the agent's own request.
// It is the only summariser: there is no fallback, because a fallback that
// re-serializes the conversation is a second answer to the same question that
// quietly goes stale, and this product already paid for that lesson once.
func (c *Compactor) summarise(ctx context.Context, sess *sessionstore.Session, summaryOutput int) (string, error) {
	if c.LiveRequest == nil {
		return "", fmt.Errorf("compaction has no live request to summarise over")
	}
	return c.summariseInBand(ctx, sess, summaryOutput)
}

// summariseInBand asks the agent, on its own request, to summarise itself.
//
// The instruction is appended to the request the agent would have been sent and
// to nothing else: it is never written to the session. Writing it there would
// leave "produce a compaction" as the last thing the model reads on every later
// turn.
//
// A reply that calls a tool fails the attempt rather than being reported as a
// summary. Compaction runs outside the tool loop, so nothing here can execute
// the call, and an answer of [tool_use] is the model refusing to stop work,
// which is the one way this variant is expected to go wrong. The Messages API
// has no tool_choice "none": a value that maps to nil leaves tools in the
// request with no tool_choice, which every endpoint reads as "auto" and
// therefore permits. On this transport a re-ask is the only enforcement
// available.
func (c *Compactor) summariseInBand(ctx context.Context, sess *sessionstore.Session, summaryOutput int) (string, error) {
	// The instruction rides on a request the provider has just accepted, so the
	// only thing that can push it over the window is the instruction and the
	// summary's own output budget. When even that does not fit, usually on the
	// overflow trigger where the history is already over the line, the
	// compaction fails honestly: there is no smaller request left to try, and a
	// checkpoint must never be faked from a conversation that cannot be sent.
	// An overflowed session is recovered by deleting messages or starting over,
	// not by summarising.
	if c.contextLimit()-estimatedContext(sess)-EstimateTokens(compactionInstruction(1)) < summaryOutput {
		return "", fmt.Errorf("the context cannot carry a summary")
	}
	var lastErr error
	for attempt := 1; attempt <= summaryAttempts; attempt++ {
		request := c.LiveRequest(ctx, sess)
		request.Messages = append(append(make([]llm.Item, 0, len(request.Messages)+1), request.Messages...),
			llm.Item{Kind: "text", Role: llm.RoleUser, Text: compactionInstruction(attempt)})
		request.ToolChoice = "none"
		request.MaxTokens = summaryOutput
		// A summary is not deliberation, and a reply produced under thinking
		// would have to carry its thinking blocks back on the next request.
		request.Reason = ""
		text, calledTool, usage, err := c.streamSummary(ctx, request)
		c.LastPromptTokens = usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens
		if err != nil {
			return "", err
		}
		if calledTool {
			lastErr = fmt.Errorf("the model called a tool instead of summarising")
			continue
		}
		if err := validateSummary(text); err != nil {
			lastErr = err
			continue
		}
		return text, nil
	}
	return "", lastErr
}

// streamSummary sends one summarisation request and returns its text.
//
// A tool call in the reply is reported rather than executed. Nothing here can
// carry out work on the model's behalf, which is the property that makes it
// safe to send an agent a request asking it to stop working.
func (c *Compactor) streamSummary(ctx context.Context, request llm.StreamRequest) (string, bool, llm.UsageInfo, error) {
	var reply strings.Builder
	var usage llm.UsageInfo
	var calledTool bool
	client, err := c.client(ctx)
	if err != nil {
		return "", false, usage, err
	}
	err = client.Stream(ctx, request, func(delta llm.ProviderResponse) {
		if delta.Content != "" {
			reply.WriteString(delta.Content)
		}
		if len(delta.ToolCalls) > 0 {
			calledTool = true
		}
		if delta.Usage != nil {
			usage.Add(*delta.Usage)
			if c.OnUsage != nil {
				c.OnUsage(*delta.Usage)
			}
		}
	})
	if err != nil {
		return "", calledTool, usage, err
	}
	return reply.String(), calledTool, usage, nil
}

// client resolves the provider for this compaction. A compaction may run on a
// different model from the one producing the run, which is the whole reason
// ResolveModel exists.
func (c *Compactor) client(ctx context.Context) (*llm.StreamingClient, error) {
	modelConfig := c.LLM
	if c.ResolveModel != nil {
		resolved, err := c.ResolveModel(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve compaction provider: %w", err)
		}
		modelConfig = resolved
	}
	return llm.New(modelConfig), nil
}

func (c *Compactor) summaryOutputTokens() int {
	if c.MaxOutputTokens > 0 && c.MaxOutputTokens < SummaryOutputTokens {
		return c.MaxOutputTokens
	}
	return SummaryOutputTokens
}

// attemptAllowed reports whether a compaction may run now.
//
// The cooldown is on the whole compaction rather than on one path, so a session
// that cannot be summarised by either summariser waits a fixed five minutes
// instead of paying for it on every turn.
func (c *Compactor) attemptAllowed() bool {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	if c.lastFailure.IsZero() {
		return true
	}
	return time.Since(c.lastFailure) >= summaryCooldown
}

func (c *Compactor) noteSummaryFailure() {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	c.lastFailure = time.Now()
}

// ChainLearn returns one hook that runs both, in order.
//
// The Compactor carries a single Learn slot, and memory extraction already
// holds it. Rather than widen the field to a slice and touch every reader for
// a second consumer, each learner takes its turn through this. A nil hook is
// skipped, so a caller wires whichever features are configured without
// checking the other's state.
//
// Learn is called SYNCHRONOUSLY, inside the compaction that produced the
// checkpoint, before the rewritten session is assembled. A hook therefore
// delays the run that triggered compaction, and its cost is per compaction, not
// per turn. That was previously documented here as "off the hot path", which
// was wrong and cost a stalled run before it was noticed.
//
// Consequence for a caller adding a second learner: keep it cheap or hand it
// off to its own goroutine. Memory extraction is one model call and stays
// inline. The bond runs two and is dispatched asynchronously by its own hook.
//
// A hook that fails must not fail the turnover: each of these logs and returns.
// Order is memory then bond, because the bond reads the same checkpoint and
// neither one's result feeds the other.
func ChainLearn(first, second func(context.Context, string, []sessionstore.Message, string)) func(context.Context, string, []sessionstore.Message, string) {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(ctx context.Context, sessionID string, discarded []sessionstore.Message, summary string) {
		first(ctx, sessionID, discarded, summary)
		second(ctx, sessionID, discarded, summary)
	}
}

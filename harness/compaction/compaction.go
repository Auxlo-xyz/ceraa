package compaction

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ceraa/harness/llm"
	"ceraa/harness/redact"
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
	// Fifty five percent rather than something larger, because bounded folding
	// compacts little and often: an early trigger keeps each fold small and leaves
	// headroom for the reply, while a late trigger would force a large rewrite at
	// the point the model can least afford it.
	CompactAtWindowPercent = 55
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

// estimateMessage measures one message the way the receiving transport will.
//
// replayReasoning false excludes reasoning, because the Codex transport has no
// case for it and sends none of it. This is the same correction the context
// budget and the cost breakdown already got: with reasoning counted, a request
// measured about a fifth larger than it is, and every threshold derived from this
// figure moved with it.
func estimateMessage(m sessionstore.Message, replayReasoning bool) int {
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
	return n + 10 // overhead
}

func estimateSession(sess *sessionstore.Session, replayReasoning bool) int {
	total := 0
	for _, m := range sess.Messages {
		total += estimateMessage(m, replayReasoning)
	}
	return total
}

// EstimatedContext is the size of the context a session is carrying: the whole
// prompt the last turn sent, cached prefix included, plus whatever was appended
// since.
//
// It is exported because it is the figure the compaction trigger compares against
// the window, and the status card has to show that same number. Summing the text
// of every stored message instead reported a session as 137% of its window while
// the request the provider actually saw carried 87k, so the card alarmed at a
// threshold the trigger was never near.
func EstimatedContext(sess *sessionstore.Session) int {
	return EstimatedContextFor(sess, true)
}

// EstimatedContextFor is EstimatedContext measured as the named transport will
// carry it. The provider-reported path is unaffected either way, because it reads
// what the provider counted. Only the local fallback estimates change.
func EstimatedContextFor(sess *sessionstore.Session, replayReasoning bool) int {
	lastUsage := 0
	lastAssistant := -1
	for i, message := range sess.Messages {
		if message.Role != llm.RoleAssistant {
			continue
		}
		// ContextTokens, not TotalTokens: on a split-reporting endpoint the cached
		// prefix is reported beside the input, so TotalTokens carries only the
		// fresh tokens and a session holding tens of thousands reads as a few
		// thousand. That is how one passed 137% of its window while the trigger
		// still saw a figure under its threshold and stayed silent.
		if size := message.Usage.ContextTokens(); size > 0 {
			lastUsage = size
			lastAssistant = i
		}
	}
	if lastAssistant < 0 || lastUsage == 0 {
		return estimateSession(sess, replayReasoning)
	}
	trailing := 0
	for _, message := range sess.Messages[lastAssistant+1:] {
		trailing += estimateMessage(message, replayReasoning)
	}
	return lastUsage + trailing
}

type messageUnit struct {
	messages []sessionstore.Message
	tokens   int
}

func selectMessageUnits(messages []sessionstore.Message, keepTokens int, replayReasoning bool) (head []sessionstore.Message, recent []sessionstore.Message) {
	var units []messageUnit
	var current []sessionstore.Message
	// outstanding holds the ids of calls made inside the unit being built that
	// have not been answered yet. A unit is a whole exchange, not a run of
	// messages: an assistant turn and the results that answer it are one thing,
	// and a fold that keeps only half of one sends a tool_result whose tool_use
	// the provider never received, which the Messages API rejects outright.
	var outstanding map[string]bool
	flush := func() {
		if len(current) == 0 {
			return
		}
		tokens := 0
		for _, message := range current {
			tokens += estimateMessage(message, replayReasoning)
		}
		units = append(units, messageUnit{messages: current, tokens: tokens})
		current = nil
		outstanding = nil
	}
	for _, message := range messages {
		if message.Compaction != nil || message.Role == llm.RoleSystem {
			flush()
			continue
		}
		if len(outstanding) == 0 && message.Role == llm.RoleUser && len(current) > 0 {
			flush()
		}
		current = append(current, message)
		for _, call := range message.AllToolCalls() {
			if call.ID == "" {
				continue
			}
			if outstanding == nil {
				outstanding = make(map[string]bool)
			}
			outstanding[call.ID] = true
		}
		if message.Role == llm.RoleTool && message.ToolCallID != "" {
			delete(outstanding, message.ToolCallID)
		}
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

// FoldKeepRecentPercent is the share of the history that folding never touches.
// The newest forty percent of messages stay verbatim, so a compaction summarizes
// only the oldest sixty percent. The structural tail from selectMessageUnits (the
// open exchange and the one before it) is also kept, and the fold is the part of
// the head that is older than both guards.
const FoldKeepRecentPercent = 40

// maxFoldLedgerEntries caps the in memory fold ledger. Each entry is a few dozen
// bytes, so this is a count cap rather than a size cap: enough to show the recent
// folding pattern, small enough to stay out of the way.
const maxFoldLedgerEntries = 8

// FoldEntry records one bounded fold: which pre compact range it covered and when.
// Future compactions start after the latest checkpoint, so they extend this ledger
// rather than re summarizing a range it already covers. Merge means carrying the
// prior summary forward through the live request, not re folding its messages.
type FoldEntry struct {
	ID    string    `json:"id"`
	Start int       `json:"start"`
	End   int       `json:"end"`
	At    time.Time `json:"at"`
	// Tier and Generation mirror the checkpoint the fold wrote: tier 1 young
	// at creation, promoted and merged by the tier machinery below.
	Tier       int    `json:"tier"`
	Generation string `json:"generation"`
}

// Tier distillation mirrors billion-context's acp-kernel: folds land at tier
// 1, survivors promote to old, and old blocks of one tier merge into a single
// block of the next. Ancient history keeps shrinking instead of stacking.
// There is deliberately no age-based deletion: a merged summary still carries
// its span, while a deleted one carries nothing.
//
// One adaptation is forced: there, the model executes merges by calling
// compress over block ranges; here no compress tool exists, so the compactor
// executes a merge itself while it is already running. The count triggers
// keep their upstream defaults (effectively off); MergeMinOldGenBlocks is the
// live floor that makes merging actually fire during a compaction.
const TiersEnabled = true
const Tier2Trigger = 1000
const Tier3Trigger = 2000
const TierPromotionThreshold = 5
const MergeMaxSummaryLength = 20000
const MergeMinOldGenBlocks = 2

func flattenMessages(units []messageUnit) []sessionstore.Message {
	var messages []sessionstore.Message
	for _, unit := range units {
		messages = append(messages, unit.messages...)
	}
	return messages
}

// FoldLedger returns a copy of the fold ledger, oldest first.
func (c *Compactor) FoldLedger() []FoldEntry {
	c.foldMu.Lock()
	defer c.foldMu.Unlock()
	out := make([]FoldEntry, len(c.foldLedger))
	copy(out, c.foldLedger)
	return out
}

// recordFold appends one ledger entry and returns it. Callers record only after
// a validated summary has been written, so the ledger never names a range that
// was not folded.
func (c *Compactor) recordFold(start, end, tier int) FoldEntry {
	c.foldMu.Lock()
	defer c.foldMu.Unlock()
	if c.runSeed == 0 {
		c.runSeed = time.Now().UnixNano()
	}
	c.foldSeq++
	entry := FoldEntry{
		ID:         fmt.Sprintf("fold-%d-%d", c.runSeed, c.foldSeq),
		Start:      start,
		End:        end,
		At:         time.Now().UTC(),
		Tier:       tier,
		Generation: "young",
	}
	c.foldLedger = append(c.foldLedger, entry)
	if len(c.foldLedger) > maxFoldLedgerEntries {
		c.foldLedger = c.foldLedger[len(c.foldLedger)-maxFoldLedgerEntries:]
	}
	return entry
}

// tierOf reads a checkpoint's tier, treating pre-tier checkpoints as tier 1
// so nothing written before tiers needs migration.
func tierOf(info *sessionstore.CompactionInfo) int {
	if info == nil || info.Tier < 1 {
		return 1
	}
	if info.Tier > 3 {
		return 3
	}
	return info.Tier
}

// generationOf reads a checkpoint's generation, defaulting to young.
func generationOf(info *sessionstore.CompactionInfo) string {
	if info == nil || info.Generation == "" {
		return "young"
	}
	return info.Generation
}

// tickSurvivors promotes checkpoints that outlive compactions: every survivor
// gains one count per Compact call, and TierPromotionThreshold survivals flip
// it to old, which is what makes it eligible for merging. Upstream counts
// every turn; here compactions are the survival ticks, which is the closest
// this architecture gets to per-turn ageing.
func tickSurvivors(messages []sessionstore.Message) {
	for i := range messages {
		info := messages[i].Compaction
		if info == nil {
			continue
		}
		info.Survived++
		if info.Survived >= TierPromotionThreshold && generationOf(info) == "young" {
			info.Generation = "old"
		}
	}
}

// firstUserIndex returns the session's opening user message, or -1. The pin
// below keeps it verbatim no matter what range covers it.
func firstUserIndex(messages []sessionstore.Message) int {
	for i, message := range messages {
		if message.Role == llm.RoleUser {
			return i
		}
	}
	return -1
}

// splitFoldRange bounds one fold to the oldest slice of the head.
//
// totalCandidates is the number of candidate messages the head came from. The
// newest FoldKeepRecentPercent of those stay verbatim by age, and the head
// already excludes the structural tail, so the fold is the overlap: the part of
// the head that is also older than the age guard. What is left over (the middle)
// stays in place beside the recent tail. An empty fold means there is nothing
// old enough to summarize, which is how the newest messages survive.
func splitFoldRange(head []sessionstore.Message, totalCandidates int) (fold, rest []sessionstore.Message) {
	if len(head) == 0 || totalCandidates <= 0 {
		return nil, head
	}
	keep := (totalCandidates*FoldKeepRecentPercent + 99) / 100
	if keep < 1 {
		keep = 1
	}
	if keep > totalCandidates {
		keep = totalCandidates
	}
	ageFoldEnd := totalCandidates - keep
	if ageFoldEnd < 0 {
		ageFoldEnd = 0
	}
	n := len(head)
	if ageFoldEnd < n {
		n = ageFoldEnd
	}
	if n <= 0 {
		return nil, head
	}
	// Snap the cut to an exchange boundary. The arithmetic above only counts
	// messages, so n can land between an assistant turn that called tools and
	// the results answering them; splitting there folds the calls away and
	// leaves orphan tool_results behind, which the Messages API refuses with
	// "tool_use_id found in tool_result blocks". Walk back over any results
	// whose call is in the same exchange, so the fold ends on a whole one.
	n = snapToExchangeEnd(head, n)
	if n <= 0 {
		return nil, head
	}
	return head[:n], head[n:]
}

// snapToExchangeEnd moves a cut at index n backwards so it never splits an
// assistant turn from the results that answer its calls.
//
// head[:n] is folded and head[n:] stays. The cut is unsafe when the first
// message that stays is a tool result whose call was declared by a turn that
// is being folded: the calls go into the summary and the results stay behind,
// which is exactly the shape the Messages API rejects. The cut is pulled back
// to the turn that declared the call, so the whole exchange stays together.
//
// A cut that lands after an answered exchange, or before a message that made
// no calls, is already on a boundary and is returned unchanged.
func snapToExchangeEnd(head []sessionstore.Message, n int) int {
	if n <= 0 || n >= len(head) {
		return n
	}
	// Call id to the index of the turn that declared it. A tool result has no
	// back-pointer of its own, so the owner is resolved once over the head.
	owners := make(map[string]int, len(head))
	for i, message := range head {
		for _, call := range message.AllToolCalls() {
			if call.ID != "" {
				owners[call.ID] = i
			}
		}
	}
	// n only ever decreases, so this terminates.
	for n > 0 {
		first := head[n]
		if first.Role != llm.RoleTool || first.ToolCallID == "" {
			return n
		}
		owner, ok := owners[first.ToolCallID]
		if !ok || owner >= n {
			return n
		}
		n = owner
	}
	return n
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

	// replayReasoning is whether the transport that will carry a summary request
	// sends stored reasoning back, and replayReasoningSet records that the owner
	// has said. Unset means true, so a compactor nobody configured keeps
	// measuring the whole conversation.
	replayReasoning    bool
	replayReasoningSet bool
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

	// Carry reports the sidekick/memory ids that should survive this checkpoint.
	// Optional; nil carries nothing, which is correct for every caller with no sidekick.
	//
	// The judgement lives on the owner and not in here because the compactor does
	// not know what an id means. It cannot read the memory log and cannot tell
	// one delivered memory note from another; the owner holds the log tail the
	// score-free selection is read from. So the compactor asks who still holds
	// and carries the answer verbatim, and a fold never decides on its own which
	// notes are worth telling again.
	//
	// nil forwards previous unchanged. That is the safe direction - nothing
	// expires silently - and for a caller with no sidekick previous is empty, so
	// it is the same as carrying nothing.
	Carry func(sess *sessionstore.Session, previous []string) []string

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

	// foldMu guards the fold ledger below. It is separate from failureMu so a
	// reader inspecting past folds never blocks the cooldown check.
	foldMu     sync.Mutex
	foldLedger []FoldEntry
	foldSeq    int
	// runSeed makes fold ids unique across Compactor instances. A Compactor
	// is built per run, so a plain sequence restarts at fold-1 every run and
	// each run's retention file overwrites the last. Lazily minted on first
	// use; zero means unseeded, never a valid seed.
	runSeed int64
}

// MinFoldGrowthTokens is how much new content must arrive after the latest
// checkpoint before another fold may fire.
//
// The trigger metric counts the whole request, including costs folding can
// never remove: the system prompt, the tool catalogue, and fresh reasoning,
// which alone can sit over the 55% line. Without this gate a session past the
// line compacts on every message forever, writing a checkpoint per turn while
// the estimate never drops. Fifteen thousand tokens of genuinely new
// conversation is the rhythm: folds stay frequent enough to matter and can
// never spin turn after turn on the same history.
const MinFoldGrowthTokens = 15000

// growthSinceCheckpoint estimates the tokens arrived after the latest
// checkpoint message. A session with no checkpoint yet returns the whole
// session, so first folds are never gated.
func growthSinceCheckpoint(sess *sessionstore.Session, replayReasoning bool) int {
	latest := -1
	for i, message := range sess.Messages {
		if message.Compaction != nil {
			latest = i
		}
	}
	// No checkpoint yet means no fold could repeat: first folds stay ungated.
	if latest < 0 {
		return int(^uint(0) >> 1)
	}
	total := 0
	for _, message := range sess.Messages[latest+1:] {
		total += estimateMessage(message, replayReasoning)
	}
	return total
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
//
// The transport's answer is read here rather than pushed in by each caller,
// because every caller would have to remember and the failure is silent: a
// compactor that measures reasoning the transport drops folds history early, and
// early folds cost a summarisation request every time they fire.
func New(cfg llm.ProviderConfig) *Compactor {
	c := &Compactor{LLM: cfg, Settings: DefaultSettings(), ContextLimit: MaxTotalTokens, MaxOutputTokens: 20000}
	c.ReplaysReasoning(!llm.UsesCodexNative(cfg.BaseURL))
	return c
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
// The trigger is now the lesser of a 55% fill and the point where the absolute
// reserve would be consumed. A large window therefore uses 55% of itself, and a
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
	if c.EstimatedContext(sess) <= c.compactAt() {
		return false
	}
	// Growth gate: refuse to fold the same history twice. See MinFoldGrowthTokens.
	return c.growthSinceCheckpoint(sess) > MinFoldGrowthTokens
}

// EstimatedContext measures the session as the transport that will carry it will
// see it. Set by the runner from the same resolution the context budget uses.
func (c *Compactor) EstimatedContext(sess *sessionstore.Session) int {
	return EstimatedContextFor(sess, c.replaysReasoning())
}

// ReplaysReasoning records whether the transport sends stored reasoning back.
func (c *Compactor) ReplaysReasoning(replayed bool) {
	c.replayReasoning = replayed
	c.replayReasoningSet = true
}

func (c *Compactor) growthSinceCheckpoint(sess *sessionstore.Session) int {
	return growthSinceCheckpoint(sess, c.replaysReasoning())
}

func (c *Compactor) replaysReasoning() bool {
	if c.replayReasoningSet {
		return c.replayReasoning
	}
	return true
}

// IsOverflow checks like soraacode overflow.ts isOverflow
func (c *Compactor) IsOverflow(sess *sessionstore.Session) bool {
	if !c.Settings.Auto {
		return false
	}
	used := c.EstimatedContext(sess)
	usable := c.contextLimit() - c.outputReserve()
	return used >= usable
}

// CompactAfterOverflow performs summarization and rewrites session
//
// The rewrite is a bounded fold, not a whole history rewrite. Only the oldest
// portion of the candidate history is summarized: everything older than the
// newest forty percent by count, and never the structural tail that
// selectMessageUnits keeps. The checkpoint replaces the folded range at its own
// position, so the middle (too new to fold, too old to be the tail) and the
// recent tail stay where they were. The fold ledger records each covered range,
// and the next compaction starts after the latest checkpoint, so it extends the
// ledger rather than re summarizing a folded range. Prior summaries are carried
// forward because the fold scoped request includes them.
// mergeTier distills old blocks of one tier into a single block of the next.
// It fires on the count trigger (upstream default, effectively off) or on the
// live floor MergeMinOldGenBlocks. The merged summary goes through the same
// template and validation as a fold; a merge that fails is skipped without
// touching the fold, the cooldown, or the session, because merging is hygiene
// and must never endanger the compaction it rides on.
func (c *Compactor) mergeTier(ctx context.Context, sess *sessionstore.Session, fromTier, countTrigger int) {
	var indices []int
	for i := range sess.Messages {
		info := sess.Messages[i].Compaction
		if info == nil || tierOf(info) != fromTier || generationOf(info) != "old" {
			continue
		}
		indices = append(indices, i)
	}
	if len(indices) < countTrigger && len(indices) < MergeMinOldGenBlocks {
		return
	}
	c.mergeCheckpoints(ctx, sess, indices, fromTier+1)
}

// MaxCheckpointChain bounds how many checkpoints stand ahead of the live turns
// before they are collapsed into one.
const MaxCheckpointChain = 6

// collapseChain rolls the standing checkpoint chain into a single checkpoint
// once it passes MaxCheckpointChain. It reports whether it rewrote anything.
//
// The chain is append-only by design: each fold adds a checkpoint and leaves the
// ones behind it standing, so the bytes ahead of the newest checkpoint never move
// and the provider keeps reading that whole stretch warm. That is the right
// trade while the chain is short.
//
// Past the cap it stops being free. The chain is a growing share of the request,
// it is history the model has been told more than once, and every later fold
// re-summarizes it. So the chain is collapsed in one operation: exactly one full
// prefix miss, paid here instead of spread across every future turn, and
// append-only again afterwards. That is the epoch roll.
//
// It runs inside Compact, ahead of the fold, so a Compact that both rolls and
// folds still produces a single request and therefore a single rewrite on the
// wire: the two rewrites of the in-memory slice cost one cache invalidation
// between them.
func (c *Compactor) collapseChain(ctx context.Context, sess *sessionstore.Session) bool {
	indices := make([]int, 0, len(sess.Messages))
	for i := range sess.Messages {
		if sess.Messages[i].Compaction != nil {
			indices = append(indices, i)
		}
	}
	if len(indices) <= MaxCheckpointChain {
		return false
	}
	c.mergeCheckpoints(ctx, sess, indices, 1)
	return true
}

// mergeCheckpoints rewrites the checkpoints at indices into one, placed at the
// position of the first. It is the body both mergeTier and collapseChain share.
//
// A merge that fails is a no-op: the summaries it would have carried are still
// standing, and nothing about the fold that follows depends on it having run.
func (c *Compactor) mergeCheckpoints(ctx context.Context, sess *sessionstore.Session, indices []int, nextTier int) {
	if len(indices) == 0 {
		return
	}
	var parts []string
	// The delivered ids come off the consumed blocks alongside their summaries,
	// and this is the one place in the package where they can be dropped without
	// anything noticing. A merge is a mechanical rewrite of N checkpoints into
	// one; the fold that follows calls Carry over the union, but the fold is not
	// guaranteed - it aborts on a summariser failure and on an empty fold range,
	// and the merge has already rewritten the messages by then. So the union is
	// written here and now: an id that is only on a block this merge consumed has
	// no other copy in the session, and losing it is the silent version of the
	// bug this field exists to prevent.
	var delivered []string
	for _, i := range indices {
		parts = append(parts, sess.Messages[i].Compaction.Summary)
		delivered = appendUnique(delivered, sess.Messages[i].Compaction.Delivered...)
	}
	joined := strings.Join(parts, "\n\n---\n\n")
	if len(joined) > MergeMaxSummaryLength {
		joined = joined[len(joined)-MergeMaxSummaryLength:]
	}
	mergeSession := &sessionstore.Session{ID: sess.ID}
	mergeSession.Messages = append(mergeSession.Messages, sessionstore.Message{
		Role:    llm.RoleUser,
		Content: joined,
	})
	text, err := c.summarise(ctx, mergeSession, c.summaryOutputTokens())
	if err != nil || text == "" {
		return
	}
	merged := sessionstore.Message{
		Role:    llm.RoleSystem,
		Content: "<conversation-checkpoint>",
		Compaction: &sessionstore.CompactionInfo{
			Summary:    text,
			Seq:        len(sess.Messages),
			Tier:       nextTier,
			Generation: "young",
			Delivered:  delivered,
		},
	}
	first := indices[0]
	kept := sess.Messages[:0:0]
	for i, message := range sess.Messages {
		consumed := false
		for _, j := range indices {
			if i == j {
				consumed = true
				break
			}
		}
		if !consumed {
			kept = append(kept, message)
		}
	}
	out := append(kept[:first], append([]sessionstore.Message{merged}, kept[first:]...)...)
	// A merge only removes checkpoint markers, but a checkpoint can stand between
	// a turn and its results after an earlier fold cut through one. Sweep here for
	// the same reason the fold does: whatever produced the pairing, the request
	// is refused while it is broken.
	sess.Messages = dropOrphanToolResults(out)
	// A merge rewrites the front of the prompt, so it counts as a prefix rewrite
	// for the same reason a fold does: the next request re-bills everything after
	// the position it moved.
	sess.PrefixRewrites++
	entry := c.recordFold(first, first+1, nextTier)
	if c.ArchiveDir != "" {
		sources := make([]FoldMessage, 0, len(parts))
		for _, summary := range parts {
			text := redact.SecretsInText(summary)
			sources = append(sources, FoldMessage{Role: "system", Text: text})
		}
		_, _ = RetainFold(filepath.Join(c.ArchiveDir, "folds"), entry.ID, sources)
	}
}

// dropOrphanToolResults removes any tool result whose call is not on the wire.
//
// A tool_result names the tool_use that produced it, and an endpoint that
// validates the pairing refuses the whole request when the name has nothing to
// match: "tool_use_id found in tool_result blocks". Every rewrite of the history
// is a chance to produce one, so each runs through here rather than trusting
// the cut that preceded it. It is also what repairs a session already damaged
// on disk, which folding alone cannot do: the bad pair is stored, not cut.
//
// A result is kept only when the assistant turn declaring its call is present
// and stands earlier in the list. Everything else is dropped, because the
// alternative - keeping it and being refused - costs the turn either way.
func dropOrphanToolResults(messages []sessionstore.Message) []sessionstore.Message {
	declared := make(map[string]bool)
	out := make([]sessionstore.Message, 0, len(messages))
	for _, message := range messages {
		isResult := message.Role == llm.RoleTool && message.ToolCallID != ""
		if isResult && !declared[message.ToolCallID] {
			continue
		}
		out = append(out, message)
		for _, call := range message.AllToolCalls() {
			if call.ID != "" {
				declared[call.ID] = true
			}
		}
	}
	return out
}

func (c *Compactor) Compact(ctx context.Context, sess *sessionstore.Session) (bool, error) {
	c.LastPromptTokens = 0
	if !c.attemptAllowed() {
		return false, nil
	}
	// Survival ticks first: every checkpoint still standing gains one, and
	// five survivals promote it to old age and merge eligibility.
	tickSurvivors(sess.Messages)
	// Tier distillation before the new fold: old blocks of one tier merge
	// into a single block of the next, so ancient history shrinks instead of
	// stacking one checkpoint per fold forever.
	if TiersEnabled {
		c.mergeTier(ctx, sess, 1, Tier2Trigger)
		c.mergeTier(ctx, sess, 2, Tier3Trigger)
	}
	// The epoch roll, ahead of the fold so that a Compact which does both still
	// produces one request and therefore one rewrite on the wire.
	c.collapseChain(ctx, sess)
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
	// What the standing checkpoints say the model has been told, read here while
	// the tool results carrying those notes are still on the wire. The same walk
	// that read previousSummary reads this, because a set kept anywhere else is
	// a second answer to the same question that goes stale quietly.
	previousDelivered := sess.CheckpointDelivered()
	candidate := sess.Messages
	offset := 0
	if latestCompaction >= 0 {
		offset = latestCompaction + 1
		candidate = sess.Messages[offset:]
	}
	headMessages, recentMessages := selectMessageUnits(candidate, keepTokens, c.replaysReasoning())
	if len(headMessages) == 0 && previousSummary == "" {
		return false, nil
	}
	foldMessages, middleMessages := splitFoldRange(headMessages, len(candidate))
	if len(foldMessages) == 0 {
		return false, nil
	}
	// The ledger range is the pre compact span the fold covered, from the first
	// folded message through the last one. System and checkpoint markers occupy
	// candidate slots but are never folded, so they are skipped while walking.
	start := offset
	end := offset
	next := 0
	for i, message := range candidate {
		if message.Compaction != nil || message.Role == llm.RoleSystem {
			continue
		}
		if next < len(foldMessages) {
			if next == 0 {
				start = offset + i
			}
			end = offset + i + 1
			next++
		}
	}
	// Summarize the fold, not the whole history. The prior summary rides along
	// so its unresolved items are carried forward into the new checkpoint.
	foldSession := &sessionstore.Session{ID: sess.ID}
	if previousSummary != "" {
		foldSession.Messages = append(foldSession.Messages, sessionstore.Message{
			Role:    llm.RoleSystem,
			Content: "<conversation-checkpoint>",
			Compaction: &sessionstore.CompactionInfo{
				Summary: previousSummary,
				Seq:     latestCompaction,
			},
		})
	}
	foldSession.Messages = append(foldSession.Messages, foldMessages...)
	summaryOutput := c.summaryOutputTokens()
	text, err := c.summarise(ctx, foldSession, summaryOutput)
	if err != nil {
		c.noteSummaryFailure()
		return false, err
	}
	if text == "" {
		return false, nil
	}
	// Archive before the rewrite, while the fold is still in hand. The summary is
	// the only surviving account of this stretch, and without the fold there is no
	// way to check that account later.
	c.writeArchive(sess.ID, len(sess.Messages), text, foldMessages)
	if c.Learn != nil {
		c.Learn(ctx, sess.ID, foldMessages, text)
	}
	var newMessages []sessionstore.Message
	if offset > 0 {
		newMessages = append(newMessages, sess.Messages[:offset]...)
	} else if systemMessage != nil {
		newMessages = append(newMessages, *systemMessage)
	}
	newMessages = append(newMessages, sessionstore.Message{
		Role:    llm.RoleSystem,
		Content: "<conversation-checkpoint>",
		Compaction: &sessionstore.CompactionInfo{
			Summary:    text,
			Seq:        len(sess.Messages),
			Tier:       1,
			Generation: "young",
			Delivered:  c.carry(sess, previousDelivered),
		},
	})
	// First-user-message pin: the session's opening user message survives the
	// fold verbatim even when the folded range covers it. The summary above
	// still describes the whole span including it; the raw stays right after
	// the checkpoint so the wire keeps a user message no range can strip.
	pinned := firstUserIndex(sess.Messages)
	if pinned >= start && pinned < end {
		newMessages = append(newMessages, sess.Messages[pinned])
	}
	newMessages = append(newMessages, middleMessages...)
	newMessages = append(newMessages, recentMessages...)
	newMessages = dropOrphanToolResults(newMessages)
	sess.Messages = newMessages
	// A fold is the one rewrite an epoch pays for: everything from the checkpoint
	// onward is re-billed at full price on the next request, which is why the
	// pointerize boundary is tied to the checkpoint rather than to the turn.
	sess.PrefixRewrites++
	entry := c.recordFold(start, end, 1)
	// Retain the fold source beside the archive: the summary is lossy, and
	// without the original messages no later turn can decompress or verify
	// it. Best-effort by design; a retention failure must not fail the fold.
	if c.ArchiveDir != "" {
		sources := make([]FoldMessage, 0, len(foldMessages))
		for _, message := range foldMessages {
			text := message.Content
			if message.ToolResult != "" {
				text += "\n[tool result]\n" + message.ToolResult
			}
			text = redact.SecretsInText(text)
			sources = append(sources, FoldMessage{Role: string(message.Role), Text: text})
		}
		_, _ = RetainFold(filepath.Join(c.ArchiveDir, "folds"), entry.ID, sources)
	}
	return true, nil
}

// carry asks the owner which delivered ids this checkpoint keeps.
//
// It is asked here, at the instant the fold is written, while the discarded
// head is still on the wire and the session's own delivered list still stands:
// that is the only moment at which "what was the model told, and does it still
// hold" is answerable at all. Afterwards the fold has already deleted the tool
// results that were the only copies, and an owner asking then would be reading
// its own guess back.
func (c *Compactor) carry(sess *sessionstore.Session, previous []string) []string {
	if c.Carry == nil {
		return previous
	}
	return c.Carry(sess, previous)
}

// appendUnique appends ids, dropping blanks and repeats in first-seen order, so
// the same input always writes the same set to the session file.
func appendUnique(dst []string, ids ...string) []string {
	seen := make(map[string]bool, len(dst)+len(ids))
	for _, id := range dst {
		seen[id] = true
	}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		dst = append(dst, id)
	}
	return dst
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
	if c.contextLimit()-c.EstimatedContext(sess)-EstimateTokens(compactionInstruction(1)) < summaryOutput {
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
//
// It does not re-read the transport for the flag. A resolution that swapped
// transports mid-run would have the trigger and the request disagreeing, which
// is worse than being one provider stale; the owner sets ReplaysReasoning from
// the provider it configured the compactor with.
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
// inline; the log writer is cheap and stays inline.
//
// A hook that fails must not fail the turnover: each of these logs and returns.
// Order is memory extraction and then the memory-log summary, because the log
// reads the same checkpoint and neither one's result feeds the other.
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

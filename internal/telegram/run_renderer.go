package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"strings"
	"sync"
	"time"

	"ceraa/harness/operation"
	"ceraa/harness/redact"
	"ceraa/internal/logger"
	"ceraa/internal/telegram/keyboard"
)

type toolActivity struct {
	id string
	// name is the tool as the harness named it, so the line can carry the same
	// icon and the same word Soraa Code shows. label is the sentence the harness
	// wrote ("Read file", "Run command"), which is what the call is matched on.
	name   string
	status string
	label  string
	detail string
	// startedAt is when this call first appeared. A tool that blocks without
	// printing anything emits no events at all, so elapsed time has to be read
	// off a clock rather than off the event stream.
	startedAt time.Time
	// shownBucket is the bucketed duration already on screen, in milliseconds,
	// and -1 until the first one is shown. The live line is only re-rendered
	// when the bucket actually changes, so a running tool costs an edit when the
	// displayed number would change and not before.
	shownBucket int64
	elapsed     string
}

type runRenderer struct {
	token  string
	chatID int64
	// started is when the run began. The outbound throttle widens with the age
	// of the run rather than staying flat, so a long run slows its own cadence
	// down instead of walking into Telegram's rate limit.
	started time.Time
	// throttleOverride replaces the age-based outbound floor. Production leaves it
	// zero and gets streamThrottle(runAge). It exists because the floor is a whole
	// second in the first minute of a run, and a test that needs two signals
	// through had no option but to sleep past it: five tests spent about a second
	// each waiting for a timer rather than for behaviour. A test sets it to
	// something small and asserts on what was sent, not on how long it took.
	throttleOverride time.Duration

	mu sync.Mutex
	// messageID is the streamed answer. It carries the reply keyboard and is
	// edited down to the final answer when the run completes.
	messageID int
	// toolMessageIDs is one message per tool call, keyed by call id, and
	// toolSent is the text last accepted on each. One message for the whole run
	// made every call a line in a list that grew into a wall, and reusing that
	// one message across calls meant the record of what the run did was the last
	// call only. The reference maps entry index to message id for the same
	// reason: an entry is a message.
	toolMessageIDs map[string]int
	toolSent       map[string]string
	// thoughtID is the current thinking block's message, and thoughtSent is what
	// the API accepted on it. A run reasons in several blocks, and each is its own
	// message rather than one message redrawn for the whole run.
	thoughtID     int
	thoughtSent   string
	streamContent string
	thought       string
	tools         []toolActivity
	lastStatus    time.Time
	// Last text accepted by the API for each message, used to skip no-op edits.
	//
	// The stream loop re-sends the whole accumulated buffer on every signal,
	// because Telegram has no append. Without this, any signal that did not change
	// the rendered text still PUT the entire buffer, and Telegram answered
	// "message is not modified". Live cost: 404 such rejections, and a run that
	// ended in 429 retry after 227s because a single 3k answer was re-sent around
	// thirty times.
	//
	// sentStreamID is the message that text was accepted on. The comparison has
	// to be per message: the preview replacement swaps messageID underneath the
	// buffer, and a text-only comparison would take the new message's seeded
	// content as already delivered. That case used to be settled by timing,
	// which made TestReasoningDoesNotResendThePreviewItJustCreated pass on a
	// race rather than on the invariant.
	sentStream   string
	sentStreamID int
	sentThought  string
	lastSignal   time.Time
	// lastThoughtPush is when the thought block was last redrawn. It exists
	// separately from the redraw floor because the two count different things:
	// the floor paces how often a block is redrawn, and this one
	// paces what the reader is shown.
	lastThoughtPush time.Time
	stopped         bool
	cancel          context.CancelFunc
	streamSignal    chan struct{}
	streamDone      chan struct{}
	streamMu        sync.Mutex
	streamClosed    bool
	// richEditUnavailable latches once this run has been refused a rich edit, so
	// the remaining ticks take the plain path without paying for a round trip
	// that is known to come back 400.
	richEditUnavailable bool
}

// truncateStatus shortens a status line to fit a button label.
//
// It lived in pinned.go, which this change removed: it was the one symbol there
// still in use, so deleting the file would not have compiled and moving it next to
// its single caller is where a reader will look for it.
func truncateStatus(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

func newRunRenderer(token string, chatID int64) *runRenderer {
	renderer := &runRenderer{
		token:        token,
		chatID:       chatID,
		started:      time.Now(),
		streamSignal: make(chan struct{}, 1),
		streamDone:   make(chan struct{}),
	}
	// No message is created here. The assistant message is created by
	// ensureMessageLocked the first time there is something to put in it: the first
	// reasoning, the first content, or the final answer.
	//
	// It used to be created up front carrying a bare cursor, so that the reply
	// keyboard could be attached before the run produced anything. That put an
	// empty cursor bubble at the top of every run, visible in a screenshot as a
	// lone cursor sitting between the prompt and the thought. The keyboard is
	// attached on the same call that creates the message, and a run that produces
	// nothing gets no bubble at all.
	go renderer.runStream()
	go renderer.runToolTicker()
	return renderer
}

// ensureMessageLocked creates the assistant message once, and attaches the reply
// keyboard to it.
//
// The rich branch is the one production takes: the endpoint works, so the plain
// fallback is dead in practice. Attaching the keyboard only to the fallback is why
// a prompt finished with no keyboard while /status, which goes down the plain
// path, looked correct. Both branches carry it.
//
// editMessageReplyMarkup is documented for inline keyboards and answers
// "inline keyboard expected" for a reply keyboard on a minority of calls (122
// recorded against 2220 successes). This is the one place where a refusal cannot
// be recovered later: the run would stay keyboardless for its whole life. One
// bounded second attempt, then give up and say which end failed so a log can tell
// this from the final-answer attach.
//
// Callers must hold r.mu.
func (r *runRenderer) ensureMessageLocked() int {
	if r.messageID != 0 {
		return r.messageID
	}
	// Never create a message with nothing in it. The cursor that used to stand in
	// for absent text is gone, so an empty seed has to be refused here rather than
	// posted as a bubble containing nothing.
	seed := strings.TrimSpace(r.streamContent)
	if seed == "" {
		return 0
	}
	if messageID, err := sendRichMessage(r.token, r.chatID, seed); err == nil {
		r.messageID = messageID
		if err := attachReplyKeyboard(r.token, r.chatID, messageID); err != nil {
			if retryErr := attachReplyKeyboard(r.token, r.chatID, messageID); retryErr != nil {
				logger.Warn("reply keyboard attach failed at run start: " + retryErr.Error())
			}
		}
		return messageID
	}
	// Carry the reply keyboard from creation. Every later step of the run only
	// edits this message, and an edit that omits reply_markup leaves the existing
	// one in place, so this one call covers the streamed text and the final answer.
	r.messageID = sendWithKeyboard(r.token, r.chatID, seed, keyboard.Main())
	return r.messageID
}

func (r *runRenderer) runStream() {
	defer close(r.streamDone)
	// A final render once the channel is closed.
	//
	// Pacing means the last state change can have had its signal dropped: a tool
	// that completes inside the interval never gets a signal of its own, and the
	// queued one may already have been consumed against the earlier state. Without
	// this the live preview can show a tool as still running right up to the final
	// answer, which is the only place the completed state would otherwise appear.
	defer r.renderCurrentState()
	for range r.streamSignal {
		r.renderCurrentState()
	}
}

// renderCurrentState pushes the thought, the tool trail and the answer, skipping
// each when the API already has it.
//
// Shared by the loop and the final render so the three cannot drift on what
// counts as changed. Each keeps its own message and its own last-accepted text,
// so a tool line moving costs one edit of one message and leaves the answer the
// reader is watching alone.
func (r *runRenderer) renderCurrentState() {
	r.renderThought()
	r.renderTools()
	r.renderStream()
}

func (r *runRenderer) renderThought() {
	r.mu.Lock()
	thoughtID := r.thoughtID
	thoughtText := r.thoughtTextLocked()
	skip := thoughtID != 0 && thoughtText == r.sentThought
	lastThoughtPush := r.lastThoughtPush
	age := r.runAge()
	r.mu.Unlock()
	if thoughtID == 0 || thoughtText == "" || skip || time.Since(lastThoughtPush) < r.throttleAt(age) {
		return
	}
	if err := editTelegramHTMLMessage(r.token, r.chatID, thoughtID, thoughtText); err != nil {
		if fallbackErr := editTelegramMessage(r.token, r.chatID, thoughtID, thoughtFallbackText(thoughtText)); fallbackErr != nil {
			logger.Warn("Telegram thought preview fallback failed: " + fallbackErr.Error())
		}
	}
	r.mu.Lock()
	// Both values move together: sentThought keeps the next signal from spending
	// itself on identical text, and lastThoughtPush keeps a busy answer from
	// redrawing the thought faster than a reader can follow it. A signal refused
	// by the floor leaves sentThought alone, so the newer accumulated text goes
	// out on the next one instead of being lost.
	r.sentThought = thoughtText
	r.lastThoughtPush = time.Now()
	r.mu.Unlock()
}

// renderTools gives every tool call a message of its own.
//
// It used to be one message for the whole run, with each call a line inside it.
// That made a twenty minute run a single unreadable wall, and it meant the chat
// held no record of individual calls once the last one overwrote the text. The
// reference maps each entry to its own message id for the same reason.
//
// A message is created when the call first reports, edited in place while the
// call runs so the elapsed time ticks on the message it belongs to, and left
// alone once the call is over. That is why a running line updates rather than
// appends, and why the duration can be frozen onto a message that is not going
// to be edited again.
func (r *runRenderer) renderTools() {
	r.mu.Lock()
	type pending struct {
		callID  string
		text    string
		message int
		sent    string
	}
	work := make([]pending, 0, len(r.tools))
	for _, tool := range r.tools {
		text := formatToolActivity([]toolActivity{tool})
		if text == "" {
			continue
		}
		message := r.toolMessageIDs[tool.id]
		sent := r.toolSent[tool.id]
		if message != 0 && text == sent {
			continue
		}
		work = append(work, pending{callID: tool.id, text: text, message: message, sent: sent})
	}
	r.mu.Unlock()

	for _, item := range work {
		if item.message == 0 {
			id, err := sendRichMessage(r.token, r.chatID, item.text)
			if err != nil {
				id = sendWithKeyboard(r.token, r.chatID, item.text, nil)
			}
			if id == 0 {
				logger.Warn("Telegram tool message could not be created")
				continue
			}
			r.mu.Lock()
			if r.toolMessageIDs == nil {
				r.toolMessageIDs = make(map[string]int)
				r.toolSent = make(map[string]string)
			}
			r.toolMessageIDs[item.callID] = id
			r.toolSent[item.callID] = item.text
			r.mu.Unlock()
			continue
		}
		if err := r.editRichOrUnavailable(item.message, item.text); err != nil {
			if fallbackErr := editTelegramMessage(r.token, r.chatID, item.message, item.text); fallbackErr != nil {
				logger.Warn("Telegram tool message fallback failed: " + fallbackErr.Error())
				continue
			}
		}
		// Recorded only after the API accepted the text, so a rejected edit leaves
		// the last known good value in place and the next signal retries it.
		r.mu.Lock()
		r.toolSent[item.callID] = item.text
		r.mu.Unlock()
	}
}

func (r *runRenderer) renderStream() {
	r.mu.Lock()
	// Nothing to draw yet means nothing to create. An empty message is not worth
	// posting, which is what the eager message at run start was doing.
	if strings.TrimSpace(r.streamContent) == "" && r.sentStream == "" {
		r.mu.Unlock()
		return
	}
	messageID := r.ensureMessageLocked()
	text := r.streamTextLocked()
	if messageID != 0 && r.sentStreamID != messageID {
		// The message was just created carrying exactly this text, so there is
		// nothing to edit: recording it as delivered and returning is the only
		// correct reading. It used to get away with falling through because the
		// preview appended a cursor the creation seed did not, so the two always
		// differed and the redundant edit looked like a real update. Telegram
		// answered "message is not modified" to the copy.
		r.sentStream = text
		r.sentStreamID = messageID
		r.mu.Unlock()
		return
	}
	skip := messageID == r.sentStreamID && text == r.sentStream
	r.mu.Unlock()
	if messageID == 0 || skip {
		return
	}
	if err := r.editRichOrUnavailable(messageID, text); err != nil {
		if fallbackErr := editTelegramMessage(r.token, r.chatID, messageID, text); fallbackErr != nil {
			logger.Warn("Telegram streaming preview fallback failed: " + fallbackErr.Error())
			return
		}
	}
	// Recorded only after the API accepted the text. A rejected edit leaves the
	// last-known-good value in place, so the next signal retries it instead of
	// treating the message as already up to date. The id is recorded with it:
	// what was accepted was accepted on this message, and no other.
	r.mu.Lock()
	r.sentStream = text
	r.sentStreamID = messageID
	r.mu.Unlock()
}

// streamMinInterval is the floor between two outbound stream edits in the first
// minute of a run.
//
// The three producers used to pace themselves independently: content at 350ms,
// reasoning at 300ms, and tool with no limit at all. A burst of tool results
// therefore signalled on every single event, and each signal cost a full-buffer
// edit. Coalescing here, at the one point every producer funnels through, bounds
// the rate regardless of which producer is busy.
//
// Dropping a signal is safe: streamContent keeps accumulating, so the next one
// that is let through renders the newer text rather than losing it. The final
// answer does not go through here at all, so a dropped tail is always superseded
// by the real answer.
const streamMinInterval = time.Second

// thoughtMinInterval is the floor between two redraws of the thought block, and
// is the first tier of the same ladder the stream paces itself on.
//
// The thought used to ride the stream signal, so it was redrawn as often as the
// answer even while the reasoning itself barely moved. Every redraw is a full
// replacement of a block the reader may be in the middle of, so the thought now
// paces itself, and a signal inside that window is left alone rather than spent,
// because the accumulated text is still there for the next one.
const thoughtMinInterval = time.Second

// streamThrottle is the floor between two outbound edits for a run of the given
// age.
//
// A flat floor is right for the opening minute and wrong for everything after
// it: the recorded incident ended in a 429 with retry after 227s, and a run that
// has been going for twenty minutes is a run Telegram has been receiving
// full-buffer PUTs from for twenty minutes. Widening the floor with the age of
// the run lets a long run slow its own cadence down instead of borrowing the
// budget from its next edit.
//
// The ladder and its tier lengths are Soraa Code's, which is where this shape
// came from. Every tier is at least thoughtMinInterval, so the once-a-second
// floor the thought is pinned to still holds.
// throttleAt is the outbound floor for this run, honouring the test override.
func (r *runRenderer) throttleAt(age time.Duration) time.Duration {
	if r.throttleOverride > 0 {
		return r.throttleOverride
	}
	return streamThrottle(age)
}

func streamThrottle(age time.Duration) time.Duration {
	switch {
	case age < time.Minute:
		return streamMinInterval
	case age < 5*time.Minute:
		return 2 * time.Second
	case age < 15*time.Minute:
		return 5 * time.Second
	default:
		return 10 * time.Second
	}
}

// runAge reports how long this run has been going. started is written once,
// before either goroutine starts, so reading it needs no lock. A renderer built
// by a test as a struct literal has no start time, and its age is reported as
// zero rather than as the age of the Unix epoch, which would pin it to the
// slowest tier for no reason.
func (r *runRenderer) runAge() time.Duration {
	if r.started.IsZero() {
		return 0
	}
	return time.Since(r.started)
}

// Tool elapsed timing, ported from Soraa Code's RunningToolTracker and
// duration-formatter.
//
// A tool that blocks without printing anything emits no events at all, so a live
// line driven only by the event stream sits frozen with nothing to say whether
// the run had stalled or the tool was simply still working. Elapsed time has to
// come from an own ticker, never from incoming events.
var (
	// toolElapsedThreshold is how long a call runs before any duration appears.
	// Below it the line reads exactly as it did before, so a call that finishes
	// in under twenty seconds never looks like it was waiting on anything.
	toolElapsedThreshold = 20 * time.Second
	// toolElapsedTick is how often the ticker looks. It is slower than a human
	// expects, on purpose: the displayed number is bucketed below, so a faster
	// tick would only spend API calls re-sending a value that has not moved.
	toolElapsedTick = 10 * time.Second
	// toolElapsedMax stops tracking. Past this the line reads ">2h" and stays
	// there, rather than growing a digit nobody reads.
	toolElapsedMax = 2 * time.Hour
)

func (r *runRenderer) runToolTicker() {
	for {
		interval := toolElapsedTick
		if interval <= 0 {
			interval = time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-r.streamDone:
			timer.Stop()
			return
		case <-timer.C:
			if r.tickToolElapsed() {
				r.signalStream()
			}
		}
	}
}

// tickToolElapsed advances the duration on every running line whose displayed
// number would change, and reports whether a duration is on screen to publish.
//
// It reports on every look rather than only on a change. The first look can
// land inside the throttle window, and a change swallowed there would otherwise
// never reach the message: the next look finds nothing left to change, so it
// would have nothing to announce, and the number would sit in memory while the
// chat showed nothing. renderCurrentState skips text the API already holds, so
// a repeated signal costs no call.
func (r *runRenderer) tickToolElapsed() bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.tools {
		tool := &r.tools[i]
		if tool.status != "🔄" && tool.status != "⚡" {
			continue
		}
		elapsed := now.Sub(tool.startedAt)
		if elapsed < toolElapsedThreshold {
			continue
		}
		bucket := int64(-2)
		if elapsed < toolElapsedMax {
			bucket = int64(bucketElapsed(elapsed) / time.Millisecond)
		}
		if bucket != tool.shownBucket {
			tool.shownBucket = bucket
			if bucket == -2 {
				tool.elapsed = ">" + formatElapsed(toolElapsedMax)
			} else {
				tool.elapsed = formatElapsed(time.Duration(bucket) * time.Millisecond)
			}
		}
		return tool.elapsed != ""
	}
	return false
}

// bucketElapsed rounds a duration down to a display step that grows with the
// value. The live line is rendered from the bucketed number, so it is only
// edited when the shown number actually changes.
func bucketElapsed(elapsed time.Duration) time.Duration {
	if elapsed <= 0 {
		return 0
	}
	switch {
	case elapsed < time.Minute:
		return elapsed / (10 * time.Second) * (10 * time.Second)
	case elapsed < 10*time.Minute:
		return elapsed / time.Minute * time.Minute
	case elapsed < time.Hour:
		return elapsed / (5 * time.Minute) * (5 * time.Minute)
	default:
		return elapsed / (15 * time.Minute) * (15 * time.Minute)
	}
}

// formatElapsed renders a duration as "1h 2m 3s", dropping the parts that are
// zero. A zero duration reads as "0s" rather than as an empty string so a line
// that has just crossed the threshold says something.
func formatElapsed(elapsed time.Duration) string {
	total := int64(elapsed / time.Second)
	if total < 0 {
		total = 0
	}
	parts := make([]string, 0, 3)
	if hours := total / 3600; hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes := (total % 3600) / 60; minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds := total % 60; seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts, " ")
}

func (r *runRenderer) signalStream() {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if r.streamClosed {
		return
	}
	if time.Since(r.lastSignal) < r.throttleAt(r.runAge()) {
		return
	}
	r.lastSignal = time.Now()
	select {
	case r.streamSignal <- struct{}{}:
	default:
	}
}

// streamTextLocked is the answer preview, and nothing else. The tool trail
// used to ride in front of it in the same message, which meant every tool line
// redraw re-PUT the answer and the final answer had to shed the trail on the
// way out.
func (r *runRenderer) streamTextLocked() string {
	// No cursor. Telegram already draws a typing indicator on the bot's own
	// message while it is being edited, so a block cursor appended to the text
	// showed up twice, and when the run ended without publishing it was left
	// standing in the message as a stray "▌" on a line of its own. The reference
	// sends plain text and relies on the indicator too.
	content := strings.TrimSpace(r.streamContent)
	if content == "" {
		return ""
	}
	return truncateStream(content)
}

// toolTextLocked is the tool trail, empty while no tool has reported.
func (r *runRenderer) toolTextLocked() string {
	if len(r.tools) == 0 {
		return ""
	}
	return formatToolActivity(r.tools)
}

func (r *runRenderer) thoughtTextLocked() string {
	if strings.TrimSpace(r.thought) == "" {
		return ""
	}
	return formatThought(r.thought, true)
}

// thoughtHeader is the line above the blockquote. Soraa Code writes
// "💭 Thinking...", and a header is what tells the reader that the message below
// it is reasoning rather than an answer, so the word alone is not enough.
const thoughtHeader = "<b>💭 Thinking...</b>"

func formatThought(value string, streaming bool) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var clipped, wrapper string
	if streaming {
		value += " ▌"
		clipped = truncateStream(value)
		// Plain while the run is live. An expandable blockquote is collapsed by
		// default to three lines, and every redraw re-applies that default, so a
		// reader watching it open is shut again on the next tick. Nothing in a
		// plain blockquote has state to reset, so the redraw has nothing to snap.
		wrapper = "<blockquote>"
	} else {
		// The header counts against the same ceiling as the body, so the room it
		// takes is taken off the body first. Truncating the body alone let the
		// finished message finish 45 runes over the limit the API refuses.
		clipped = truncateThought(value)
		// The published thought is written once and never redrawn, so it can
		// afford to fold itself out of the way.
		wrapper = "<blockquote expandable>"
	}
	return wrapper + thoughtHeader + "\n\n" + html.EscapeString(clipped) + "</blockquote>"
}

func (r *runRenderer) setCancel(cancel context.CancelFunc) {
	r.mu.Lock()
	r.cancel = cancel
	stopped := r.stopped
	r.mu.Unlock()
	if stopped && cancel != nil {
		cancel()
	}
}

func (r *runRenderer) close() {
	if r.streamSignal == nil {
		return
	}
	r.streamMu.Lock()
	if !r.streamClosed {
		r.streamClosed = true
		close(r.streamSignal)
	}
	r.streamMu.Unlock()
	<-r.streamDone
}

// endThoughtBlock closes the current thinking block, so the reasoning that comes
// after it opens a message of its own.
//
// The boundary is a tool call, because that is where the model stops reasoning
// and starts acting. One message for the whole run forced every block of
// reasoning into a single bubble that was redrawn for the entire run, so the last
// block overwrote the first and the reader could only ever see the tail of the
// thinking. The closing block is published on the way out, not left carrying a
// live cursor.
func (r *runRenderer) endThoughtBlock() {
	r.mu.Lock()
	thoughtID := r.thoughtID
	thought := r.thought
	empty := strings.TrimSpace(thought) == ""
	r.thoughtID = 0
	r.thought = ""
	r.sentThought = ""
	r.mu.Unlock()
	if thoughtID == 0 || empty {
		return
	}
	if err := editTelegramHTMLMessage(r.token, r.chatID, thoughtID, formatThought(thought, false)); err != nil {
		if fallbackErr := editTelegramMessage(r.token, r.chatID, thoughtID, thoughtFallbackText(formatThought(thought, false))); fallbackErr != nil {
			logger.Warn("Telegram thought block publication failed: " + fallbackErr.Error())
		}
	}
}

// endAnswerBlock publishes whatever the model wrote before this tool call and
// releases the message, so the next thing it writes opens a bubble of its own.
//
// A tool call is the boundary here for the same reason it is for the thought: the
// model stopped writing prose and started acting. Without this, every fragment the
// model produced during a run was written into one message, each edit overwriting
// the last, so the run ended showing only its final fragment and the chat held no
// record of what it said along the way.
func (r *runRenderer) endAnswerBlock() {
	r.mu.Lock()
	messageID := r.messageID
	content := strings.TrimSpace(r.streamContent)
	if messageID == 0 || content == "" {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if err := r.editRichOrUnavailable(messageID, truncateStream(content)); err != nil {
		if err := editTelegramMessage(r.token, r.chatID, messageID, truncateStream(content)); err != nil {
			logger.Warn("Telegram answer block publication failed: " + err.Error())
		}
	}
	r.mu.Lock()
	r.messageID = 0
	r.streamContent = ""
	// sentStream and sentStreamID are deliberately kept. They identify the message
	// now holding the published fragment, so finish can complete that message in
	// place when the final answer supersedes it, instead of sending a duplicate.
	// Clearing them here is what left the fragment orphaned while the final went
	// out as a brand new message beside it.
	r.mu.Unlock()
}

func (r *runRenderer) reasoning(value string) {
	now := time.Now()
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.thought += value
	thoughtID := r.thoughtID
	r.lastStatus = now
	r.mu.Unlock()
	// Opening a message is never rate limited. The floor below paces redraws of
	// a block that already exists, and applying it to creation meant a new block
	// of reasoning that arrived inside the window never got a message at all: the
	// text was accumulated into a thought with no message to show it on.
	if thoughtID == 0 {
		thoughtMessageID, err := sendHTMLMessage(r.token, r.chatID, formatThought(value, true))
		if err != nil {
			thoughtMessageID = sendPlainMessage(r.token, r.chatID, thoughtFallbackText(formatThought(value, true)))
		}
		if thoughtMessageID == 0 {
			return
		}
		r.mu.Lock()
		r.thoughtID = thoughtMessageID
		// Seeded with what was actually posted. Leaving it empty cost one
		// identical redraw at the start of every block, which Telegram answered
		// "message is not modified" to, and which the reader saw as the block
		// flashing before it started moving.
		r.sentThought = formatThought(r.thought, true)
		r.lastThoughtPush = now
		r.mu.Unlock()
	}
	r.signalStream()
}

func (r *runRenderer) tool(callID, value string) {
	activity, ok := parseToolActivity(value)
	if !ok {
		return
	}
	activity.id = callID
	now := time.Now()
	running := activity.status == "🔄" || activity.status == "⚡"
	// A tool call is a block boundary: the model stopped reasoning and started
	// acting, so whatever reasons next is a new block and gets its own message.
	// Taken outside the lock below, because publishing the closing block is a
	// network call and holding the mutex across one would stall every producer.
	if !running {
		r.endThoughtBlock()
		r.endAnswerBlock()
	}
	r.mu.Lock()
	// streamContent is deliberately NOT cleared here. It used to be, because the
	// tool trail rode in front of the answer in one message and a tool result
	// ended the prose paragraph. With the trail in a message of its own, clearing
	// it dropped the answer the reader was watching down to a bare cursor for as
	// long as the tool ran, which is the lone "\u258c" bubble a screenshot showed
	// sitting under the prompt.
	// A running line is updated in place, not appended. Soraa Code keys the live
	// line on the call id for the same reason: a tool that reports progress more
	// than once is one line that moves, and a list that grew on every report made
	// a busy run unreadable while pushing the answer off the bottom of the
	// message. The label fallback only applies when there is no call id to key
	// on, and never for a running line, where merging two concurrent calls that
	// happen to share a label would silently drop one.
	index := findToolLocked(r.tools, callID, activity.label, !running || callID == "")
	switch {
	case index < 0:
		activity.startedAt = now
		activity.shownBucket = -1
		r.tools = append(r.tools, activity)
	default:
		entry := &r.tools[index]
		entry.status = activity.status
		if activity.name != "" {
			entry.name = activity.name
		}
		if activity.detail != "" {
			entry.detail = activity.detail
		}
		// The elapsed time shown on a running line is frozen when the call ends.
		// A call that never crossed the threshold has nothing on screen to freeze,
		// and stays as it was, which is how a fast call reads as fast.
		if !running && entry.shownBucket >= 0 {
			entry.elapsed = formatElapsed(now.Sub(entry.startedAt))
		}
	}
	r.mu.Unlock()
	// Signalled unconditionally: the tool trail is created by the render, so the
	// first report has to reach it before any message exists.
	r.signalStream()
}

// findToolLocked locates the entry a tool event belongs to, newest first.
//
// allowLabelMatch covers the case where a completion arrives for a call whose
// start was never seen, so the id lookup has nothing to land on.
func findToolLocked(tools []toolActivity, callID, label string, allowLabelMatch bool) int {
	if callID != "" {
		for i := len(tools) - 1; i >= 0; i-- {
			if tools[i].id == callID {
				return i
			}
		}
		if !allowLabelMatch {
			return -1
		}
	}
	if !allowLabelMatch {
		return -1
	}
	for i := len(tools) - 1; i >= 0; i-- {
		if tools[i].label == label {
			return i
		}
	}
	return -1
}

func (r *runRenderer) content(delta string) {
	if delta == "" {
		return
	}
	r.mu.Lock()
	r.streamContent += delta
	r.mu.Unlock()
	// Pacing lives in signalStream now, so a stream tick cannot outrun the others.
	r.signalStream()
}

// markReasoningOnly records that the step which just ended produced no tool call,
// which is how the coordinator defines a final answer. Everything on the content
// channel so far was therefore reasoning.
//
// A provider is free to put its reasoning on the content channel and Atria does
// exactly that when no reasoning level was requested, which the run log recorded
// as reasoning="" next to an answer message that opened with the same sentence as
// the thought block above it. The preview then showed the thinking a second time,
// in a bubble meant for the answer, and it never recovered: the run ended with the
// answer published into a message that had been carrying reasoning all along.
//
// There is no way to tell the two apart from the delta alone, because the split
// lands mid-word and the channels interleave. The coordinator can tell, so it says
// so rather than leaving the renderer to guess.
func (r *runRenderer) markReasoningOnly() {
	r.mu.Lock()
	content := r.streamContent
	messageID := r.messageID
	sent := r.sentStream
	// The answer message is released, not kept. Keeping it meant the next content
	// delta landed on a message whose text had just been folded into a thought, so
	// the run's next fragment overwrote the same bubble instead of opening one.
	r.messageID = 0
	r.streamContent = ""
	r.sentStream = ""
	r.sentStreamID = 0
	r.mu.Unlock()
	if strings.TrimSpace(content) == "" {
		return
	}
	// The text arrived on the content channel because the provider put it there, so
	// it belongs to the thought. It is folded rather than dropped: dropping it lost
	// the opening of a turn, and folding it is also what stops it being shown twice.
	r.reasoning(content)
	if messageID == 0 {
		return
	}
	// The message this text was shown on is now redundant: its whole content lives
	// in the thought. Leaving it meant the chat held the reasoning twice, once as
	// the thought and once as an answer that never was. It is deleted only when it
	// holds nothing but the folded text; if a real answer landed on it since, the
	// sent text no longer matches and it is left alone.
	if sent != "" && strings.TrimSpace(sent) == strings.TrimSpace(content) {
		if err := deleteTelegramMessage(r.token, r.chatID, messageID); err != nil {
			logger.Warn("Telegram folded reasoning cleanup failed: " + err.Error())
		}
	}
}

func (r *runRenderer) finish(answer string) {
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		r.close()
		return
	}
	r.close()
	r.mu.Lock()
	stopped = r.stopped
	messageID := r.messageID
	fragmentID := r.sentStreamID
	fragment := r.sentStream
	thoughtID := r.thoughtID
	thought := r.thought
	r.mu.Unlock()
	if stopped {
		return
	}
	if thoughtID != 0 && strings.TrimSpace(thought) != "" {
		if !r.publishThoughtMessage(formatThought(thought, false), thoughtID) {
			logger.Warn("Telegram final thought publication failed")
		}
	}
	// When the live preview was released (a tool call closed the block) but the
	// final answer is that same text grown to completion, the final belongs on the
	// preview's message. Sending it as a new message left the chat holding the
	// fragment and the final side by side, which is the duplication a run log
	// showed as an 829-rune streaming message next to its own 855-rune final.
	// A fragment the final does not contain is independent history and is left
	// alone; only a preview the final supersedes is completed in place.
	if messageID == 0 && fragmentID != 0 && fragment != "" && strings.HasPrefix(strings.TrimSpace(answer), strings.TrimSpace(fragment)) {
		messageID = fragmentID
	}
	r.publishFinalAnswer(answer, messageID)
}

// publishFinalAnswer delivers the answer and makes sure the keyboard follows it.
//
// Extracted so the contract is testable: the answer is the message the user
// reads, and the one that has to end up carrying the keyboard. A green suite that
// only exercised message creation passed while this path silently stopped
// attaching anything.
func (r *runRenderer) publishFinalAnswer(answer string, messageID int) {
	if answer != "" {
		runes := []rune(answer)
		if len(runes) <= telegramRichMessageLimit {
			if !r.publishRichMessage(answer, messageID) {
				if messageID == 0 {
					sendPlainMessage(r.token, r.chatID, answer)
				} else {
					if err := editTelegramMessage(r.token, r.chatID, messageID, answer); err != nil {
						logger.Warn("Telegram final message fallback failed: " + err.Error())
						// The edit failed, so nothing is carrying the keyboard. Fall
						// back to a fresh send, which does.
						if sendPlainMessage(r.token, r.chatID, answer) == 0 {
							logger.Warn("Telegram final answer could not be delivered")
						}
					} else if err := attachReplyKeyboard(r.token, r.chatID, messageID); err != nil {
						// The answer landed but the keyboard did not. Reported rather
						// than retried: editMessageReplyMarkup succeeds about 85% of
						// the time, and hammering a rejection here turns one wasted
						// call into several. Tagged with the end that failed so a log
						// line names the caller instead of leaving two identical
						// messages with different causes.
						logger.Warn("reply keyboard attach failed on final answer: " + err.Error())
					}
				}
			}
		} else if messageID == 0 {
			for rest := answer; len([]rune(rest)) > 0; {
				chunkRunes := []rune(rest)
				size := telegramRichMessageLimit
				if len(chunkRunes) < size {
					size = len(chunkRunes)
				}
				chunk := string(chunkRunes[:size])
				rest = string(chunkRunes[size:])
				if _, err := sendRichMessage(r.token, r.chatID, chunk); err != nil {
					sendPlainMessage(r.token, r.chatID, chunk)
				}
			}
		} else {
			first := string(runes[:telegramRichMessageLimit])
			if !r.publishRichMessage(first, messageID) {
				if err := editTelegramMessage(r.token, r.chatID, messageID, first); err != nil {
					logger.Warn("Telegram long message fallback failed: " + err.Error())
				}
			}
			for rest := string(runes[telegramRichMessageLimit:]); len([]rune(rest)) > 0; {
				chunkRunes := []rune(rest)
				size := telegramRichMessageLimit
				if len(chunkRunes) < size {
					size = len(chunkRunes)
				}
				chunk := string(chunkRunes[:size])
				rest = string(chunkRunes[size:])
				if _, err := sendRichMessage(r.token, r.chatID, chunk); err != nil {
					sendPlainMessage(r.token, r.chatID, chunk)
				}
			}
		}
	}
}

func (r *runRenderer) fail() {
	r.close()
	r.mu.Lock()
	stopped := r.stopped
	thoughtID := r.thoughtID
	thought := r.thought
	r.mu.Unlock()
	if stopped {
		return
	}
	if thoughtID != 0 && strings.TrimSpace(thought) != "" {
		if !r.publishThoughtMessage(formatThought(thought, false), thoughtID) {
			logger.Warn("Telegram failed thought publication failed")
		}
	}
	r.resolvePreview()
}

// resolvePreview retires the live answer preview when no final answer is coming.
//
// A run that errors, or that a new prompt supersedes, never reaches finish, so
// the preview kept the streaming cursor forever: a message in the chat claiming
// to still be typing, minutes after the run was over, with nothing to replace it.
// The text produced so far is published without the cursor when there is any, and
// the message is deleted when there is not, because an empty bubble is worse than
// no bubble.
func (r *runRenderer) resolvePreview() {
	r.mu.Lock()
	messageID := r.messageID
	content := strings.TrimSpace(r.streamContent)
	if content == "" {
		r.messageID = 0
		r.sentStream = ""
		r.sentStreamID = 0
	}
	r.mu.Unlock()
	if messageID == 0 {
		return
	}
	if content == "" {
		if err := deleteTelegramMessage(r.token, r.chatID, messageID); err != nil {
			logger.Warn("Telegram empty preview cleanup failed: " + err.Error())
		}
		return
	}
	if err := r.editRichOrUnavailable(messageID, truncateStream(content)); err != nil {
		if err := editTelegramMessage(r.token, r.chatID, messageID, truncateStream(content)); err != nil {
			logger.Warn("Telegram abandoned preview could not be finalised: " + err.Error())
		}
	}
}

func (r *runRenderer) stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// A stopped run never reaches finish, so its preview is retired here for the
	// same reason fail() does it. Without this, stopping a run to ask something else
	// left the old preview claiming to still be typing.
	go func() {
		<-r.streamDone
		r.resolvePreview()
	}()
}

func (r *runRenderer) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *runRenderer) publishThoughtMessage(text string, messageID int) bool {
	if messageID != 0 {
		// Publishing text the message is already showing is the one edit Telegram
		// answers "message is not modified". It started happening once the cursor
		// came off the live preview, because the live and published forms became
		// identical for a thought whose last delta had already been drawn.
		r.mu.Lock()
		alreadyShown := r.sentThought == text
		r.mu.Unlock()
		if alreadyShown {
			return true
		}
		if err := editTelegramHTMLMessage(r.token, r.chatID, messageID, text); err == nil {
			return true
		}
	}
	newMessageID, err := sendHTMLMessage(r.token, r.chatID, text)
	if err != nil {
		newMessageID = sendPlainMessage(r.token, r.chatID, thoughtFallbackText(text))
	}
	if newMessageID == 0 {
		return false
	}
	if messageID != 0 && messageID != newMessageID {
		if err := deleteTelegramMessage(r.token, r.chatID, messageID); err != nil {
			logger.Warn("Telegram thought replacement cleanup failed: " + err.Error())
		}
	}
	return true
}

func (r *runRenderer) publishRichMessage(markdown string, messageID int) bool {
	if messageID != 0 {
		// The streaming preview may already show exactly this text, because the
		// message was created carrying it and the creation was recorded as
		// delivered. Re-PUTting it is the one edit Telegram answers "message is
		// not modified", and in production the rich edit then fails loudly enough
		// to trigger the replacement path below, which sends a duplicate and
		// deletes the original for no reason at all.
		r.mu.Lock()
		alreadyShown := messageID == r.sentStreamID && strings.TrimSpace(markdown) == strings.TrimSpace(r.sentStream)
		r.mu.Unlock()
		if alreadyShown {
			return true
		}
		if err := r.editRichOrUnavailable(messageID, markdown); err == nil {
			return true
		} else {
			// Named with the id and the refusal, because the fallback below sends a
			// brand new message and deletes this one: if the delete then fails or
			// targets the wrong id, the chat holds two copies of the same answer,
			// and without this line there is no way to tell why the edit path was
			// abandoned.
			logger.Warn(fmt.Sprintf("Telegram rich edit of message %d refused (%s); sending replacement", messageID, err.Error()))
		}
	}
	if _, err := sendRichMessage(r.token, r.chatID, markdown); err != nil {
		return false
	}
	if messageID != 0 {
		if err := deleteTelegramMessage(r.token, r.chatID, messageID); err != nil {
			logger.Warn("Telegram replacement message cleanup failed: " + err.Error())
		}
	}
	return true
}

const telegramRichMessageLimit = 32768

func editTelegramMessage(token string, chatID int64, messageID int, text string) error {
	return postTelegramEditMessage(token, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
}

// editTelegramRichMessage keeps the original payload: no reply_markup. See
// sendRichMessage for why the rich payload is treated as fixed.
func editTelegramRichMessage(token string, chatID int64, messageID int, markdown string) error {
	return postTelegramEditMessage(token, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"rich_message": map[string]any{
			"markdown": markdown,
		},
	})
}

// errRichEditUnavailable is returned when this run has already learned that the
// API refuses rich edits, so the call is not placed at all.
var errRichEditUnavailable = errors.New("rich edit is unavailable")

// editRichOrUnavailable places one rich edit and remembers a refusal.
//
// editMessageText carrying rich_message is rejected with "rich edit is
// unavailable" by the live API, while the rich send alongside it succeeds: the
// two are recorded separately in menuhandler_test and streamedit_test from the
// same trace. Attempting it on every streaming tick therefore bought a 400 on
// each one, up to 1999 in the recorded log, and only fell through to the plain
// edit after the round trip had already been paid.
//
// The probe stays live rather than being deleted outright: one attempt per run
// is what keeps the branch honest if the endpoint ever starts accepting it.
func (r *runRenderer) editRichOrUnavailable(messageID int, markdown string) error {
	r.mu.Lock()
	known := r.richEditUnavailable
	r.mu.Unlock()
	if known {
		return errRichEditUnavailable
	}
	err := editTelegramRichMessage(r.token, r.chatID, messageID, markdown)
	if isRichEditUnavailable(err) {
		r.mu.Lock()
		r.richEditUnavailable = true
		r.mu.Unlock()
	}
	return err
}

func isRichEditUnavailable(err error) bool {
	var apiErr *telegramAPIError
	return errors.As(err, &apiErr) && apiErr.description == "rich edit is unavailable"
}

// attachReplyKeyboard puts the reply keyboard on a message that is already sent,
// without touching its content.
//
// This is a separate call rather than a field on the send because the message was
// created by the rich endpoint, whose payload is fixed. editMessageReplyMarkup
// changes only the markup, so the rich rendering of tool calls and thoughts is
// left alone. A rich send is the live path, so without this the run finishes with
// no keyboard at all.
func attachReplyKeyboard(token string, chatID int64, messageID int) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": keyboard.Main(),
	})
	resp, err := doTelegramPost(api("editMessageReplyMarkup", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("editMessageReplyMarkup: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("editMessageReplyMarkup: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("editMessageReplyMarkup response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("editMessageReplyMarkup: %s", result.Description)
	}
	return nil
}

// editTelegramHTMLMessage carries no reply markup. Attaching a reply keyboard
// to editMessageText fails about 75% of the time, and thought messages are
// collapsible rather than carriers, so there is nothing to gain and a wasted call
// to lose on every streaming tick.
func editTelegramHTMLMessage(token string, chatID int64, messageID int, text string) error {
	return postTelegramEditMessage(token, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	})
}

func sendHTMLMessage(token string, chatID int64, text string) (int, error) {
	return sendMessagePayload(token, chatID, text, "HTML", nil)
}

// thoughtFallbackText reduces a thought to the plain text it carries.
//
// Both wrappers are listed, and the expandable one first: the streaming form
// dropped "expandable" once the collapse state started resetting on every
// redraw, and a replacer that only knew the old spelling left a raw <blockquote>
// in the plain fallback.
func thoughtFallbackText(value string) string {
	value = strings.NewReplacer(
		"<blockquote expandable>", "",
		"<blockquote>", "",
		"</blockquote>", "",
		"<b>", "",
		"</b>", "",
	).Replace(value)
	return html.UnescapeString(value)
}

// telegramAPIError is a rejection the API itself described.
//
// It exists so a caller can branch on Telegram's own words instead of on a
// formatted string, and so the string a human reads in the log stays identical
// to what the call would have printed before the type existed.
type telegramAPIError struct {
	method      string
	description string
}

func (e *telegramAPIError) Error() string {
	return e.method + ": " + e.description
}

// telegramEditVerdict interprets a rejected editMessageText.
//
// Telegram answers every API-level rejection with a body carrying its own
// description, and that body survives the transport: doTelegramPost reports a
// non-2xx as an error that still holds the JSON. Reading the description there
// is what makes the two interesting cases branchable without parsing an error
// string back apart. A description saying the message already holds this exact
// text is not a failure, it is the API confirming there was nothing to write:
// calling it one left sentStream unset, so the next signal re-sent
// byte-identical text, and that loop is what took the recorded run to
// "retry after 227".
//
// fallback is returned untouched when the body is not a Telegram rejection, so
// an unrelated transport failure keeps the message it already had.
func telegramEditVerdict(fallback error, raw []byte) error {
	var result struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Description == "" {
		return fallback
	}
	if strings.Contains(strings.ToLower(result.Description), "message is not modified") {
		return nil
	}
	return &telegramAPIError{method: "editMessageText", description: result.Description}
}

// postTelegramEditMessage runs one editMessageText call.
func postTelegramEditMessage(token string, payload map[string]any) error {
	body, _ := json.Marshal(payload)
	resp, err := doTelegramPost(api("editMessageText", token), body)
	if err != nil {
		var httpErr *telegramHTTPError
		if !errors.As(err, &httpErr) {
			return err
		}
		return telegramEditVerdict(err, []byte(httpErr.body))
	}
	if resp == nil {
		return fmt.Errorf("editMessageText: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return telegramEditVerdict(fmt.Errorf("editMessageText: HTTP %d: %s", resp.StatusCode, raw), raw)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("editMessageText response: %w", err)
	}
	if !result.OK {
		return telegramEditVerdict(&telegramAPIError{method: "editMessageText"}, raw)
	}
	return nil
}

// sendRichMessage creates the assistant message through the rich endpoint.
//
// The payload is exactly chat_id plus rich_message and nothing else. Adding a
// reply_markup key here, to attach the reply keyboard, broke the endpoint's own
// rendering of tool calls and thoughts, so the keyboard is attached afterwards by
// a separate markup-only call instead. The endpoint is private and undocumented;
// treat its payload as fixed.
func sendRichMessage(token string, chatID int64, markdown string) (int, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"rich_message": map[string]any{
			"markdown": markdown,
		},
	}
	body, _ := json.Marshal(payload)
	resp, err := doTelegramPost(api("sendRichMessage", token), body)
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, fmt.Errorf("sendRichMessage: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("sendRichMessage: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("sendRichMessage response: %w", err)
	}
	if !result.OK {
		return 0, fmt.Errorf("sendRichMessage: %s", result.Description)
	}
	return result.Result.MessageID, nil
}

func deleteTelegramMessage(token string, chatID int64, messageID int) error {
	body, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	resp, err := doTelegramPost(api("deleteMessage", token), body)
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("deleteMessage: empty response")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deleteMessage: HTTP %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("deleteMessage response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("deleteMessage: %s", result.Description)
	}
	return nil
}

// streamPreviewLimit is the ceiling on one rendered body.
//
// Telegram caps editMessageText at 4096 characters and the blockquote wrapper
// around a thought eats part of that, so nothing is sent over 4000 runes.
const streamPreviewLimit = 4000

// truncateStream keeps the newest text of a live preview.
//
// A streamed message has no append: the whole buffer is re-sent on every tick,
// so the limit has to cut somewhere. The tail is the part the reader has not
// reached yet, so the head goes, and the leading ellipsis says so instead of
// letting a truncated answer open as though it had started there.
func truncateStream(value string) string {
	runes := []rune(value)
	if len(runes) <= streamPreviewLimit {
		return value
	}
	return "..." + string(runes[len(runes)-(streamPreviewLimit-3):])
}

// truncateThought keeps the opening of a completed thought.
//
// The published thought is a finished artifact, not a progress line, and cutting
// its start would rewrite where the reasoning began. The same ceiling applies,
// so the tail carries the ellipsis.
// truncateThought keeps the start of a finished thought within the same ceiling
// as a live one, counting the header that is added around it afterwards.
func truncateThought(value string) string {
	runes := []rune(value)
	// The header, the blank line after it, and the ellipsis that marks a cut.
	room := streamPreviewLimit - len([]rune(thoughtHeader)) - 2 - 3
	if len(runes) <= room {
		return value
	}
	return string(runes[:room]) + "..."
}

func toolArgsSummary(name string, args map[string]any) string {
	raw, _ := args["args"].(string)
	if raw == "" {
		return ""
	}
	values := make(map[string]any)
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return ""
	}
	key := "value"
	switch name {
	case "read", "write", "edit", "edit_line", "batch_edit_file", "move", "copy":
		key = "path"
	case "bash":
		key = "command"
	case "glob":
		key = "pattern"
	case "grep":
		key = "pattern"
	case "fts_search":
		key = "query"
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	text := strings.Join(strings.Fields(redact.SecretsInText(fmt.Sprint(value))), " ")
	if text == "" {
		return ""
	}
	return " · " + truncateStatus(text, 96)
}

// toolNameToken carries the tool name through the same string as the label.
//
// The renderer is handed one line of text per tool event and nothing else, so
// the name has to travel inside it. A space would split the name for tools whose
// name has one in it, and the label is what the entry is matched on, so the
// token is delimited and taken from the front.
func toolNameToken(name string) string {
	if name == "" {
		return toolNameTokenPrefix + " · "
	}
	return toolNameTokenPrefix + " " + name + " · "
}

// toolNameTokenPrefix opens the tool name inside an event line.
const toolNameTokenPrefix = "·tool·"

func toolDisplayName(name string) string {
	switch name {
	case "read":
		return "📚 Read file"
	case "write":
		return "📝 Write file"
	case "edit", "edit_line", "batch_edit_file":
		return "📝 Edit file"
	case "bash":
		return ">_ Run command"
	case "glob":
		return "Find files"
	case "grep":
		return "Search files"
	case "fts_search":
		return "Search project"
	default:
		if name == "" {
			return "Run tool"
		}
		return strings.ReplaceAll(name, "_", " ")
	}
}

func parseToolActivity(value string) (toolActivity, bool) {
	value = strings.TrimSpace(value)
	prefixes := []struct {
		prefix string
		status string
	}{
		{"🔄 ", "🔄"},
		{"✅ ", "✓"},
		{"✓ ", "✓"},
		{"⚠️ ", "⚠"},
		{"⚠ ", "⚠"},
		{"⚡ ", "⚡"},
	}
	for _, item := range prefixes {
		if !strings.HasPrefix(value, item.prefix) {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(value, item.prefix))
		name := ""
		if rest, ok := strings.CutPrefix(body, toolNameTokenPrefix+" "); ok {
			if between, tail, found := strings.Cut(rest, " · "); found {
				name = between
				body = tail
			}
		}
		detail := ""
		if separator := strings.Index(body, " · "); separator >= 0 {
			detail = strings.TrimSpace(body[separator+3:])
			body = strings.TrimSpace(body[:separator])
		}
		if item.status != "🔄" && item.status != "⚡" {
			body = strings.TrimSpace(strings.TrimSuffix(body, " completed"))
			body = strings.TrimSpace(strings.TrimSuffix(body, " failed"))
			body = strings.TrimSpace(strings.TrimSuffix(body, " cancelled"))
		}
		return toolActivity{name: name, status: item.status, label: body, detail: detail}, true
	}
	return toolActivity{}, false
}

// formatToolActivity is one line per call: icon, tool name, then whatever
// identifies it.
//
// The bullet, the tick and the middle dot are all gone, and with them the line
// that reads "• ✓ > Run command · cd /workspace". The same call renders as
// "⚡ bash cd /workspace", which is the tool rather than the harness's
// sentence about it, and that is what a reader scanning the message is looking
// for. Entries are separated by a blank line for the same reason: one call per
// paragraph, so a long command does not run into the line under it.
//
// Nothing is dropped. The old six-line window existed because the lines were
// bullets sharing one paragraph, and a window over a list that grows for twenty
// minutes reads as the run forgetting what it did.
func formatToolActivity(tools []toolActivity) string {
	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := tool.name
		if name == "" {
			name = tool.label
		}
		line := toolIcon(name) + " " + name
		if tool.detail != "" {
			line += " " + tool.detail
		}
		if tool.elapsed != "" {
			line += " · 🕐 " + tool.elapsed
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n\n")
}

// toolIcon is the per-tool glyph, so a call reads as a mark plus a name rather
// than a sentence. An unknown tool gets a gear rather than nothing: an
// unlabelled line is worse than a generic one.
func toolIcon(name string) string {
	switch name {
	case "read":
		return "📚"
	case "write":
		return "✏️"
	case "edit", "edit_line", "batch_edit_file":
		return "✏️"
	case "apply_patch":
		return "🛠"
	case "bash":
		return "⚡"
	case "glob":
		return "📂"
	case "grep", "fts_search":
		return "🔎"
	case "task":
		return "🎯"
	case "question":
		return "❓"
	case "todowrite":
		return "📌"
	case "webfetch":
		return "🔍"
	case "compaction":
		return "⚡"
	default:
		return "⚙️"
	}
}

func toolResultLabel(result operation.ResultEvent) string {
	switch result.Status {
	case operation.StatusCompleted:
		return "completed"
	case operation.StatusCanceled:
		return "cancelled"
	default:
		return "failed"
	}
}

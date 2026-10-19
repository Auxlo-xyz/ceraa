// Package whatsapp links WhatsApp direct messages to Ceraa agent runs.
//
// One DM chat maps to one session. Group chats are skipped; groups are
// phase two.
package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ceraa/harness/coordinator"
	"ceraa/harness/redact"
	"ceraa/harness/sessionstore"
	"ceraa/internal/logger"
	"ceraa/internal/paths"
)

const (
	waSessionPrefix = "wa-"
	// waEnableEnv gates the bridge. Read here directly; no sibling
	// config.go exists yet to own it.
	waEnableEnv = "CERAA_WHATSAPP_ENABLED"
	// waBusyReply answers a message that arrives while its chat has a run.
	waBusyReply = "Ceraa is still working on your last message - one moment."
	// waProgressAfter bounds silence before one progress note goes out.
	waProgressAfter = 30 * time.Second
	// waProgressNote is that note. At most one goes out per run.
	waProgressNote = "Still working on it - I will reply when the run finishes."
	// waMaxChunk bounds one outbound text; longer answers send in parts.
	waMaxChunk = 4000
	// waTypingRefresh re-sends the typing indicator while a run is active.
	// WhatsApp expires the chat state after a few seconds, so a single send
	// does not cover a long run.
	waTypingRefresh = 8 * time.Second
	// waNoAnswerNote covers runs that finish with no reply text.
	waNoAnswerNote = "Done."
	// waFailNote covers runs that error. Detail stays in the log.
	waFailNote = "Ceraa could not finish that run."
	// waToolLineCap bounds how many tool lines one run may send, so a runaway
	// loop cannot flood the chat. Anything past the cap is reported once at
	// the end. There is deliberately no minimum gap between lines: a gap hid
	// real tool calls from the reader, and the cap already bounds the volume.
	waToolLineCap = 25
	// waToolTargetMax bounds the identifying argument shown on a tool line.
	waToolTargetMax = 60
)

// RunFunc runs one turn through the coordinator. The bridge builds the
// RunnerConfig (session plus WhatsApp-flavored callbacks); the caller
// supplies provider and store wiring, which the daemon fills in.
type RunFunc func(ctx context.Context, rc coordinator.RunnerConfig, prompt string) (coordinator.Result, error)

// Transport is owned by transport.go. It is referenced here, not
// redefined: the narrow seam stays testable through the fake in
// transport_test.go. Shape, proven by that test: Connect, Disconnect,
// IsLoggedIn, SendText(ctx, toJID, text) (string, error), Incoming.

// Bridge maps one WhatsApp DM to one agent run.
type Bridge struct {
	transport Transport
	cfg       Config
	run       RunFunc
	filesDir  string

	mu     sync.Mutex
	active map[string]struct{}
}

// Manager owns a Bridge and its lifecycle. A failed run ends the message,
// never the process: each inbound is handled on its own goroutine.
type Manager struct {
	bridge *Bridge

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager builds a Manager. An empty filesDir resolves via
// paths.FilesDir, which honors CERAA_FILES_DIR.
func NewManager(t Transport, cfg Config, run RunFunc, filesDir string) *Manager {
	if filesDir == "" {
		filesDir = paths.FilesDir()
	}
	return &Manager{bridge: &Bridge{transport: t, cfg: cfg, run: run, filesDir: filesDir, active: map[string]struct{}{}}}
}

// Start connects and serves inbound messages until ctx ends or Stop runs.
// It refuses when CERAA_WHATSAPP_ENABLED is not true.
func (m *Manager) Start(ctx context.Context) error {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(waEnableEnv)), "true") {
		return fmt.Errorf("ceraa whatsapp: bridge disabled (set %s=true)", waEnableEnv)
	}
	if m.bridge.transport == nil {
		return fmt.Errorf("ceraa whatsapp: no transport")
	}
	if m.bridge.run == nil {
		return fmt.Errorf("ceraa whatsapp: no runner")
	}
	if err := m.bridge.transport.Connect(ctx); err != nil {
		return fmt.Errorf("ceraa whatsapp: connect: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.done = make(chan struct{})
	m.mu.Unlock()
	go m.serve(runCtx)
	return nil
}

// Stop ends the inlet loop and disconnects.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if m.bridge.transport != nil {
		_ = m.bridge.transport.Disconnect()
	}
}

// serve reads the inlet until ctx ends or the channel closes.
func (m *Manager) serve(ctx context.Context) {
	defer close(m.done)
	ch := m.bridge.transport.Incoming()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				logger.Info("whatsapp inlet closed")
				return
			}
			go m.bridge.handle(ctx, msg)
		}
	}
}

// allowed reports whether senderJID may use the bridge. The boot config carries
// the environment seed; Allowed() adds the hub-editable store, so a number
// added from the Hub applies on the next message without a restart. The boot
// config is checked first so a cfg-only allow-list (a test's, or an install
// with no store) never touches the file.
func (b *Bridge) allowed(senderJID, selfJID string) bool {
	if b.cfg.Allowed(senderJID, selfJID) {
		return true
	}
	return Allowed(senderJID, selfJID)
}

// handle maps one DM to one run. It recovers so a bad message ends the
// message, not the process.
func (b *Bridge) handle(ctx context.Context, msg InboundMessage) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn(fmt.Sprintf("whatsapp handler panic: %v", r))
		}
	}()
	tag := chatTag(msg.ChatJID)
	if msg.FromMe {
		return
	}
	if msg.GroupJID != "" {
		// Groups are phase two.
		logger.Info(fmt.Sprintf("whatsapp group skipped tag=%s", tag))
		return
	}
	if !b.allowed(msg.ChatJID, b.transport.SelfJID()) {
		// Fail closed: a stranger's message buys no run and no reply.
		// The tag is a hash, so this line cannot be used to harvest
		// the sender, only to confirm a skip happened.
		logger.Info(fmt.Sprintf("whatsapp sender not allowed tag=%s", tag))
		return
	}
	// An accepted message is read immediately, so the sender gets blue ticks
	// when the run is queued rather than when it finishes. A failure here is
	// cosmetic: it must not stop the run.
	if msg.ID != "" {
		if err := b.transport.MarkRead(ctx, msg.ReceiptChat, msg.ReceiptSender, msg.ID); err != nil {
			logger.Warn(fmt.Sprintf("whatsapp read receipt failed tag=%s err=%.80s", tag, err))
		} else {
			// A success is logged too: a fix you cannot observe is not a
			// fix, and only a failure line made the receipt invisible.
			logger.Info(fmt.Sprintf("whatsapp read receipt sent tag=%s", tag))
		}
	}
	prompt := strings.TrimSpace(msg.Text)
	if msg.MediaError != "" {
		// The transport could not fetch the attachment. Saying so is the
		// difference between "the bot ignored my photo" and "the bot
		// tried and told me", and the reason stays in the log.
		logger.Warn(fmt.Sprintf("whatsapp media download failed tag=%s err=%.80s", tag, msg.MediaError))
		_ = b.send(ctx, msg.ChatJID, "Ceraa could not download that attachment.")
		return
	}
	if msg.MediaPath != "" {
		saved, err := SaveInboundMedia(msg, b.filesDir)
		// The staging file is the transport's and the copy in the files
		// dir replaces it, so it goes on both paths. Leaving it behind
		// would leak one file per attachment.
		_ = os.Remove(msg.MediaPath)
		if err != nil {
			logger.Warn(fmt.Sprintf("whatsapp media save failed tag=%s err=%.80s", tag, err))
			_ = b.send(ctx, msg.ChatJID, "Ceraa could not save that attachment.")
			return
		}
		// The save notice is a chat message no run ever sees, so the path
		// is prefixed to the prompt the run does see.
		prompt = strings.TrimSpace("The user attached a file saved at " + saved + ". " + prompt)
	}
	if prompt == "" {
		logger.Info(fmt.Sprintf("whatsapp empty skipped tag=%s", tag))
		return
	}
	sessionID := sessionIDFor(msg.ChatJID)
	b.mu.Lock()
	if _, busy := b.active[sessionID]; busy {
		b.mu.Unlock()
		_ = b.send(ctx, msg.ChatJID, waBusyReply)
		return
	}
	b.active[sessionID] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.active, sessionID)
		b.mu.Unlock()
	}()
	logger.Info(fmt.Sprintf("whatsapp run started tag=%s session=%s", tag, sessionID))
	// The typing indicator is the only sign of life a sender gets until the
	// run finishes. The stop is deferred, so every exit from here clears it.
	stopTyping := b.startTyping(msg.ChatJID, tag)
	defer stopTyping()

	// Collect-first for the answer: deltas accumulate and one final text
	// goes out. Tool calls are different: each distinct call is streamed as
	// its own short message so the sender can watch the run work, with a
	// per-run cap so a runaway loop cannot flood the chat.
	var stream strings.Builder
	var streamMu sync.Mutex
	var doneText string
	var toolMu sync.Mutex
	toolSeen := map[string]struct{}{}
	toolLines, toolSkipped := 0, 0
	emitTool := func(name string, args map[string]any) {
		line := toolLine(name, args)
		if line == "" {
			return
		}
		toolMu.Lock()
		defer toolMu.Unlock()
		if _, dup := toolSeen[line]; dup {
			// The same call again in one run adds nothing the sender has
			// not already seen, and a loop of them must not become a wall.
			return
		}
		toolSeen[line] = struct{}{}
		if toolLines >= waToolLineCap {
			toolSkipped++
			return
		}
		toolLines++
		_ = b.send(context.Background(), msg.ChatJID, line)
	}
	toolSummary := func() {
		toolMu.Lock()
		skipped := toolSkipped
		toolSkipped = 0
		toolMu.Unlock()
		if skipped == 0 {
			return
		}
		noun := "tool calls"
		if skipped == 1 {
			noun = "tool call"
		}
		_ = b.send(context.Background(), msg.ChatJID, fmt.Sprintf("_+%d more %s_", skipped, noun))
	}
	progress := time.AfterFunc(waProgressAfter, func() {
		toolMu.Lock()
		streamed := toolLines > 0
		toolMu.Unlock()
		// Tool lines are the progress; the note is only for a run that has
		// streamed nothing, such as a long stretch of thinking.
		if streamed {
			return
		}
		_ = b.send(context.Background(), msg.ChatJID, waProgressNote)
	})
	defer progress.Stop()

	// Callbacks mirror the Telegram wiring: content accumulates, the tool
	// line is one summary, finish records the run. Refs: OnContent and
	// OnDone in agent-bridge.go agentRunnerConfig, OnRunStarted and
	// OnRunFinished in runAgentWithConfigContextAt.
	rc := coordinator.RunnerConfig{
		SessionID: sessionID,
		// ChatID is the chat the run is answering. It is bound here so a
		// tool whose destination is that chat, such as send_media, cannot be
		// aimed at a different conversation.
		ChatID: msg.ChatJID,
		Callbacks: coordinator.Callbacks{
			OnContent: func(delta string) {
				streamMu.Lock()
				stream.WriteString(delta)
				streamMu.Unlock()
			},
			OnToolStart: func(_ string, name string, args map[string]any) {
				emitTool(name, args)
			},
			OnDone: func(final string) {
				streamMu.Lock()
				doneText = final
				streamMu.Unlock()
			},
			OnRunStarted: func(runID string) {
				logger.Info(fmt.Sprintf("whatsapp run active tag=%s run=%s", tag, runID))
			},
			OnRunFinished: func(runID string, _ sessionstore.RunStatus) {
				streamMu.Lock()
				n := stream.Len()
				streamMu.Unlock()
				logger.Info(fmt.Sprintf("whatsapp run finished tag=%s run=%s streamlen=%d", tag, runID, n))
			},
		},
	}
	res, err := b.run(ctx, rc, prompt)
	if err != nil {
		logger.Warn(fmt.Sprintf("whatsapp run error tag=%s err=%.120s", tag, err))
		toolSummary()
		_ = b.send(ctx, msg.ChatJID, waFailNote)
		return
	}
	final := strings.TrimSpace(res.Final)
	if final == "" {
		streamMu.Lock()
		final = strings.TrimSpace(doneText)
		if final == "" {
			final = strings.TrimSpace(stream.String())
		}
		streamMu.Unlock()
	}
	if final == "" {
		final = waNoAnswerNote
	}
	toolSummary()
	_ = b.send(ctx, msg.ChatJID, final)
	logger.Info(fmt.Sprintf("whatsapp run sent tag=%s answerlen=%d", tag, len(final)))
}

// startTyping turns the typing indicator on for a chat and returns a function
// that clears it. WhatsApp expires the chat state after a few seconds, so the
// indicator is refreshed on a timer until the returned function runs. Both the
// refresh and the clear are best-effort and never affect the run, and the
// returned function is safe to call more than once.
func (b *Bridge) startTyping(chat, tag string) func() {
	send := func(composing bool) {
		if err := b.transport.ChatPresence(context.Background(), chat, composing); err != nil {
			logger.Warn(fmt.Sprintf("whatsapp chat presence failed tag=%s composing=%t err=%.80s", tag, composing, err))
		}
	}
	send(true)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(waTypingRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				send(true)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			send(false)
		})
	}
}

// send delivers one reply in bounded chunks. Markdown tables are rendered as
// monospace blocks first, because a pipe table is unreadable in WhatsApp.
func (b *Bridge) send(ctx context.Context, chatJID, text string) error {
	for _, chunk := range splitText(formatTables(text), waMaxChunk) {
		if _, err := b.transport.SendText(ctx, chatJID, chunk); err != nil {
			logger.Warn(fmt.Sprintf("whatsapp send failed tag=%s err=%.80s", chatTag(chatJID), err))
			return err
		}
	}
	return nil
}

// toolLine renders one tool call as a WhatsApp line: the tool name in bold and
// the one argument that identifies the call in monospace. Monospace is not
// decoration - a path such as my_file_test.go carries two underscores, which
// WhatsApp would otherwise read as an italic span and garble. An empty line
// means there was nothing worth showing.
func toolLine(name string, args map[string]any) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// Telegram spells a tool name with spaces, and WhatsApp needs it more
	// urgently than a cosmetic match: `_` is italic there, so a raw
	// send_media inside *bold* renders with visible marks instead of the
	// word. The original name still goes to toolTarget, which switches on
	// the underscore spelling.
	line := "*" + strings.ReplaceAll(name, "_", " ") + "*"
	if target := toolTarget(name, args); target != "" {
		line += " `" + target + "`"
	}
	return line
}

// toolTarget pulls the identifying argument out of a call's arguments. They
// arrive as a JSON string under "args"; it is decoded so that one field can be
// read, never the map itself, because Go would render map[...] syntax and a
// payload field such as a file body would dump the whole file into the chat.
// Whatever is shown is redacted and bounded, following the rule in AGENTS.md
// "Adding a tool".
func toolTarget(name string, args map[string]any) string {
	raw, _ := args["args"].(string)
	if raw == "" {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return ""
	}
	value, ok := values[toolArgKey(name)]
	if !ok {
		return ""
	}
	text := strings.Join(strings.Fields(redact.SecretsInText(fmt.Sprint(value))), " ")
	if text == "" {
		return ""
	}
	return truncateToolValue(text, waToolTargetMax)
}

// toolArgKey names the argument that identifies a call for a tool; every other
// argument is payload or noise.
func toolArgKey(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "write", "edit", "edit_line", "batch_edit_file", "move", "copy", "ls", "send_media":
		return "path"
	case "bash":
		return "command"
	case "glob", "grep":
		return "pattern"
	case "webfetch":
		return "url"
	case "websearch":
		return "query"
	case "browser":
		return "action"
	}
	return "value"
}

// truncateToolValue bounds a tool value on a rune boundary.
func truncateToolValue(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit]) + "..."
}

// sessionIDFor derives a stable session per DM chat.
func sessionIDFor(chatJID string) string {
	var b strings.Builder
	b.WriteString(waSessionPrefix)
	for _, r := range chatJID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// chatTag redacts a chat JID for logs, following the credentialDebugTag
// pattern: length plus a short hash, never the value itself.
func chatTag(chatJID string) string {
	if chatJID == "" {
		return "empty"
	}
	sum := sha256.Sum256([]byte(chatJID))
	return fmt.Sprintf("len=%d sha256=%s", len([]rune(chatJID)), hex.EncodeToString(sum[:4]))
}

// splitText cuts text into rune-bounded chunks.
func splitText(text string, max int) []string {
	if max <= 0 || len(text) <= max {
		return []string{text}
	}
	var out []string
	runes := []rune(text)
	for len(runes) > max {
		out = append(out, string(runes[:max]))
		runes = runes[max:]
	}
	return append(out, string(runes))
}

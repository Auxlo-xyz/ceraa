// Package whatsapp links WhatsApp direct messages to Ceraa agent runs.
//
// One DM chat maps to one session. Group chats are skipped; groups are
// phase two.
package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"ceraa/harness/coordinator"
	"ceraa/harness/operation"
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
	// waNoAnswerNote covers runs that finish with no reply text.
	waNoAnswerNote = "Done."
	// waFailNote covers runs that error. Detail stays in the log.
	waFailNote = "Ceraa could not finish that run."
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
		}
	}
	prompt := strings.TrimSpace(msg.Text)
	if msg.MediaPath != "" {
		saved, err := SaveInboundMedia(msg, b.filesDir)
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

	// Collect-first throttle: deltas accumulate and one final text goes
	// out, plus at most one progress note on slow runs.
	var stream strings.Builder
	var streamMu sync.Mutex
	var doneText string
	var lastTool string
	rememberTool := func(name string) {
		streamMu.Lock()
		lastTool = name
		streamMu.Unlock()
	}
	progress := time.AfterFunc(waProgressAfter, func() {
		streamMu.Lock()
		note := waProgressNote
		if lastTool != "" {
			note += " (" + lastTool + ")"
		}
		streamMu.Unlock()
		_ = b.send(context.Background(), msg.ChatJID, note)
	})
	defer progress.Stop()

	// Callbacks mirror the Telegram wiring: content accumulates, the tool
	// line is one summary, finish records the run. Refs: OnContent and
	// OnDone in agent-bridge.go agentRunnerConfig, OnRunStarted and
	// OnRunFinished in runAgentWithConfigContextAt.
	rc := coordinator.RunnerConfig{
		SessionID: sessionID,
		Callbacks: coordinator.Callbacks{
			OnContent: func(delta string) {
				streamMu.Lock()
				stream.WriteString(delta)
				streamMu.Unlock()
			},
			OnToolStart: func(_ string, name string, _ map[string]any) {
				rememberTool(name)
			},
			OnToolDone: func(_ string, name string, _ operation.ResultEvent) {
				rememberTool(name)
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
	_ = b.send(ctx, msg.ChatJID, final)
	logger.Info(fmt.Sprintf("whatsapp run sent tag=%s answerlen=%d", tag, len(final)))
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

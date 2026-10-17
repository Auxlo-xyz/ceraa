// Bridge tests (bridge.go), driven against the fakeTransport script from
// transport_test.go. No real coordinator runs: the RunFunc inlet is a stub
// that records its prompt, so every test stays pure and local.
//
// Note on shapes: bridge.go speaks through the Transport contract
// interface directly, which fakeTransport implements, so the tests below
// drive the real seam with no adapter in between.
package whatsapp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ceraa/harness/coordinator"
)

// errBridgeBoom scripts a failing coordinator run.
var errBridgeBoom = errors.New("boom")

// testBridge wires a Bridge directly to the fake: Bridge.transport is the
// Transport interface itself, which fakeTransport implements, so no
// adapter sits between the test and the seam.
// stubRun records one run-shaped outcome without touching the coordinator.
type stubRun struct {
	mu       sync.Mutex
	calls    int
	prompts  []string
	sessions []string
	final    string
	err      error
	block    chan struct{}
}

func (s *stubRun) run(_ context.Context, rc coordinator.RunnerConfig, prompt string) (coordinator.Result, error) {
	s.mu.Lock()
	s.calls++
	s.prompts = append(s.prompts, prompt)
	s.sessions = append(s.sessions, rc.SessionID)
	s.mu.Unlock()
	if s.block != nil {
		<-s.block
	}
	if s.err != nil {
		return coordinator.Result{}, s.err
	}
	return coordinator.Result{Final: s.final}, nil
}

func (s *stubRun) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func testBridge(filesDir string, run *stubRun) (*Bridge, *fakeTransport) {
	ft := newFakeTransport()
	b := &Bridge{
		transport: ft,
		cfg:       Config{AllowJIDs: []string{"2348012345678@s.whatsapp.net"}},
		run:       run.run,
		filesDir:  filesDir,
		active:    map[string]struct{}{},
	}
	return b, ft
}

func waitForOutbound(t *testing.T, ft *fakeTransport, n int) []outboundText {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := ft.sentTexts(); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d outbound texts, have %d", n, len(ft.sentTexts()))
	return nil
}

func TestBridgeIgnoresOwnMessages(t *testing.T) {
	run := &stubRun{final: "should never send"}
	b, ft := testBridge(t.TempDir(), run)
	b.handle(context.Background(), InboundMessage{
		ChatJID: "2348012345678@s.whatsapp.net",
		ID:      "own-1",
		Text:    "echo",
		FromMe:  true,
	})
	if run.callCount() != 0 {
		t.Fatalf("FromMe message started %d runs, want 0", run.callCount())
	}
	if got := ft.sentTexts(); len(got) != 0 {
		t.Fatalf("FromMe message sent %d texts, want 0", len(got))
	}
}

func TestBridgeSkipsGroups(t *testing.T) {
	run := &stubRun{final: "should never send"}
	b, ft := testBridge(t.TempDir(), run)
	b.handle(context.Background(), InboundMessage{
		ChatJID:  "123-456@g.us",
		GroupJID: "123-456@g.us",
		ID:       "grp-1",
		Text:     "hello group",
	})
	if run.callCount() != 0 {
		t.Fatalf("group message started %d runs, want 0", run.callCount())
	}
	if got := ft.sentTexts(); len(got) != 0 {
		t.Fatalf("group message sent %d texts, want 0", len(got))
	}
}

func TestBridgeSkipsEmptyText(t *testing.T) {
	run := &stubRun{final: "should never send"}
	b, ft := testBridge(t.TempDir(), run)
	for _, text := range []string{"", "   "} {
		b.handle(context.Background(), InboundMessage{
			ChatJID: "2348012345678@s.whatsapp.net",
			ID:      "empty-1",
			Text:    text,
		})
	}
	if run.callCount() != 0 {
		t.Fatalf("empty messages started %d runs, want 0", run.callCount())
	}
	if got := ft.sentTexts(); len(got) != 0 {
		t.Fatalf("empty messages sent %d texts, want 0", len(got))
	}
}

// One DM produces one run-shaped outcome: one run, one reply, addressed
// back to the same chat with the run's final text.
func TestBridgeMapsOneDMToOneRun(t *testing.T) {
	run := &stubRun{final: "the answer"}
	b, ft := testBridge(t.TempDir(), run)
	chat := "2348012345678@s.whatsapp.net"
	b.handle(context.Background(), InboundMessage{
		ChatJID:   chat,
		ID:        "dm-1",
		Text:      "  hello ceraa  ",
		Timestamp: time.Now(),
	})
	if run.callCount() != 1 {
		t.Fatalf("DM started %d runs, want exactly 1", run.callCount())
	}
	run.mu.Lock()
	prompt, session := run.prompts[0], run.sessions[0]
	run.mu.Unlock()
	if prompt != "hello ceraa" {
		t.Errorf("run prompt = %q, want trimmed message text", prompt)
	}
	if session != sessionIDFor(chat) {
		t.Errorf("run session = %q, want %q", session, sessionIDFor(chat))
	}
	sent := ft.sentTexts()
	if len(sent) != 1 {
		t.Fatalf("DM produced %d outbound texts, want exactly 1", len(sent))
	}
	if sent[0].toJID != chat || sent[0].text != "the answer" {
		t.Errorf("outbound = %+v, want the final text back to the same chat", sent[0])
	}
}

func TestBridgeRunErrorSendsFailNote(t *testing.T) {
	run := &stubRun{err: errBridgeBoom}
	b, ft := testBridge(t.TempDir(), run)
	chat := "2348012345678@s.whatsapp.net"
	b.handle(context.Background(), InboundMessage{ChatJID: chat, ID: "dm-err", Text: "go"})
	sent := ft.sentTexts()
	if len(sent) != 1 {
		t.Fatalf("failed run produced %d outbound texts, want the fail note", len(sent))
	}
	if sent[0].toJID != chat || sent[0].text != waFailNote {
		t.Errorf("outbound = %+v, want the fail note", sent[0])
	}
}

func TestBridgeEmptyFinalSendsDoneNote(t *testing.T) {
	run := &stubRun{final: "   "}
	b, ft := testBridge(t.TempDir(), run)
	b.handle(context.Background(), InboundMessage{
		ChatJID: "2348012345678@s.whatsapp.net",
		ID:      "dm-done",
		Text:    "go",
	})
	sent := ft.sentTexts()
	if len(sent) != 1 || sent[0].text != waNoAnswerNote {
		t.Errorf("outbound = %+v, want the done note", sent)
	}
}

func TestBridgeBusyChatGetsBusyReply(t *testing.T) {
	run := &stubRun{final: "late answer"}
	b, ft := testBridge(t.TempDir(), run)
	chat := "2348012345678@s.whatsapp.net"
	b.active[sessionIDFor(chat)] = struct{}{}
	b.handle(context.Background(), InboundMessage{ChatJID: chat, ID: "dm-busy", Text: "again"})
	if run.callCount() != 0 {
		t.Fatalf("busy chat started %d runs, want 0", run.callCount())
	}
	sent := ft.sentTexts()
	if len(sent) != 1 || sent[0].text != waBusyReply {
		t.Errorf("outbound = %+v, want the busy reply", sent)
	}
}

// A media DM carries the saved path into the run prompt, so the run sees
// the file the save notice described.
func TestBridgeMediaDMCarriesSavedPath(t *testing.T) {
	dir := t.TempDir()
	src := writeSizedFile(t, dir, "photo.jpg", 64)
	run := &stubRun{final: "nice shot"}
	mediaDir := t.TempDir()
	b, _ := testBridge(mediaDir, run)
	chat := "2348012345678@s.whatsapp.net"
	b.handle(context.Background(), InboundMessage{
		ChatJID:   chat,
		ID:        "media-1",
		Text:      "look at this",
		MediaPath: src,
	})
	if run.callCount() != 1 {
		t.Fatalf("media DM started %d runs, want 1", run.callCount())
	}
	run.mu.Lock()
	prompt := run.prompts[0]
	run.mu.Unlock()
	if !strings.Contains(prompt, mediaDir) {
		t.Errorf("run prompt = %q, want the saved media path inside", prompt)
	}
	if !strings.Contains(prompt, "look at this") {
		t.Errorf("run prompt = %q, want the caption kept", prompt)
	}
}

func TestBridgeMediaSaveFailureReplies(t *testing.T) {
	run := &stubRun{final: "should never send"}
	b, ft := testBridge(t.TempDir(), run)
	chat := "2348012345678@s.whatsapp.net"
	b.handle(context.Background(), InboundMessage{
		ChatJID:   chat,
		ID:        "media-bad",
		Text:      "broken",
		MediaPath: "/nonexistent-dir-xyz/missing.jpg",
	})
	if run.callCount() != 0 {
		t.Fatalf("unsavable media started %d runs, want 0", run.callCount())
	}
	sent := ft.sentTexts()
	if len(sent) != 1 {
		t.Fatalf("unsavable media produced %d texts, want the save-failure reply", len(sent))
	}
	if sent[0].toJID != chat {
		t.Errorf("reply went to %q, want %q", sent[0].toJID, chat)
	}
}

// The manager inlet end to end through the fake: one scripted DM in,
// one stubbed answer out, then a clean stop. No daemon, no network.
func TestManagerServesOneDMThenStops(t *testing.T) {
	t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
	ft := newFakeTransport()
	run := &stubRun{final: "served"}
	m := NewManager(ft, Config{AllowJIDs: []string{"2348012345678@s.whatsapp.net"}}, run.run, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start = %v", err)
	}
	chat := "2348012345678@s.whatsapp.net"
	if !ft.enqueue(InboundMessage{ChatJID: chat, ID: "m-1", Text: "ping"}) {
		t.Fatal("enqueue refused on the open inlet")
	}
	sent := waitForOutbound(t, ft, 1)
	if sent[0].toJID != chat || sent[0].text != "served" {
		t.Errorf("outbound = %+v, want the served answer", sent[0])
	}
	m.Stop()
}

func TestManagerStartRefusesWhenDisabled(t *testing.T) {
	t.Setenv("CERAA_WHATSAPP_ENABLED", "")
	ft := newFakeTransport()
	m := NewManager(ft, Config{}, (&stubRun{}).run, t.TempDir())
	if err := m.Start(context.Background()); err == nil {
		t.Error("Start with the bridge disabled succeeded, want a refusal")
		m.Stop()
	}
}

func TestManagerStartNeedsRunner(t *testing.T) {
	t.Setenv("CERAA_WHATSAPP_ENABLED", "true")
	ft := newFakeTransport()
	m := NewManager(ft, Config{}, nil, t.TempDir())
	if err := m.Start(context.Background()); err == nil {
		t.Error("Start with no runner succeeded, want a refusal")
		m.Stop()
	}
}

func TestSessionIDForIsStableAndSafe(t *testing.T) {
	cases := []struct {
		chat string
		want string
	}{
		{"2348012345678@s.whatsapp.net", "wa-2348012345678-s.whatsapp.net"},
		{"123-456@g.us", "wa-123-456-g.us"},
		{"a+b=c", "wa-a-b-c"},
	}
	for _, tc := range cases {
		if got := sessionIDFor(tc.chat); got != tc.want {
			t.Errorf("sessionIDFor(%q) = %q, want %q", tc.chat, got, tc.want)
		}
		if again := sessionIDFor(tc.chat); again != tc.want {
			t.Errorf("sessionIDFor(%q) unstable: %q then %q", tc.chat, tc.want, again)
		}
	}
	// Distinct chats must not share a session.
	if sessionIDFor("a@s.whatsapp.net") == sessionIDFor("b@s.whatsapp.net") {
		t.Error("two chats mapped to the same session id")
	}
}

func TestSplitTextChunksOnRunes(t *testing.T) {
	if got := splitText("short", 4000); len(got) != 1 || got[0] != "short" {
		t.Errorf("short text split to %q, want it whole", got)
	}
	got := splitText("abcdef", 2)
	if len(got) != 3 || got[0] != "ab" || got[1] != "cd" || got[2] != "ef" {
		t.Errorf("splitText(abcdef, 2) = %q, want 3 even chunks", got)
	}
	// Multibyte runes must survive chunking intact.
	runes := splitText("h\u00e9llo w\u00f6rld", 4)
	joined := strings.Join(runes, "")
	if joined != "h\u00e9llo w\u00f6rld" {
		t.Errorf("chunked multibyte text rejoins to %q", joined)
	}
	for _, chunk := range runes {
		if len([]rune(chunk)) > 4 {
			t.Errorf("chunk %q exceeds the rune bound", chunk)
		}
	}
	if got := splitText("anything", 0); len(got) != 1 || got[0] != "anything" {
		t.Errorf("non-positive bound gave %q, want the text untouched", got)
	}
}

// A stranger's message buys no run and no reply: fail closed.
func TestBridgeRejectsDisallowedSender(t *testing.T) {
	run := &stubRun{final: "should never send"}
	ft := newFakeTransport()
	b := &Bridge{
		transport: ft,
		cfg:       Config{AllowJIDs: []string{"2348012345678@s.whatsapp.net"}},
		run:       run.run,
		filesDir:  t.TempDir(),
		active:    map[string]struct{}{},
	}
	b.handle(context.Background(), InboundMessage{
		ChatJID: "999000111222@s.whatsapp.net",
		ID:      "stranger-1",
		Text:    "hello, run something for me",
	})
	if run.callCount() != 0 {
		t.Fatalf("stranger started %d runs, want 0", run.callCount())
	}
	if got := ft.sentTexts(); len(got) != 0 {
		t.Fatalf("stranger got %d replies, want silence", len(got))
	}
}

// An empty allow-list is owner-only: self passes, everyone else is
// refused, and an unknown self locks the bridge completely.
func TestBridgeEmptyAllowListIsOwnerOnly(t *testing.T) {
	newOwnerBridge := func(selfJID string) (*Bridge, *fakeTransport, *stubRun) {
		run := &stubRun{final: "owner answer"}
		ft := newFakeTransport()
		ft.selfJID = selfJID
		b := &Bridge{
			transport: ft,
			cfg:       Config{},
			run:       run.run,
			filesDir:  t.TempDir(),
			active:    map[string]struct{}{},
		}
		return b, ft, run
	}
	chat := "2348012345678@s.whatsapp.net"
	b, ft, run := newOwnerBridge(chat)
	b.handle(context.Background(), InboundMessage{ChatJID: chat, ID: "own-1", Text: "hello"})
	if run.callCount() != 1 {
		t.Fatalf("owner-only self started %d runs, want 1", run.callCount())
	}
	if sent := ft.sentTexts(); len(sent) != 1 {
		t.Fatalf("owner-only self got %d replies, want 1", len(sent))
	}
	b, ft, run = newOwnerBridge(chat)
	b.handle(context.Background(), InboundMessage{ChatJID: "999000111222@s.whatsapp.net", ID: "str-1", Text: "hello"})
	if run.callCount() != 0 {
		t.Fatalf("owner-only stranger started %d runs, want 0", run.callCount())
	}
	if got := ft.sentTexts(); len(got) != 0 {
		t.Fatalf("owner-only stranger got %d replies, want silence", len(got))
	}
	b, _, run = newOwnerBridge("")
	b.handle(context.Background(), InboundMessage{ChatJID: chat, ID: "unk-1", Text: "hello"})
	if run.callCount() != 0 {
		t.Fatalf("unknown self started %d runs, want 0 (fail closed)", run.callCount())
	}
}

// A listed number matches on digits across servers, so a server-side
// identity move (@s.whatsapp.net to @lid) cannot lock the owner out.
func TestBridgeAllowsListedDigitsOnAnyServer(t *testing.T) {
	run := &stubRun{final: "welcome back"}
	ft := newFakeTransport()
	b := &Bridge{
		transport: ft,
		cfg:       Config{AllowJIDs: []string{"2348012345678@s.whatsapp.net"}},
		run:       run.run,
		filesDir:  t.TempDir(),
		active:    map[string]struct{}{},
	}
	chat := "2348012345678@lid"
	b.handle(context.Background(), InboundMessage{ChatJID: chat, ID: "lid-1", Text: "ping"})
	if run.callCount() != 1 {
		t.Fatalf("listed digits on another server started %d runs, want 1", run.callCount())
	}
	if sent := ft.sentTexts(); len(sent) != 1 || sent[0].toJID != chat {
		t.Errorf("outbound = %+v, want the reply back to the same chat", sent)
	}
}

// A number the hub stored is admitted with no restart: the bridge reads the
// store live, which is what makes the hub's allow-list button enough on its
// own.
func TestBridgeAllowsAStoredAllowListEntry(t *testing.T) {
	restore := SetAllowListPathForTest(filepath.Join(t.TempDir(), "allowlist.json"))
	defer restore()
	if _, _, err := AddAllowListEntry("2348012345678"); err != nil {
		t.Fatalf("seed allow-list: %v", err)
	}
	run := &stubRun{final: "pong"}
	ft := newFakeTransport()
	b := &Bridge{
		transport: ft,
		cfg:       Config{},
		run:       run.run,
		filesDir:  t.TempDir(),
		active:    map[string]struct{}{},
	}
	b.handle(context.Background(), InboundMessage{
		ChatJID: "2348012345678@s.whatsapp.net",
		ID:      "stored-1",
		Text:    "ping",
	})
	if run.callCount() != 1 {
		t.Fatalf("a stored number started %d runs, want 1", run.callCount())
	}
	if sent := waitForOutbound(t, ft, 1); sent[0].text != "pong" {
		t.Errorf("reply = %q, want pong", sent[0].text)
	}
}

func TestChatTagRedactsTheJID(t *testing.T) {
	chat := "2348012345678@s.whatsapp.net"
	tag := chatTag(chat)
	if tag == "" {
		t.Fatal("chatTag is empty")
	}
	if strings.Contains(tag, "2348012345678") || strings.Contains(tag, chat) {
		t.Errorf("chatTag %q leaks the chat JID", tag)
	}
	if again := chatTag(chat); again != tag {
		t.Error("chatTag is not stable for the same chat")
	}
	if other := chatTag("someone-else@s.whatsapp.net"); other == tag {
		t.Error("two chats share one tag")
	}
	if chatTag("") != "empty" {
		t.Errorf("chatTag(\"\") = %q, want the empty marker", chatTag(""))
	}
}

// A read receipt is sent for an accepted DM, addressed to the JIDs the
// message arrived on rather than the phone form the allow-list is matched
// against. The receipt is what turns the sender's ticks blue.
func TestBridgeMarksAcceptedMessageRead(t *testing.T) {
	run := &stubRun{final: "the answer"}
	b, ft := testBridge(t.TempDir(), run)
	b.handle(context.Background(), InboundMessage{
		ChatJID:       "2348012345678@s.whatsapp.net",
		ID:            "dm-lid-1",
		Text:          "hello ceraa",
		ReceiptChat:   "193471331729523@lid",
		ReceiptSender: "193471331729523@lid",
	})
	reads := ft.readReceipts()
	if len(reads) != 1 {
		t.Fatalf("sent %d read receipts, want exactly 1", len(reads))
	}
	if reads[0].id != "dm-lid-1" {
		t.Errorf("receipt id = %q, want the message id", reads[0].id)
	}
	if reads[0].chat != "193471331729523@lid" || reads[0].sender != "193471331729523@lid" {
		t.Errorf("receipt addressed %q/%q, want the verbatim LID pair", reads[0].chat, reads[0].sender)
	}
}

// A stranger buys no run and no reply, and no read receipt either: the read
// is an acknowledgement, and the bridge does not acknowledge a message it
// refuses.
func TestBridgeDoesNotMarkStrangersRead(t *testing.T) {
	run := &stubRun{final: "should never send"}
	b, ft := testBridge(t.TempDir(), run)
	b.handle(context.Background(), InboundMessage{
		ChatJID:       "2349999999999@s.whatsapp.net",
		ID:            "stranger-1",
		Text:          "hi",
		ReceiptChat:   "2349999999999@s.whatsapp.net",
		ReceiptSender: "2349999999999@s.whatsapp.net",
	})
	if run.callCount() != 0 {
		t.Fatalf("a stranger started %d runs, want 0", run.callCount())
	}
	if got := ft.readReceipts(); len(got) != 0 {
		t.Fatalf("a stranger got %d read receipts, want 0", len(got))
	}
}

// Own messages are never read back.
func TestBridgeDoesNotMarkOwnMessagesRead(t *testing.T) {
	run := &stubRun{final: "should never send"}
	b, ft := testBridge(t.TempDir(), run)
	b.handle(context.Background(), InboundMessage{
		ChatJID: "2348012345678@s.whatsapp.net",
		ID:      "own-2",
		Text:    "echo",
		FromMe:  true,
	})
	if got := ft.readReceipts(); len(got) != 0 {
		t.Fatalf("own message got %d read receipts, want 0", len(got))
	}
}

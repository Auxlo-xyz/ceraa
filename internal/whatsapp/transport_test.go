// Tests for the WhatsApp transport seam (transport.go).
//
// fakeTransport is the scripted stand-in for the real network-backed
// transport: it implements Transport with an explicit script of inbound
// messages and a capture of outbound texts, so bridge tests can drive the
// full receive path without a login.
package whatsapp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Compile-time proof that the fake stays a valid Transport. If the
// Transport interface gains a method, this line fails to build, which is
// the point: the fake must track the seam.
var _ Transport = (*fakeTransport)(nil)

type outboundText struct {
	toJID string
	text  string
}

// readReceipt is one captured MarkRead call: the JIDs the message arrived on
// and its id.
type readReceipt struct {
	chat   string
	sender string
	id     string
}

// chatPresence is one captured ChatPresence call: the chat and whether the
// indicator was turned on (composing) or off (paused).
type chatPresence struct {
	chat      string
	composing bool
}

type fakeTransport struct {
	mu         sync.Mutex
	loggedIn   bool
	selfJID    string
	incoming   chan InboundMessage
	closed     bool
	sent       []outboundText
	reads      []readReceipt
	presence   []chatPresence
	nextID     int
	connectErr error
	sendErr    error
	readErr    error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{incoming: make(chan InboundMessage, 32)}
}

func (f *fakeTransport) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectErr != nil {
		return f.connectErr
	}
	f.loggedIn = true
	return nil
}

func (f *fakeTransport) Disconnect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		close(f.incoming)
		f.closed = true
	}
	f.loggedIn = false
	return nil
}

func (f *fakeTransport) IsLoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loggedIn
}

func (f *fakeTransport) SelfJID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.selfJID
}

func (f *fakeTransport) SendText(ctx context.Context, toJID, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.nextID++
	id := "fake-msg-" + itoa(f.nextID)
	f.sent = append(f.sent, outboundText{toJID: toJID, text: text})
	return id, nil
}

func (f *fakeTransport) MarkRead(ctx context.Context, receiptChat, receiptSender, messageID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return f.readErr
	}
	f.reads = append(f.reads, readReceipt{chat: receiptChat, sender: receiptSender, id: messageID})
	return nil
}

func (f *fakeTransport) ChatPresence(ctx context.Context, chat string, composing bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence = append(f.presence, chatPresence{chat: chat, composing: composing})
	return nil
}

func (f *fakeTransport) Incoming() <-chan InboundMessage {
	return f.incoming
}

// enqueue scripts one inbound message. It reports false instead of sending
// on a closed channel: after Disconnect the producer must drop, never panic.
func (f *fakeTransport) enqueue(m InboundMessage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.incoming <- m
	return true
}

func (f *fakeTransport) sentTexts() []outboundText {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]outboundText, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeTransport) readReceipts() []readReceipt {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]readReceipt, len(f.reads))
	copy(out, f.reads)
	return out
}

func (f *fakeTransport) presenceEvents() []chatPresence {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]chatPresence, len(f.presence))
	copy(out, f.presence)
	return out
}

// recvInboundTimeout reads one message or fails the test instead of hanging
// forever on a broken seam.
func recvInboundTimeout(t *testing.T, ch <-chan InboundMessage) (InboundMessage, bool) {
	t.Helper()
	select {
	case m, ok := <-ch:
		return m, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an inbound message")
		return InboundMessage{}, false
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestFakeTransportConnectMarksLoggedIn(t *testing.T) {
	ft := newFakeTransport()
	if ft.IsLoggedIn() {
		t.Fatal("new fake reports logged in before Connect")
	}
	if err := ft.Connect(context.Background()); err != nil {
		t.Fatalf("Connect = %v, want nil", err)
	}
	if !ft.IsLoggedIn() {
		t.Fatal("fake does not report logged in after Connect")
	}
}

func TestFakeTransportConnectHonoursCancellation(t *testing.T) {
	ft := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ft.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect on canceled context = %v, want context.Canceled", err)
	}
	if ft.IsLoggedIn() {
		t.Fatal("fake reports logged in after a failed Connect")
	}
}

func TestFakeTransportConnectErrorLeavesLoggedOut(t *testing.T) {
	ft := newFakeTransport()
	ft.connectErr = errors.New("dial refused")
	if err := ft.Connect(context.Background()); err == nil {
		t.Fatal("Connect = nil, want the scripted error")
	}
	if ft.IsLoggedIn() {
		t.Fatal("fake reports logged in after a failed Connect")
	}
}

func TestFakeTransportSendTextCapturesOutbound(t *testing.T) {
	ft := newFakeTransport()
	if err := ft.Connect(context.Background()); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	id1, err := ft.SendText(context.Background(), "2348012345678@s.whatsapp.net", "hello")
	if err != nil {
		t.Fatalf("SendText = %v", err)
	}
	id2, err := ft.SendText(context.Background(), "2348012345678@s.whatsapp.net", "world")
	if err != nil {
		t.Fatalf("SendText = %v", err)
	}
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("message ids not distinct and non-empty: %q %q", id1, id2)
	}
	sent := ft.sentTexts()
	if len(sent) != 2 {
		t.Fatalf("captured %d outbound texts, want 2", len(sent))
	}
	if sent[0].toJID != "2348012345678@s.whatsapp.net" || sent[0].text != "hello" {
		t.Errorf("first captured text = %+v, want the hello send", sent[0])
	}
	if sent[1].text != "world" {
		t.Errorf("second captured text = %+v, want the world send", sent[1])
	}
}

func TestFakeTransportSendTextErrorCapturesNothing(t *testing.T) {
	ft := newFakeTransport()
	ft.sendErr = errors.New("send refused")
	if _, err := ft.SendText(context.Background(), "x@s.whatsapp.net", "hi"); err == nil {
		t.Fatal("SendText = nil, want the scripted error")
	}
	if got := ft.sentTexts(); len(got) != 0 {
		t.Fatalf("failed send captured %d texts, want 0", len(got))
	}
}

func TestFakeTransportIncomingDeliversScriptedMessages(t *testing.T) {
	ft := newFakeTransport()
	now := time.Now()
	script := []InboundMessage{
		{ChatJID: "a@s.whatsapp.net", ID: "1", Text: "first", Timestamp: now},
		{ChatJID: "b@s.whatsapp.net", ID: "2", Text: "second", FromMe: true, Timestamp: now},
		{ChatJID: "c@g.us", ID: "3", Text: "third", GroupJID: "c@g.us", Timestamp: now},
	}
	for _, m := range script {
		if !ft.enqueue(m) {
			t.Fatalf("enqueue refused %+v on an open channel", m)
		}
	}
	for i, want := range script {
		got, ok := recvInboundTimeout(t, ft.Incoming())
		if !ok {
			t.Fatalf("message %d: channel closed early", i)
		}
		if got != want {
			t.Errorf("message %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestFakeTransportDisconnectClosesIncoming(t *testing.T) {
	ft := newFakeTransport()
	if !ft.enqueue(InboundMessage{ChatJID: "a@s.whatsapp.net", ID: "1", Text: "last"}) {
		t.Fatal("enqueue refused on an open channel")
	}
	if err := ft.Disconnect(); err != nil {
		t.Fatalf("Disconnect = %v", err)
	}
	if ft.IsLoggedIn() {
		t.Fatal("fake still reports logged in after Disconnect")
	}
	// The buffered message still drains; afterwards the channel stays closed.
	m, ok := recvInboundTimeout(t, ft.Incoming())
	if !ok {
		t.Fatal("buffered message lost across Disconnect")
	}
	if m.ID != "1" {
		t.Errorf("drained message = %+v, want ID 1", m)
	}
	select {
	case _, ok := <-ft.Incoming():
		if ok {
			t.Fatal("Incoming delivered a message after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Incoming not closed after Disconnect")
	}
}

func TestFakeTransportDisconnectIsIdempotent(t *testing.T) {
	ft := newFakeTransport()
	if err := ft.Connect(context.Background()); err != nil {
		t.Fatalf("Connect = %v", err)
	}
	if err := ft.Disconnect(); err != nil {
		t.Fatalf("first Disconnect = %v", err)
	}
	// A second close of the same channel would panic; Disconnect must not.
	if err := ft.Disconnect(); err != nil {
		t.Fatalf("second Disconnect = %v, want nil without panic", err)
	}
}

func TestFakeTransportNoSendOnClosedChannel(t *testing.T) {
	ft := newFakeTransport()
	if err := ft.Disconnect(); err != nil {
		t.Fatalf("Disconnect = %v", err)
	}
	// Must report refusal, never panic on the closed channel.
	if ft.enqueue(InboundMessage{ChatJID: "a@s.whatsapp.net", ID: "x"}) {
		t.Fatal("enqueue on a closed channel reported success")
	}
	select {
	case _, ok := <-ft.Incoming():
		if ok {
			t.Fatal("refused message still arrived on the closed channel")
		}
	default:
		t.Fatal("closed channel should read closed immediately")
	}
}

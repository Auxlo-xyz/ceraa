package whatsapp

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// InboundMessage is one received chat message, in bridge-friendly form.
// MediaPath is reserved for the media download step and stays empty until
// that lands; Text is empty for non-text messages.
type InboundMessage struct {
	ChatJID   string
	ID        string
	Text      string
	MediaPath string
	QuotedID  string
	GroupJID  string
	FromMe    bool
	Timestamp time.Time
	// ReceiptChat and ReceiptSender are the JIDs the message actually
	// arrived on. ChatJID is rewritten to the phone form for the
	// allow-list, and a read receipt is a separate stanza: sending it to a
	// rewritten address is a guess about a namespace the message never
	// used, so the verbatim pair is carried for MarkRead.
	ReceiptChat   string
	ReceiptSender string
}

// Transport is the narrow seam the bridge programs against.
type Transport interface {
	Connect(ctx context.Context) error
	Disconnect() error
	IsLoggedIn() bool
	// SelfJID is the linked account, "" when unknown. The bridge uses
	// it for the owner-only allow-list mode.
	SelfJID() string
	SendText(ctx context.Context, toJID, text string) (string, error)
	// MarkRead sends a read receipt, which is what turns a delivered
	// message blue for its sender. chat and sender are the JIDs the
	// message arrived on; sender is ignored outside group chats.
	MarkRead(ctx context.Context, receiptChat, receiptSender, messageID string) error
	// ChatPresence shows or clears the typing indicator in a chat. WhatsApp
	// has no message for it: composing is the three-dot bubble and paused
	// removes it, and the server expires the state after a few seconds, so a
	// caller that needs it up longer re-sends it.
	ChatPresence(ctx context.Context, chat string, composing bool) error
	Incoming() <-chan InboundMessage
}

// ChatTransport is the Transport implementation. It owns the client, the
// session container and the inbound queue.
type ChatTransport struct {
	cfg       Config
	client    *whatsmeow.Client
	container *sqlstore.Container
	incoming  chan InboundMessage
}

// compileCheck keeps the implementation pinned to the seam.
var _ Transport = (*ChatTransport)(nil)

// NewChatTransport opens the session store and registers the event
// handler. It does not connect; call Connect for that.
func NewChatTransport(ctx context.Context, cfg Config) (*ChatTransport, error) {
	client, container, err := openSession(ctx, cfg.DBPath())
	if err != nil {
		return nil, err
	}
	t := &ChatTransport{
		cfg:       cfg,
		client:    client,
		container: container,
		incoming:  make(chan InboundMessage, 64),
	}
	client.AddEventHandler(t.handleEvent)
	return t, nil
}

// Connect dials the chat server. It refuses when the transport is not
// enabled in the environment.
func (t *ChatTransport) Connect(ctx context.Context) error {
	if !t.cfg.Enabled {
		return errors.New("whatsapp: transport is disabled (CERAA_WHATSAPP_ENABLED is not true)")
	}
	return t.client.ConnectContext(ctx)
}

// Disconnect drops the socket. The session stays on disk, so a later
// Connect resumes without pairing again.
func (t *ChatTransport) Disconnect() error {
	t.client.Disconnect()
	return nil
}

// IsLoggedIn reports a live, authenticated socket. It is false
// before the client connects even when a session is stored, so it is
// never the test for "is this account paired" - use SelfJID for that.
func (t *ChatTransport) IsLoggedIn() bool {
	return t.client.IsLoggedIn()
}

// SelfJID names the linked account, or "" when there is no session yet.
func (t *ChatTransport) SelfJID() string {
	if t.client == nil || t.client.Store == nil || t.client.Store.ID == nil {
		return ""
	}
	return t.client.Store.ID.String()
}

// SendText delivers text to toJID (bare number or full JID) and returns
// the server message ID.
func (t *ChatTransport) SendText(ctx context.Context, toJID, text string) (string, error) {
	to, err := parseRecipient(toJID)
	if err != nil {
		return "", err
	}
	resp, err := t.client.SendMessage(ctx, to, &waE2E.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		return "", err
	}
	return string(resp.ID), nil
}

// MarkRead sends a read receipt for one message. The receipt is addressed to
// the JID the message arrived on, not the rewritten one: WhatsApp addresses
// most DMs by LID, and a receipt is a separate stanza from the reply.
func (t *ChatTransport) MarkRead(ctx context.Context, receiptChat, receiptSender, messageID string) error {
	if messageID == "" {
		return errors.New("whatsapp: mark read without a message id")
	}
	chat, err := parseRecipient(receiptChat)
	if err != nil {
		return err
	}
	var sender types.JID
	if trimmed := strings.TrimSpace(receiptSender); trimmed != "" {
		if sender, err = types.ParseJID(trimmed); err != nil {
			return err
		}
	}
	return t.client.MarkRead(ctx, []types.MessageID{types.MessageID(messageID)}, time.Now(), chat, sender)
}

// ChatPresence shows (composing) or clears (paused) the typing indicator in a
// chat. It addresses the same JID the reply goes to, and like a read receipt it
// is a separate stanza from any message.
func (t *ChatTransport) ChatPresence(ctx context.Context, chat string, composing bool) error {
	jid, err := parseRecipient(chat)
	if err != nil {
		return err
	}
	state := types.ChatPresencePaused
	if composing {
		state = types.ChatPresenceComposing
	}
	return t.client.SendChatPresence(ctx, jid, state, types.ChatPresenceMediaText)
}

// Incoming exposes the receive queue.
func (t *ChatTransport) Incoming() <-chan InboundMessage {
	return t.incoming
}

// PairingClient returns the underlying client so onboarding can run
// QR and pairing-code flows without building a second client.
func (t *ChatTransport) PairingClient() *whatsmeow.Client {
	return t.client
}

// Close releases the session store. Unlinking calls it after Logout and a
// Disconnect so the database file can be removed; the daemon recreates the
// transport on the next link. A no-op the second time.
func (t *ChatTransport) Close() error {
	if t.container == nil {
		return nil
	}
	err := t.container.Close()
	t.container = nil
	return err
}

// handleEvent routes library events onto the inbound queue.
func (t *ChatTransport) handleEvent(evt any) {
	msg, ok := evt.(*events.Message)
	if !ok {
		return
	}
	inbound, ok := mapInbound(msg)
	if !ok {
		return
	}
	inbound.ChatJID = phoneAddressed(inbound.ChatJID, msg.Info.SenderAlt)
	select {
	case t.incoming <- inbound:
	default:
		// Queue full: drop rather than stall the event loop.
	}
}

// phoneAddressed rewrites a LID-addressed chat to its phone-number form.
// WhatsApp addresses most direct messages by LID now, while the allow-list
// holds phone numbers, so a listed owner is refused in silence: the digits
// of a LID and of a number are different namespaces and can never match.
// The library already resolved the alternate address on the stanza, so this
// is a field read rather than a map lookup. An absent alternate keeps the
// original JID, which still fails closed at the allow check.
func phoneAddressed(chat string, alt types.JID) string {
	parsed, err := types.ParseJID(chat)
	if err != nil || parsed.Server != types.HiddenUserServer {
		return chat
	}
	if alt.IsEmpty() || alt.Server != types.DefaultUserServer {
		return chat
	}
	return alt.ToNonAD().String()
}

// mapInbound converts a library message. False means skip: own messages
// and status/broadcast traffic never reach the bridge.
func mapInbound(evt *events.Message) (InboundMessage, bool) {
	if evt == nil || evt.Message == nil {
		return InboundMessage{}, false
	}
	info := evt.Info
	if info.IsFromMe {
		return InboundMessage{}, false
	}
	if info.Chat.Server == types.BroadcastServer {
		return InboundMessage{}, false
	}
	body := evt.Message.GetConversation()
	var quoted string
	if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
		if body == "" {
			body = ext.GetText()
		}
		quoted = ext.GetContextInfo().GetStanzaID()
	}
	out := InboundMessage{
		ChatJID:       info.Chat.String(),
		ID:            string(info.ID),
		Text:          body,
		QuotedID:      quoted,
		FromMe:        info.IsFromMe,
		Timestamp:     info.Timestamp,
		ReceiptChat:   info.Chat.String(),
		ReceiptSender: info.Sender.String(),
	}
	if info.IsGroup {
		out.GroupJID = info.Chat.String()
	}
	return out, true
}

// parseRecipient accepts a full JID or a bare number, which is assumed
// to be a regular chat user.
func parseRecipient(toJID string) (types.JID, error) {
	trimmed := strings.TrimSpace(toJID)
	if trimmed == "" {
		return types.JID{}, errors.New("whatsapp: recipient is empty")
	}
	if strings.Contains(trimmed, "@") {
		return types.ParseJID(trimmed)
	}
	return types.NewJID(trimmed, types.DefaultUserServer), nil
}

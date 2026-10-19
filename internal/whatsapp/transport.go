package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"ceraa/internal/logger"
	"ceraa/internal/paths"
)

// mediaDownloadTimeout bounds one attachment fetch. The dispatch loop
// waits for it, so a CDN that never answers would otherwise hold every
// message behind that attachment, and the sender would see the chat go
// quiet rather than see a failure.
const mediaDownloadTimeout = 90 * time.Second

// InboundMessage is one received chat message, in bridge-friendly form.
// Text carries the body, or the caption of a media message that had one.
type InboundMessage struct {
	ChatJID string
	ID      string
	Text    string
	// MediaPath is a staging file the transport wrote for an attachment.
	// The bridge is expected to consume it and remove it: it is not the
	// saved copy, and nothing else owns it.
	MediaPath string
	// MediaError is set when the message carried an attachment that could
	// not be fetched. It is empty on every message that had none, so an
	// empty MediaPath alone never tells the two apart.
	MediaError string
	QuotedID   string
	GroupJID   string
	FromMe     bool
	Timestamp  time.Time
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

// dispatchItem is one message on its way from the event handler to the
// bridge queue, carrying the attachment to fetch, if it had one.
type dispatchItem struct {
	inbound InboundMessage
	media   mediaAttachment
}

// ChatTransport is the Transport implementation. It owns the client, the
// session container and the inbound queue.
type ChatTransport struct {
	cfg       Config
	client    *whatsmeow.Client
	container *sqlstore.Container
	incoming  chan InboundMessage
	// dispatch is the ordered pipeline between the event handler and the
	// incoming queue. See runDispatch.
	dispatch chan dispatchItem
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
		dispatch:  make(chan dispatchItem, 64),
	}
	client.AddEventHandler(t.handleEvent)
	go t.runDispatch()
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
	item := dispatchItem{inbound: inbound}
	// The fetch is taken only for a message the bridge would otherwise
	// deliver. Group chats are dropped there, so downloading their media
	// would burn bandwidth on a file nobody will ever see; if that gate
	// moves, this one moves with it.
	if att, ok := inboundMedia(msg.Message); ok && !msg.Info.IsGroup {
		item.media = att
	}
	select {
	case t.dispatch <- item:
	default:
		// Queue full: drop rather than stall the event loop.
	}
}

// runDispatch is the single ordering point between the event handler and
// the bridge. It exists for two reasons that point the same way.
//
// A fetch cannot happen inside the handler: whatsmeow delivers events on
// its connection goroutine, so a slow attachment there would stall every
// later event behind one file, including the ones unrelated to it.
//
// And it has to be one loop, not a goroutine per attachment, because
// ordering is the point. A photo and the sentence typed after it are one
// conversation: if the fetch races in the background the sentence is
// answered before the model has seen what it refers to. Routing every
// message, media or not, through here in arrival order keeps that pair
// together, and makes the cost explicit - one stalled download holds the
// queue for at most mediaDownloadTimeout.
func (t *ChatTransport) runDispatch() {
	for item := range t.dispatch {
		if item.media.file != nil {
			path, err := t.stageMedia(item.media)
			if err != nil {
				logger.Warn("whatsapp media download failed: " + err.Error())
				item.inbound.MediaError = err.Error()
			} else {
				item.inbound.MediaPath = path
			}
		}
		select {
		case t.incoming <- item.inbound:
		default:
			// Queue full: drop, matching the handler above.
		}
	}
}

// stageMedia fetches one attachment into the files dir under a staging
// name. The name is the transport's alone: it is replaced by the saved
// copy SaveInboundMedia writes, and the bridge removes it either way.
// Nothing lands in /tmp, which this host wipes.
func (t *ChatTransport) stageMedia(att mediaAttachment) (string, error) {
	if att.length > MaxInboundMediaBytes {
		return "", fmt.Errorf("attachment declares %d bytes, over the %d cap", att.length, MaxInboundMediaBytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mediaDownloadTimeout)
	defer cancel()
	data, err := t.client.Download(ctx, att.file)
	if err != nil {
		return "", err
	}
	if len(data) > MaxInboundMediaBytes {
		return "", fmt.Errorf("attachment is %d bytes, over the %d cap", len(data), MaxInboundMediaBytes)
	}
	dir := paths.FilesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The extension is taken from a mime table this file owns, so it is
	// always a known short suffix or empty and can be trusted in a name.
	f, err := os.CreateTemp(dir, ".stage-*"+att.ext)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// mediaAttachment is the downloadable part of a message, plus what the
// staging step needs to name the file and to refuse an oversized download
// before paying for it.
type mediaAttachment struct {
	file whatsmeow.DownloadableMessage
	// length is the size the sender claims, checked before the fetch so a
	// message declaring a bomb never gets as far as allocating for one.
	length uint64
	ext    string
}

// inboundMedia picks the downloadable part of a message, in the same
// order the library's own DownloadAny uses. It returns false for a
// message with no attachment, which is most of them.
func inboundMedia(m *waE2E.Message) (mediaAttachment, bool) {
	switch {
	case m.GetImageMessage() != nil:
		a := m.GetImageMessage()
		return mediaAttachment{file: a, length: a.GetFileLength(), ext: extForMIME(a.GetMimetype())}, true
	case m.GetVideoMessage() != nil:
		a := m.GetVideoMessage()
		return mediaAttachment{file: a, length: a.GetFileLength(), ext: extForMIME(a.GetMimetype())}, true
	case m.GetAudioMessage() != nil:
		a := m.GetAudioMessage()
		return mediaAttachment{file: a, length: a.GetFileLength(), ext: extForMIME(a.GetMimetype())}, true
	case m.GetDocumentMessage() != nil:
		a := m.GetDocumentMessage()
		return mediaAttachment{file: a, length: a.GetFileLength(), ext: extForMIME(a.GetMimetype())}, true
	case m.GetStickerMessage() != nil:
		a := m.GetStickerMessage()
		return mediaAttachment{file: a, length: a.GetFileLength(), ext: extForMIME(a.GetMimetype())}, true
	}
	return mediaAttachment{}, false
}

// mediaCaption is the text a media message can carry alongside its file.
// Only image, video and document messages have the field, so asking the
// others would return empty anyway. The bridge has no other way to learn
// what the sender wrote with the photo: the body is on the media, not on
// a Conversation.
func mediaCaption(m *waE2E.Message) string {
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption()
	}
	return ""
}

// mimeExtTable maps the mime types WhatsApp actually sends to a short
// suffix, because a saved file with no extension is a file no reader
// wants to open. Anything missing falls back to the empty suffix rather
// than guessing: an invented extension is worse than none.
var mimeExtTable = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"video/mp4":       ".mp4",
	"video/3gpp":      ".3gp",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"application/pdf": ".pdf",
}

// extForMIME looks up a suffix. The input is a wire value, so it is
// matched rather than copied into a filename.
func extForMIME(mime string) string {
	return mimeExtTable[strings.SplitN(strings.TrimSpace(mime), ";", 2)[0]]
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
	// A media message has no Conversation at all: what the sender typed
	// travels as the attachment's caption. Without this the prompt the
	// bridge runs carries only the saved path and none of the sentence
	// that gave it a meaning.
	if body == "" {
		body = mediaCaption(evt.Message)
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
